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

// machineRow renders one machine as the subscription's columns.
func machineRow(id, name, hostID, machineState string) string {
	return machineRowInApp(id, name, hostID, machineState, "", 0)
}

// machineRowInApp is the same row with its grouping and netns slot filled in.
func machineRowInApp(id, name, hostID, machineState, app string, slot int) string {
	return fmt.Sprintf(
		`["%s","%s","%s","%s","","",1,512,"%s.pilotrun.app","",8080,3001,"","","","","","","","","%s",%d,0,0]`,
		id, name, hostID, machineState, name, app, slot)
}

func hostRow(id, addr string, lastSeen int64) string {
	return fmt.Sprintf(`["%s","%s","pk-%s","203.0.113.1",8,4096,%d]`, id, addr, id, lastSeen)
}

// cacheServer serves the seven subscriptions a Cache opens.
type cacheServer struct {
	machineRows    []string
	hostRows       []string
	serviceRows    []string
	tenancyRows    []string
	revocationRows []string
	hostCPURows    []string
	machineCPURows []string

	machineChanges    chan string
	hostChanges       chan string
	serviceChanges    chan string
	tenancyChanges    chan string
	revocationChanges chan string
	hostCPUChanges    chan string
	machineCPUChanges chan string
	subscribes        atomic.Int32
}

func startCache(t *testing.T, s *cacheServer) *Cache {
	t.Helper()

	if s.machineChanges == nil {
		s.machineChanges = make(chan string)
	}
	if s.hostChanges == nil {
		s.hostChanges = make(chan string)
	}
	if s.serviceChanges == nil {
		s.serviceChanges = make(chan string)
	}
	if s.tenancyChanges == nil {
		s.tenancyChanges = make(chan string)
	}
	if s.revocationChanges == nil {
		s.revocationChanges = make(chan string)
	}
	if s.hostCPUChanges == nil {
		s.hostCPUChanges = make(chan string)
	}
	if s.machineCPUChanges == nil {
		s.machineCPUChanges = make(chan string)
	}

	handler := func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		isHostCPU := strings.Contains(string(body), "FROM host_cpu")
		isMachineCPU := strings.Contains(string(body), "FROM machine_cpu")
		isHosts := strings.Contains(string(body), "FROM hosts")
		isServices := strings.Contains(string(body), "FROM services")
		isTenancy := strings.Contains(string(body), "FROM tenancy")
		isRevocations := strings.Contains(string(body), "FROM api_key_revocations")

		s.subscribes.Add(1)
		w.Header().Set("corro-query-id", "sub")
		if isHostCPU {
			flushLine(w, `{"columns":["host_id","vendor"]}`)
			for _, row := range s.hostCPURows {
				flushLine(w, `{"row":[1,`+row+`]}`)
			}
		} else if isMachineCPU {
			flushLine(w, `{"columns":["id","kind","vendor","last_start","last_start_at"]}`)
			for _, row := range s.machineCPURows {
				flushLine(w, `{"row":[1,`+row+`]}`)
			}
		} else if isTenancy {
			flushLine(w, `{"columns":["id","org_id","kind"]}`)
			for _, row := range s.tenancyRows {
				flushLine(w, `{"row":[1,`+row+`]}`)
			}
		} else if isRevocations {
			flushLine(w, `{"columns":["hash"]}`)
			for _, row := range s.revocationRows {
				flushLine(w, `{"row":[1,`+row+`]}`)
			}
		} else if isServices {
			flushLine(w, `{"columns":["id","name","app"]}`)
			for _, row := range s.serviceRows {
				flushLine(w, `{"row":[1,`+row+`]}`)
			}
		} else if isHosts {
			flushLine(w, `{"columns":["id","wg_addr","wg_pubkey","public_ip","cpu_free","mem_free_mib","last_seen"]}`)
			for _, row := range s.hostRows {
				flushLine(w, `{"row":[1,`+row+`]}`)
			}
		} else {
			flushLine(w, `{"columns":["id","name","host_id","state","kind_knobs","image_ref","vcpus","mem_mib","domain","custom_domain","app_port","agent_port","agent_token_hash","mem_build_id","rootfs_build_id","template_mem_build_id","template_rootfs_build_id","volume_id","service_id","release_id","app","slot","last_activity","updated_at"]}`)
			for _, row := range s.machineRows {
				flushLine(w, `{"row":[1,`+row+`]}`)
			}
		}
		flushLine(w, `{"eoq":{"time":0,"change_id":1}}`)

		ch := s.machineChanges
		switch {
		case isHostCPU:
			ch = s.hostCPUChanges
		case isMachineCPU:
			ch = s.machineCPUChanges
		case isTenancy:
			ch = s.tenancyChanges
		case isRevocations:
			ch = s.revocationChanges
		case isServices:
			ch = s.serviceChanges
		case isHosts:
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
		if strings.Contains(string(body), "FROM host_cpu") {
			flushLine(w, `{"columns":["host_id","vendor"]}`)
			flushLine(w, `{"eoq":{"time":0,"change_id":1}}`)
			<-r.Context().Done()
			return
		}
		if strings.Contains(string(body), "FROM machine_cpu") {
			flushLine(w, `{"columns":["id","kind","vendor","last_start","last_start_at"]}`)
			flushLine(w, `{"eoq":{"time":0,"change_id":1}}`)
			<-r.Context().Done()
			return
		}
		if isHosts {
			flushLine(w, `{"columns":["id","wg_addr","wg_pubkey","public_ip","cpu_free","mem_free_mib","last_seen"]}`)
			flushLine(w, `{"eoq":{"time":0,"change_id":1}}`)
			<-r.Context().Done()
			return
		}
		if strings.Contains(string(body), "FROM services") {
			flushLine(w, `{"columns":["id","name","app"]}`)
			flushLine(w, `{"eoq":{"time":0,"change_id":1}}`)
			<-r.Context().Done()
			return
		}
		if strings.Contains(string(body), "FROM tenancy") {
			flushLine(w, `{"columns":["id","org_id","kind"]}`)
			flushLine(w, `{"eoq":{"time":0,"change_id":1}}`)
			<-r.Context().Done()
			return
		}
		if strings.Contains(string(body), "FROM api_key_revocations") {
			flushLine(w, `{"columns":["hash"]}`)
			flushLine(w, `{"eoq":{"time":0,"change_id":1}}`)
			<-r.Context().Done()
			return
		}

		n := subscribes.Add(1)
		flushLine(w, `{"columns":["id","name","host_id","state","kind_knobs","image_ref","vcpus","mem_mib","domain","custom_domain","app_port","agent_port","agent_token_hash","mem_build_id","rootfs_build_id","template_mem_build_id","template_rootfs_build_id","volume_id","service_id","release_id","app","slot","last_activity","updated_at"]}`)
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

// A host arriving must be announced, not merely stored.
//
// The mesh reconciles on a 15-second ticker whose comment called itself a
// safety net and said the cache pushed changes as they happened. It did not:
// nothing here signalled anyone, so the ticker WAS the mechanism. A host that
// had just joined could therefore see the whole fleet through Corrosion while
// having a route to none of it, and for those seconds every request it was
// asked to forward on another host's behalf timed out -- which is what the
// phase gate caught, as a freshly bootstrapped third host failing to serve an
// exec for a machine it did not own.
func TestAHostChangeIsAnnounced(t *testing.T) {
	changes := make(chan string)
	cache := startCache(t, &cacheServer{
		machineRows: []string{machineRow("m-1", "alpha", "host-a", "running")},
		hostRows:    []string{hostRow("host-a", "fdcc::1", time.Now().Unix())},
		hostChanges: changes,
	})

	// Drain the signal the initial load may have left.
	select {
	case <-cache.HostsChanged():
	default:
	}

	changes <- `{"change":["insert",2,` + hostRow("host-b", "fdcc::2", time.Now().Unix()) + `,3]}`

	select {
	case <-cache.HostsChanged():
	case <-time.After(5 * time.Second):
		t.Fatal("a host joined and nothing was told about it; the mesh would " +
			"not route to it until its next tick")
	}

	// And the signal does not depend on anyone draining it promptly: a second
	// change while one is pending must not block the cache.
	changes <- `{"change":["insert",3,` + hostRow("host-c", "fdcc::3", time.Now().Unix()) + `,4]}`
	waitFor(t, func() bool {
		for _, h := range cache.Hosts() {
			if h.ID == "host-c" {
				return true
			}
		}
		return false
	}, "the second host to reach the cache")
}

// Org scoping and revocation run on the API's hot path, so both answer from
// the cache. A request path that queries the agent is a request path that can
// block on it.
func TestCacheServesTenancyAndRevocations(t *testing.T) {
	tenancy := make(chan string)
	revocations := make(chan string)
	cache := startCache(t, &cacheServer{
		machineRows:       []string{machineRow("m-1", "alpha", "host-a", "running")},
		hostRows:          []string{hostRow("host-a", "fdcc::1", time.Now().Unix())},
		tenancyRows:       []string{`["m-1","org-1","machine"]`},
		revocationRows:    []string{`["dead"]`},
		tenancyChanges:    tenancy,
		revocationChanges: revocations,
	})

	if org, ok := cache.OrgOf("m-1"); !ok || org != "org-1" {
		t.Errorf("OrgOf(m-1) = %q, %v; want org-1, true", org, ok)
	}
	// An id with no row is not public: it predates tenancy, and the caller
	// treats "no row" as admin-only.
	if _, ok := cache.OrgOf("m-legacy"); ok {
		t.Error("an id with no tenancy row reported an owner")
	}
	if !cache.Revoked("dead") {
		t.Error("a revoked hash from the initial rows is not revoked")
	}
	if cache.Revoked("alive") {
		t.Error("an unrevoked hash reported as revoked")
	}

	tenancy <- `{"change":["insert",1,["m-2","org-2","machine"],2]}`
	waitFor(t, func() bool {
		org, ok := cache.OrgOf("m-2")
		return ok && org == "org-2"
	}, "a tenancy insert to reach the cache")

	revocations <- `{"change":["insert",1,["beef"],2]}`
	waitFor(t, func() bool { return cache.Revoked("beef") },
		"a revocation to reach the cache")
}

// The rescue ranking reads the vendor off the live host list, and the list is
// built from the cache. hosts has no vendor column -- adding one to a table
// with rows is the cr-sqlite backfill hard rule 6 forbids -- so the cache
// stamps it on from its own host_cpu subscription.
func TestTheCacheStampsVendorsOntoLiveHosts(t *testing.T) {
	now := time.Now()
	cache := startCache(t, &cacheServer{
		hostRows: []string{
			hostRow("host-a", "fdcc::1", now.Unix()),
			hostRow("host-b", "fdcc::2", now.Unix()),
		},
		hostCPURows:    []string{`["host-a","AuthenticAMD"]`},
		machineCPURows: []string{`["m-1","machine","GenuineIntel","cold_boot",42]`},
	})

	live := cache.LiveHosts(now, 30*time.Second)
	if len(live) != 2 {
		t.Fatalf("LiveHosts returned %d hosts", len(live))
	}
	if live[0].Vendor != "AuthenticAMD" {
		t.Errorf("host-a's vendor is %q, want AuthenticAMD", live[0].Vendor)
	}
	// A host that has not written its row yet is in no pool, and still live.
	if live[1].Vendor != "" {
		t.Errorf("host-b has no cpu row; its vendor is %q, want empty", live[1].Vendor)
	}

	if got := cache.MachineVendor("m-1"); got != "GenuineIntel" {
		t.Errorf("MachineVendor = %q, want GenuineIntel", got)
	}
	// An unrecorded machine is in no pool, which ranks over the whole fleet.
	if got := cache.MachineVendor("m-nope"); got != "" {
		t.Errorf("an unrecorded machine's vendor is %q, want empty", got)
	}
	row, ok := cache.MachineCPU("m-1")
	if !ok || row.LastStart != "cold_boot" || row.LastStartAt != 42 {
		t.Errorf("MachineCPU = %+v/%v, want the cold_boot row", row, ok)
	}
}
