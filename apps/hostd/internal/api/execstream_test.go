package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A stream with no argv never reaches the machine. The agent would answer its
// own 400, but only after a wake, and waking a suspended sandbox to tell the
// caller their query was malformed is the wrong order.
func TestExecStreamRequiresCmd(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	rec := do(t, h, "GET", "/v1/machines/m_1/exec/stream", testKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cmd is required") {
		t.Errorf("body = %q", rec.Body.String())
	}
	if got := fake.streamedMachines(); len(got) != 0 {
		t.Errorf("the manager was called anyway: %v", got)
	}
}

func TestExecStreamReachesTheManager(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	if rec := do(t, h, "GET", "/v1/machines/m_1/exec/stream?cmd=ls", testKey); rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if got := fake.streamedMachines(); len(got) != 1 || got[0] != "m_1" {
		t.Errorf("streamed %v, want [m_1]", got)
	}
}

// realID is the shape newID("m") mints: the alias tries a value of this shape
// as an id before scanning names. The rest of this package's fixtures use the
// older m_1 spelling, which is a name as far as the alias is concerned.
const realID = "m-0123456789abcdef01234567"

// seedRealID adds a machine whose id has the minted shape, owned by org_1.
func seedRealID(t *testing.T, st state.Store, name string) {
	t.Helper()
	ctx := context.Background()
	if err := st.PutMachine(ctx, &state.Machine{
		ID: realID, Name: name, HostID: "host-test", State: "running",
		Domain: name + ".pilotrun.app", VCPUs: 1, MemMiB: 512,
	}); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	if err := st.PutTenancy(ctx, &state.Tenancy{ID: realID, OrgID: "org_1", Kind: "machine"}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
}

// The sprites alias is keyed by NAME, because a sprites consumer persists the
// name as the sprite id. An id-shaped value still works, which is what makes
// an id usable everywhere a name is.
func TestSpriteAliasResolvesNameAndID(t *testing.T) {
	for _, seg := range []string{"api", realID} {
		t.Run(seg, func(t *testing.T) {
			h, st, fake := newTestServerWithManager(t)
			seedRealID(t, st, "api")

			rec := do(t, h, "GET", "/v1/sprites/"+seg+"/exec?cmd=ls", testKey)
			if rec.Code != http.StatusOK {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			if got := fake.streamedMachines(); len(got) != 1 || got[0] != realID {
				t.Errorf("streamed %v, want [%s]", got, realID)
			}
		})
	}
}

// A name-first resolution would answer 404 here, because no machine is NAMED
// like this id.
func TestSpriteAliasTriesAnIDShapedValueAsAnID(t *testing.T) {
	h, st, _ := newTestServerWithManager(t)
	seedRealID(t, st, "api")

	d := Deps{HostID: "host-test", Store: st}
	if id, ok := d.machineIDByName(context.Background(), realID); !ok || id != realID {
		t.Errorf("machineIDByName(%s) = %q, %v", realID, id, ok)
	}
	if rec := do(t, h, "GET", "/v1/sprites/"+realID+"/exec?cmd=ls", testKey); rec.Code != http.StatusOK {
		t.Errorf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSpriteAliasIsA404ForAnUnknownName(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)
	rec := do(t, h, "GET", "/v1/sprites/nobody/exec?cmd=ls", testKey)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("body = %q, want the JSON error shape", rec.Body.String())
	}
	if got := fake.streamedMachines(); len(got) != 0 {
		t.Errorf("the manager was called for an unknown name: %v", got)
	}
}

// Existence must not leak across tenants: another org's machine is 404 by name
// and by id, never 403, because a 403 is a machine-name oracle.
func TestSpriteAliasHidesAnotherOrgsMachine(t *testing.T) {
	h, st, fake := twoTenants(t)
	seedRealID(t, st, "api") // org_1's, reachable by name and by id

	for _, path := range []string{
		"/v1/sprites/webapp/exec?cmd=ls",
		"/v1/sprites/api/exec?cmd=ls",
		"/v1/sprites/" + realID + "/exec?cmd=ls",
		"/v1/machines/m_1/exec/stream?cmd=ls",
	} {
		rec := do(t, h, "GET", path, "pilot_org2")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", path, rec.Code)
		}
	}
	if got := fake.streamedMachines(); len(got) != 0 {
		t.Errorf("a foreign caller reached the manager: %v", got)
	}
}

// Two live rows with one name is a bug elsewhere; the lowest id is the stable
// answer to it. A destroyed row is never the answer at all.
func TestMachineIDByNamePrefersTheLowestLiveID(t *testing.T) {
	_, st, _ := newTestServerWithManager(t)
	ctx := context.Background()
	for _, m := range []state.Machine{
		{ID: "m_b", Name: "dup", HostID: "host-test", State: "running"},
		{ID: "m_a", Name: "dup", HostID: "host-test", State: "running"},
		{ID: "m_0", Name: "dup", HostID: "host-test", State: state.StateDestroyed},
	} {
		if err := st.PutMachine(ctx, &m); err != nil {
			t.Fatalf("PutMachine: %v", err)
		}
	}

	d := Deps{HostID: "host-test", Store: st}
	if id, ok := d.machineIDByName(ctx, "dup"); !ok || id != "m_a" {
		t.Errorf("machineIDByName(dup) = %q, %v; want m_a", id, ok)
	}
	if _, ok := d.machineIDByName(ctx, "gone"); ok {
		t.Error("an unknown name resolved")
	}
}

// countingRowStore counts the row reads a follow costs.
type countingRowStore struct {
	state.Store
	mu   sync.Mutex
	gets int
}

func (c *countingRowStore) GetMachine(ctx context.Context, id string) (*state.Machine, error) {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	return c.Store.GetMachine(ctx, id)
}

func (c *countingRowStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets
}

// A follow keeps the response open and delivers what the guest writes after
// the request started. It ends when the row is gone, which is what a destroy
// leaves behind.
//
// The two cadences are the other assertion here. The file is read every tick;
// the ROW is read far more rarely, because on a Corrosion host a row read is
// an HTTP round trip and a per-tick one made every open tail cost 2 queries a
// second for its whole life.
func TestLogsFollowStreamsAndEndsOnDestroy(t *testing.T) {
	for _, query := range []string{"?follow=1", "?follow"} {
		t.Run(query, func(t *testing.T) {
			h, st, fake, counted := newFollowServer(t)
			srv := httptest.NewServer(h)
			defer srv.Close()

			fake.logs = "boot log\n"
			// A read that fails once must not end a tail the caller cannot
			// restart from where it stopped.
			fake.fail(errors.New("transient read failure"))

			req, err := http.NewRequest("GET", srv.URL+"/v1/machines/m_1/logs"+query, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+testKey)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()

			if res.StatusCode != http.StatusOK {
				t.Fatalf("got %d", res.StatusCode)
			}
			if ct := res.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
				t.Errorf("content-type = %q", ct)
			}

			// The row reads the request itself already paid, before a single
			// tick: the tenancy check and the backlog read.
			opened := counted.count()

			// The transient failure clears, a guest writes to its console, and
			// then the row is deleted -- which is what a destroy leaves behind
			// and the only thing besides a disconnect that ends a follow.
			go func() {
				// Past the first tick, so the transient failure is one the
				// follow actually saw and continued through.
				time.Sleep(followPoll + 20*time.Millisecond)
				fake.fail(nil)
				fake.appendLog("late line\n")
				time.Sleep(3 * followPoll)
				ticks := counted.count() - opened
				if err := st.DeleteMachine(context.Background(), "m_1"); err != nil {
					t.Errorf("DeleteMachine: %v", err)
				}
				// Four file ticks have passed and the row was read at most
				// once. Reading it every tick is the cost this split removes.
				if ticks > 1 {
					t.Errorf("the follow read the row %d times across 4 file ticks", ticks)
				}
			}()

			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !strings.HasPrefix(string(body), "boot log\n") {
				t.Errorf("body did not start with the backlog: %q", body)
			}
			if !strings.Contains(string(body), "late line") {
				t.Errorf("the line written after the request started never arrived: %q", body)
			}
		})
	}
}

// The follow's two cadences, shrunk so a test need not wait out a real one.
// The ratio is what production runs: the row is read once per several file
// ticks, never once per tick.
const (
	followPoll = 100 * time.Millisecond
	followRow  = 500 * time.Millisecond
)

// newFollowServer is newTestServerWithManager with a store that counts row
// reads and a follow that runs on the shortened cadences.
func newFollowServer(t *testing.T) (http.Handler, state.Store, *fakeManager, *countingRowStore) {
	t.Helper()
	_, st, fake := newTestServerWithManager(t)
	counted := &countingRowStore{Store: st}
	h := Routes(Deps{
		HostID: "host-test", Store: counted, Machines: fake,
		LogFollowInterval: followPoll, LogRowInterval: followRow,
	})
	return h, st, fake, counted
}

// A PERSISTENT read failure ends the tail and says so.
//
// The retry exists for a failure that clears on the next tick. One that never
// clears -- EIO on the state dir, a permission change -- used to be retried
// for as long as the client held the connection: a warn line twice a second,
// forever, and a reader watching what looked like a machine that had gone
// quiet.
func TestLogsFollowEndsOnAPersistentReadFailure(t *testing.T) {
	h, _, fake, _ := newFollowServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	fake.logs = "boot log\n"
	fake.fail(errors.New("input/output error"))

	req, err := http.NewRequest("GET", srv.URL+"/v1/machines/m_1/logs?follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// Generous next to the budget (logFollowRetries ticks) and far short of
	// forever, which is what this used to be.
	done := make(chan []byte, 1)
	go func() {
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		done <- body
	}()

	select {
	case body := <-done:
		if !strings.HasPrefix(string(body), "boot log\n") {
			t.Errorf("body did not start with the backlog: %q", body)
		}
		if !strings.Contains(string(body), "log follow ended") {
			t.Errorf("the tail ended without telling the reader why: %q", body)
		}
		if !strings.Contains(string(body), "input/output error") {
			t.Errorf("the reason never reached the reader: %q", body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follow never ended on a failure that never clears")
	}
}

// A failure that clears resets the budget, so a tail that saw one hiccup an
// hour ago is not one tick closer to being cut.
func TestLogsFollowRetriesClearAfterASuccessfulRead(t *testing.T) {
	h, _, fake, _ := newFollowServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	fake.logs = "boot log\n"
	req, err := http.NewRequest("GET", srv.URL+"/v1/machines/m_1/logs?follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// Two runs of failures with a good read between them. Summed they are the
	// whole budget; consecutively they are never more than half of it.
	go func() {
		for range 2 {
			fake.fail(errors.New("transient"))
			time.Sleep(time.Duration(logFollowRetries/2) * followPoll)
			fake.fail(nil)
			fake.appendLog("still here\n")
			time.Sleep(2 * followPoll)
		}
		res.Body.Close()
	}()

	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), "log follow ended") {
		t.Errorf("transient failures cut the tail: %q", body)
	}
	if strings.Count(string(body), "still here") != 2 {
		t.Errorf("the tail stopped delivering across the hiccups: %q", body)
	}
}

// Without follow the body is exactly the backlog and the response ends, which
// is what every non-following reader depends on.
func TestLogsWithoutFollowEnd(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)
	rec := do(t, h, "GET", "/v1/machines/m_1/logs", testKey)
	if rec.Body.String() != "boot log" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// countingStore counts the full-table scans a resolution costs.
type countingStore struct {
	state.Store
	lists int
}

func (c *countingStore) ListMachines(ctx context.Context) ([]state.Machine, error) {
	c.lists++
	return c.Store.ListMachines(ctx)
}

// The alias reads the subscription cache before it scans.
//
// ListMachines on a Corrosion host is a full-table query over HTTP, and a
// cross-host alias call pays it on the forwarding host and again on the owner.
// The router's hot path has always tried the in-memory replica first; the
// alias resolves the same names and must be no more expensive.
func TestMachineIDByNameReadsTheCacheBeforeItScans(t *testing.T) {
	_, st, _ := newTestServerWithManager(t)
	ctx := context.Background()
	for _, m := range []state.Machine{
		{ID: "m_x", Name: "api", HostID: "host-test", State: "running"},
		{ID: "m_y", Name: "worker", HostID: "host-test", State: "running"},
	} {
		if err := st.PutMachine(ctx, &m); err != nil {
			t.Fatalf("PutMachine: %v", err)
		}
	}

	counted := &countingStore{Store: st}
	d := Deps{HostID: "host-test", Store: counted, Lookup: func(name string) (state.Machine, bool) {
		if name == "api" {
			return state.Machine{ID: "m_x", Name: "api"}, true
		}
		return state.Machine{}, false // not delivered to this replica yet
	}}

	if id, ok := d.machineIDByName(ctx, "api"); !ok || id != "m_x" {
		t.Errorf("machineIDByName(api) = %q, %v; want m_x", id, ok)
	}
	if counted.lists != 0 {
		t.Errorf("a cache hit still cost %d ListMachines scans, want 0", counted.lists)
	}

	// A miss falls through, so a row the subscription has not delivered still
	// resolves -- the cache is an optimisation, never the authority.
	if id, ok := d.machineIDByName(ctx, "worker"); !ok || id != "m_y" {
		t.Errorf("machineIDByName(worker) = %q, %v; want m_y", id, ok)
	}
	if counted.lists != 1 {
		t.Errorf("a cache miss cost %d scans, want 1", counted.lists)
	}
}
