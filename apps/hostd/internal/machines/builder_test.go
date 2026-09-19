package machines

import (
	"context"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// A running machine is not the same as a daemon accepting connections. Without
// this wait buildctl fails instantly with a refused connection, which reads
// like a networking fault rather than a daemon that has not finished starting.
func TestWaitForBuildkitReturnsOnceSomethingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if err := waitForBuildkitAddr(context.Background(), net.JoinHostPort(host, port), 5*time.Second); err != nil {
		t.Fatalf("a listening port was reported unreachable: %v", err)
	}
}

// And it gives up rather than hanging, so a builder that will never answer
// fails the build inside the build's own timeout.
func TestWaitForBuildkitGivesUp(t *testing.T) {
	// Port 1 on loopback: nothing listens, and a connect fails immediately
	// rather than hanging, so this costs the timeout and no more.
	start := time.Now()
	err := waitForBuildkitAddr(context.Background(), "127.0.0.1:1", 300*time.Millisecond)
	if err == nil {
		t.Fatal("a dead address was reported reachable")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the wait overran its timeout by too much: %v", time.Since(start))
	}
}

// A failed create leaves a builder row in the error state with no image to
// start from. Reusing it failed every later build on "no usable memory build";
// it has to be reported for replacement instead, while a healthy builder is
// still reused and other hosts' rows are never touched.
func TestFindBuilderReplacesAFailedCreate(t *testing.T) {
	m, st := storeManager(t)
	ctx := t.Context()
	const name = "builder-org-host-a"
	for _, row := range []state.Machine{
		{ID: "m_failed", Name: name, HostID: "host-a", State: StateError},
		{ID: "m_elsewhere", Name: name, HostID: "host-b", State: StateError},
		{ID: "m_other_org", Name: "builder-other-host-a", HostID: "host-a", State: StateError},
	} {
		if err := st.PutMachine(ctx, &row); err != nil {
			t.Fatalf("PutMachine %s: %v", row.ID, err)
		}
	}

	id, stale, err := m.findBuilder(ctx, name, "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Errorf("reused %q, a builder that can never start", id)
	}
	if !reflect.DeepEqual(stale, []string{"m_failed"}) {
		t.Errorf("stale = %v, want only this host's failed row for this name", stale)
	}

	healthy := state.Machine{ID: "m_ok", Name: name, HostID: "host-a", State: StateSuspended}
	if err := st.PutMachine(ctx, &healthy); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteMachine(ctx, "m_failed"); err != nil {
		t.Fatal(err)
	}
	id, stale, err = m.findBuilder(ctx, name, "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "m_ok" || len(stale) != 0 {
		t.Errorf("findBuilder = %q, %v; want the suspended builder and nothing to clear", id, stale)
	}
}

// A cancelled context stops the wait at once: a client that hung up should not
// leave hostd polling a guest for a minute and a half.
func TestWaitForBuildkitHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForBuildkitAddr(ctx, "127.0.0.1:1", time.Minute); err == nil {
		t.Fatal("a cancelled wait reported success")
	}
}

// A builder minted from an older builder template is replaced, so a rebuilt
// builder image reaches it; a current one is reused.
func TestFindBuilderReplacesOneFromAnOlderTemplate(t *testing.T) {
	m, st := storeManager(t)
	ctx := t.Context()
	const name = "builder-org-host-a"
	old := state.Machine{ID: "m_old", Name: name, HostID: "host-a",
		State: StateSuspended, TemplateMemBuildID: "tmpl-v1"}
	if err := st.PutMachine(ctx, &old); err != nil {
		t.Fatal(err)
	}

	id, stale, err := m.findBuilder(ctx, name, "tmpl-v2")
	if err != nil {
		t.Fatal(err)
	}
	if id != "" || !reflect.DeepEqual(stale, []string{"m_old"}) {
		t.Errorf("findBuilder = %q, %v; want the older-template builder replaced", id, stale)
	}

	id, stale, err = m.findBuilder(ctx, name, "tmpl-v1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "m_old" || len(stale) != 0 {
		t.Errorf("findBuilder = %q, %v; want a current builder reused", id, stale)
	}
}
