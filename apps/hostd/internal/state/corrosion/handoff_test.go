package corrosion

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// offer inserts a handoff row directly, the way the source host's write would
// arrive through gossip.
func offer(t *testing.T, agent *fakeAgent, id, machineID, from, to string, seq int) {
	t.Helper()
	agent.exec(t, fmt.Sprintf(
		`INSERT INTO machine_handoffs (id, machine_id, from_host, to_host, seq, created_at)
		 VALUES ('%s','%s','%s','%s',%d,1)`, id, machineID, from, to, seq))
}

// The happy path, and the property the whole drain rests on: a machine moves
// between two LIVE hosts, which no other path in the system allows.
//
// Before this existed a machine only ever changed owner when its host was
// provably dead, so a planned reboot was customer-visible.
func TestAMachineMovesOnAValidHandoff(t *testing.T) {
	store, agent := newTestStore(t, "host-b")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-a','suspended')`)
	offer(t, agent, "ho-1", "m-1", "host-a", "host-b", 1)

	if err := store.ClaimMachine(context.Background(), "m-1", "host-b", "suspended",
		state.WithHandoff("ho-1")); err != nil {
		t.Fatalf("a valid handoff was refused: %v", err)
	}
	if got := agent.scalar(t, `SELECT host_id FROM machines WHERE id='m-1'`); got != "host-b" {
		t.Errorf("host_id = %q, want host-b", got)
	}
}

// Every way an offer can be wrong is a refusal, because this is the one path
// on which a running fleet's machine changes owner: a mistake here gives two
// hosts one machine, and two Firecrackers for one id is not a bookkeeping
// error.
func TestEveryBadHandoffIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, agent *fakeAgent)
		claim string
		why   string
	}{
		{
			name:  "no offer at all",
			setup: func(*testing.T, *fakeAgent) {},
			claim: "ho-missing",
			why:   "without a row there is nothing authorising the claim",
		},
		{
			name: "offered to somebody else",
			setup: func(t *testing.T, a *fakeAgent) {
				offer(t, a, "ho-1", "m-1", "host-a", "host-c", 1)
			},
			claim: "ho-1",
			why:   "any host could otherwise take a machine by quoting an id",
		},
		{
			name: "offered by a host that no longer holds it",
			setup: func(t *testing.T, a *fakeAgent) {
				offer(t, a, "ho-1", "m-1", "host-z", "host-b", 1)
			},
			claim: "ho-1",
			why:   "a stale offer would take the machine from whoever holds it now",
		},
		{
			name: "superseded by a newer offer",
			setup: func(t *testing.T, a *fakeAgent) {
				offer(t, a, "ho-1", "m-1", "host-a", "host-b", 1)
				offer(t, a, "ho-2", "m-1", "host-a", "host-c", 2)
			},
			claim: "ho-1",
			why:   "a target the source gave up on must not arrive late and take it",
		},
		{
			name: "naming another machine",
			setup: func(t *testing.T, a *fakeAgent) {
				offer(t, a, "ho-1", "m-other", "host-a", "host-b", 1)
			},
			claim: "ho-1",
			why:   "an offer for one machine must not move another",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, agent := newTestStore(t, "host-b")
			agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-a','suspended')`)
			tc.setup(t, agent)

			err := store.ClaimMachine(context.Background(), "m-1", "host-b", "suspended",
				state.WithHandoff(tc.claim))
			if !errors.Is(err, state.ErrNotOwner) {
				t.Fatalf("err = %v, want ErrNotOwner: %s", err, tc.why)
			}
			if got := agent.scalar(t, `SELECT host_id FROM machines WHERE id='m-1'`); got != "host-a" {
				t.Errorf("the machine moved anyway, to %q: %s", got, tc.why)
			}
		})
	}
}

// A RUNNING machine is never taken, whatever the offer says.
//
// The source suspends before it offers, so a running row means either the
// offer has not been acted on by its own writer yet or the machine came back.
// Taking it would leave two Firecrackers for one id. Gossip can deliver the
// offer before the suspended write that should precede it, and this is what
// absorbs that: the target simply retries.
func TestARunningMachineIsNeverTakenOnAHandoff(t *testing.T) {
	store, agent := newTestStore(t, "host-b")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-a','running')`)
	offer(t, agent, "ho-1", "m-1", "host-a", "host-b", 1)

	err := store.ClaimMachine(context.Background(), "m-1", "host-b", "suspended",
		state.WithHandoff("ho-1"))
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner: a running machine must not be taken", err)
	}
	if got := agent.scalar(t, `SELECT host_id FROM machines WHERE id='m-1'`); got != "host-a" {
		t.Errorf("a running machine moved to %q", got)
	}
}

// One offer, one move. A second claim against a spent offer must fail, or a
// target that retried after a slow success would take a machine back from
// wherever it had since gone.
func TestAHandoffIsSpentOnceItIsUsed(t *testing.T) {
	store, agent := newTestStore(t, "host-b")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-a','suspended')`)
	offer(t, agent, "ho-1", "m-1", "host-a", "host-b", 1)
	ctx := context.Background()

	if err := store.ClaimMachine(ctx, "m-1", "host-b", "suspended", state.WithHandoff("ho-1")); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// The machine has since moved on, as it would after another drain.
	agent.exec(t, `UPDATE machines SET host_id='host-c' WHERE id='m-1'`)

	if err := store.ClaimMachine(ctx, "m-1", "host-b", "suspended",
		state.WithHandoff("ho-1")); !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("err = %v; a spent offer must not move the machine again", err)
	}
	if got := agent.scalar(t, `SELECT host_id FROM machines WHERE id='m-1'`); got != "host-c" {
		t.Errorf("the machine was taken back, to %q", got)
	}
}

// A claim with NEITHER authorisation is refused. Without this the whole
// single-writer rule would be one forgotten option away from gone.
func TestAClaimWithNoAuthorisationIsRefused(t *testing.T) {
	store, agent := newTestStore(t, "host-b")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-a','suspended')`)

	err := store.ClaimMachine(context.Background(), "m-1", "host-b", "suspended")
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner", err)
	}
}

// A host may only offer its OWN machine. Writing an offer for somebody else's
// would be inventing permission for a third host to take it.
func TestAHostCannotOfferAnotherHostsMachine(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, `INSERT INTO machines (id, host_id, state) VALUES ('m-1','host-b','running')`)

	err := store.PutHandoff(context.Background(), &state.Handoff{
		ID: "ho-1", MachineID: "m-1", FromHost: "host-a", ToHost: "host-c", Seq: 1,
	})
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner", err)
	}
	// And it may not forge one in another host's name either.
	err = store.PutHandoff(context.Background(), &state.Handoff{
		ID: "ho-2", MachineID: "m-1", FromHost: "host-b", ToHost: "host-c", Seq: 1,
	})
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner on an offer written in another host's name", err)
	}
}
