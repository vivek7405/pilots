package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A follow keeps the response open and delivers what the guest writes after
// the request started. It ends when the row is gone, which is what a destroy
// leaves behind.
func TestLogsFollowStreamsAndEndsOnDestroy(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, query := range []string{"?follow=1", "?follow"} {
		t.Run(query, func(t *testing.T) {
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

			// The transient failure clears, a guest writes to its console, and
			// then the row is deleted -- which is what a destroy leaves behind
			// and the only thing besides a disconnect that ends a follow.
			go func() {
				// Past the first tick, so the transient failure is one the
				// follow actually saw and continued through.
				time.Sleep(logFollowInterval + 100*time.Millisecond)
				fake.fail(nil)
				fake.appendLog("late line\n")
				time.Sleep(3 * logFollowInterval)
				fake.fail(state.ErrNotFound)
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

// Without follow the body is exactly the backlog and the response ends, which
// is what every non-following reader depends on.
func TestLogsWithoutFollowEnd(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)
	rec := do(t, h, "GET", "/v1/machines/m_1/logs", testKey)
	if rec.Body.String() != "boot log" {
		t.Errorf("body = %q", rec.Body.String())
	}
}
