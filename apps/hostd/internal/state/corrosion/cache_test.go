package corrosion

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"net/http/httptest"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// machineRow renders one machine as the subscription's 22 columns.
func machineRow(id, name, hostID, machineState string) string {
	return fmt.Sprintf(
		`["%s","%s","%s","%s","","",1,512,"%s.pilotrun.app","",8080,3001,"","","","","","","","",0,0]`,
		id, name, hostID, machineState, name)
}

func hostRow(id, addr string, lastSeen int64) string {
	return fmt.Sprintf(`["%s","%s","pk-%s","203.0.113.1",8,4096,%d]`, id, addr, id, lastSeen)
}

// cacheServer serves the two subscriptions a Cache opens.
type cacheServer struct {
	machineRows []string
	hostRows    []string

	machineChanges chan string
	hostChanges    chan string
	subscribes     atomic.Int32
}

func startCache(t *testing.T, s *cacheServer) *Cache {
	t.Helper()

	if s.machineChanges == nil {
		s.machineChanges = make(chan string)
	}
	if s.hostChanges == nil {
		s.hostChanges = make(chan string)
	}

	handler := func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		isHosts := strings.Contains(string(body), "FROM hosts")

		s.subscribes.Add(1)
		w.Header().Set("corro-query-id", "sub")
		if isHosts {
			flushLine(w, `{"columns":["id","wg_addr","wg_pubkey","public_ip","cpu_free","mem_free_mib","last_seen"]}`)
			for _, row := range s.hostRows {
				flushLine(w, `{"row":[1,`+row+`]}`)
			}
		} else {
			flushLine(w, `{"columns":["id","name","host_id","state","kind_knobs","image_ref","vcpus","mem_mib","domain","custom_domain","app_port","agent_port","agent_token_hash","mem_build_id","rootfs_build_id","template_mem_build_id","template_rootfs_build_id","volume_id","service_id","release_id","last_activity","updated_at"]}`)
			for _, row := range s.machineRows {
				flushLine(w, `{"row":[1,`+row+`]}`)
			}
		}
		flushLine(w, `{"eoq":{"time":0,"change_id":1}}`)

		ch := s.machineChanges
		if isHosts {
			ch = s.hostChanges
		}
		for {
			select {
			case line, ok := <-ch:
				if !ok {
					return
				}
				flushLine(w, line)
			case <-r.Context().Done():
				return
			}
		}
	}

	srv := httptest.NewServer(h2c.NewHandler(http.HandlerFunc(handler), &http2.Server{}))
	t.Cleanup(srv.Close)

	client, err := NewClient(strings.TrimPrefix(srv.URL, "http://"), "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cache, err := NewCache(ctx, client)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	return cache
}

// A caller that gets a Cache must get a populated one: a router serving from
// an empty cache 404s every machine on the host until the first rows land.
func TestCacheIsPopulatedBeforeItIsReturned(t *testing.T) {
	cache := startCache(t, &cacheServer{
		machineRows: []string{machineRow("m-1", "alpha", "host-a", "running")},
		hostRows:    []string{hostRow("host-a", "fdcc::1", time.Now().Unix())},
	})

	if _, ok := cache.Machine("m-1"); !ok {
		t.Error("the cache was returned before its initial rows were applied")
	}
	if len(cache.Hosts()) != 1 {
		t.Error("host rows were not applied")
	}
}

func TestCacheAppliesChanges(t *testing.T) {
	changes := make(chan string)
	cache := startCache(t, &cacheServer{
		machineRows:    []string{machineRow("m-1", "alpha", "host-a", "running")},
		hostRows:       []string{hostRow("host-a", "fdcc::1", time.Now().Unix())},
		machineChanges: changes,
	})

	changes <- `{"change":["update",1,` + machineRow("m-1", "alpha", "host-b", "suspended") + `,2]}`

	waitFor(t, func() bool {
		m, ok := cache.Machine("m-1")
		return ok && m.HostID == "host-b" && m.State == "suspended"
	}, "the update to reach the cache")

	changes <- `{"change":["insert",2,` + machineRow("m-2", "beta", "host-a", "running") + `,3]}`
	waitFor(t, func() bool {
		_, ok := cache.Machine("m-2")
		return ok
	}, "the insert to reach the cache")
}

// A destroyed machine is gone as far as every caller is concerned, even while
// its tombstone row is still replicated.
func TestCacheHidesDestroyedMachines(t *testing.T) {
	cache := startCache(t, &cacheServer{
		machineRows: []string{
			machineRow("m-1", "alpha", "host-a", "running"),
			machineRow("m-2", "beta", "host-a", state.StateDestroyed),
		},
		hostRows: []string{hostRow("host-a", "fdcc::1", time.Now().Unix())},
	})

	if _, ok := cache.Machine("m-2"); ok {
		t.Error("a destroyed machine is still readable by id")
	}
	if _, ok := cache.MachineByName("beta"); ok {
		t.Error("a destroyed machine is still routable by name")
	}
	if len(cache.Machines()) != 1 {
		t.Errorf("Machines returned %d, want only the live one", len(cache.Machines()))
	}
}

// Corrosion cannot enforce uniqueness, so two hosts that disagree about who
// owns a name can each create a row with it. Every host must then resolve the
// name the SAME way, or one URL serves two different machines depending on
// which host the client happened to reach.
func TestCacheResolvesADuplicateNameDeterministically(t *testing.T) {
	cache := startCache(t, &cacheServer{
		machineRows: []string{
			machineRow("m-zzz", "alpha", "host-b", "running"),
			machineRow("m-aaa", "alpha", "host-a", "running"),
		},
		hostRows: []string{hostRow("host-a", "fdcc::1", time.Now().Unix())},
	})

	for i := 0; i < 20; i++ {
		m, ok := cache.MachineByName("alpha")
		if !ok {
			t.Fatal("a duplicated name stopped resolving entirely")
		}
		if m.ID != "m-aaa" {
			t.Fatalf("resolved to %s; every host must pick the lowest id", m.ID)
		}
	}
}

// Rank in this list decides which machines a survivor rescues. Two hosts that
// order it differently compute different ranks and then either both rescue the
// same machine or neither rescues it.
func TestLiveHostsAreSortedAndFilteredByHeartbeat(t *testing.T) {
	now := time.Now()
	cache := startCache(t, &cacheServer{
		hostRows: []string{
			hostRow("host-c", "fdcc::3", now.Unix()),
			hostRow("host-a", "fdcc::1", now.Unix()),
			hostRow("host-b", "fdcc::2", now.Unix()),
		},
	})

	// A host that has stopped arriving falls out, and the list stays sorted.
	live := cache.LiveHosts(now.Add(time.Hour), 30*time.Second)
	if len(live) != 0 {
		t.Fatalf("got %d live hosts an hour after the last heartbeat: %+v", len(live), live)
	}

	live = cache.LiveHosts(now, 30*time.Second)
	if len(live) != 3 {
		t.Fatalf("got %d live hosts, want 3: %+v", len(live), live)
	}
	for i, want := range []string{"host-a", "host-b", "host-c"} {
		if live[i].ID != want {
			t.Errorf("live[%d] = %s, want %s", i, live[i].ID, want)
		}
	}
	if cache.IsLive("host-gone", now, 30*time.Second) {
		t.Error("a host that was never in the fleet is reported live")
	}
}

// A host whose clock disagrees with ours is not a dead host.
//
// last_seen is stamped by the peer and compared against our clock, so a box a
// few minutes behind heartbeats steadily and still reads as long dead: peers
// claim its machines while it is serving them, its own release loop stops
// them, and ownership oscillates for as long as the skew lasts. Liveness is
// measured from when the heartbeat ARRIVED here instead, which is one clock
// deciding, and the same clock that measures the interval.
func TestAHostWithASkewedClockIsNotMistakenForDead(t *testing.T) {
	now := time.Now()
	skewed := now.Add(-5 * time.Minute).Unix() // its clock runs five minutes behind

	cache := startCache(t, &cacheServer{
		hostRows: []string{hostRow("host-skewed", "fdcc::7", skewed)},
	})

	if !cache.IsLive("host-skewed", now, 30*time.Second) {
		t.Error("a heartbeating host was called dead because its clock disagrees with ours")
	}
	if live := cache.LiveHosts(now, 30*time.Second); len(live) != 1 {
		t.Errorf("got %d live hosts, want the skewed one counted: %+v", len(live), live)
	}
}

// A subscription the agent has forgotten cannot be resumed, and the changes
// missed in between are gone. Carrying on with a cache that has silently
// stopped updating is the worse failure: the router keeps sending traffic to a
// host that no longer owns the machine.
func TestCacheRebuildsWhenItsSubscriptionIsGone(t *testing.T) {
	var subscribes atomic.Int32
	rows := []string{machineRow("m-1", "alpha", "host-a", "running")}

	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound) // the agent forgot it
			return
		}
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		isHosts := strings.Contains(string(body), "FROM hosts")

		w.Header().Set("corro-query-id", "sub")
		if isHosts {
			flushLine(w, `{"columns":["id","wg_addr","wg_pubkey","public_ip","cpu_free","mem_free_mib","last_seen"]}`)
			flushLine(w, `{"eoq":{"time":0,"change_id":1}}`)
			<-r.Context().Done()
			return
		}

		n := subscribes.Add(1)
		flushLine(w, `{"columns":["id","name","host_id","state","kind_knobs","image_ref","vcpus","mem_mib","domain","custom_domain","app_port","agent_port","agent_token_hash","mem_build_id","rootfs_build_id","template_mem_build_id","template_rootfs_build_id","volume_id","service_id","release_id","last_activity","updated_at"]}`)
		for _, row := range rows {
			flushLine(w, `{"row":[1,`+row+`]}`)
		}
		if n >= 2 {
			// The rebuilt subscription carries the row the cache missed.
			flushLine(w, `{"row":[2,`+machineRow("m-2", "beta", "host-a", "running")+`]}`)
		}
		flushLine(w, `{"eoq":{"time":0,"change_id":1}}`)
		if n >= 2 {
			<-r.Context().Done()
		}
		// The first stream just ends, as it does when the agent restarts.
	}

	srv := httptest.NewServer(h2c.NewHandler(http.HandlerFunc(handler), &http2.Server{}))
	defer srv.Close()

	client, err := NewClient(strings.TrimPrefix(srv.URL, "http://"), "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cache, err := NewCache(ctx, client)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	waitFor(t, func() bool {
		_, ok := cache.Machine("m-2")
		return ok
	}, "the cache to rebuild from a fresh subscription")

	if got := subscribes.Load(); got < 2 {
		t.Errorf("the cache subscribed %d times; it never rebuilt", got)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
