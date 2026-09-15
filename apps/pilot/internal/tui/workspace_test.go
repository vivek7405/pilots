package tui

import (
	"testing"

	pilots "github.com/vivek7405/pilots/sdks/go"
)

// newTestClient is a client that is never called: these tests drive the model,
// and switchWorkspace builds a NEW client from this one's key rather than
// reaching the network through it.
func newTestClient() *pilots.Client {
	return pilots.New("pilot_test_key", pilots.WithBaseURL("https://api.example.test"))
}

// shift+tab cycles BACKWARDS.
//
// Both branches used to read `(m.tab + 1)`, so shift+tab did exactly what tab
// did. With two tabs the two directions are indistinguishable, which is why it
// survived: the bug would only have shown itself on the day a third tab
// arrived, to somebody who had no reason to suspect the key.
func TestShiftTabGoesBackwards(t *testing.T) {
	m := &Model{}

	if _, _ = m.keyDashboard("tab"); m.tab != tabServices {
		t.Fatalf("tab from machines went to %v, want services", m.tab)
	}
	if _, _ = m.keyDashboard("shift+tab"); m.tab != tabMachines {
		t.Errorf("shift+tab from services went to %v, want machines", m.tab)
	}

	// And it wraps the other way, which is the half a two-tab strip cannot
	// tell apart from going forwards.
	if _, _ = m.keyDashboard("shift+tab"); m.tab != tabServices {
		t.Errorf("shift+tab from the first tab went to %v, want the last", m.tab)
	}
}

// The arithmetic reads tabCount, so adding a tab is one line rather than one
// line plus two literals somebody has to remember to find.
func TestTabCountIsTheNumberOfTabs(t *testing.T) {
	if tabCount != 2 {
		t.Errorf("tabCount = %d but there are two tabs; the cycling arithmetic "+
			"reads this, so a wrong value leaves a tab unreachable", tabCount)
	}
}

// A switch must clear the screen, not replace it row by row. A list that
// changed one row at a time would show two workspaces' machines at once, and
// somebody acting on what they saw would act on the wrong one.
func TestSwitchingWorkspaceClearsWhatWasOnScreen(t *testing.T) {
	m := &Model{
		org:     "team-a",
		baseURL: "https://api.example.test",
		screen:  screenMachine,
		cursor:  7,
		logText: "old output",
		logFor:  "m_1",
	}
	m.client = newTestClient()
	m.snap = snapshot{Err: nil}

	m.switchWorkspace("team-b")

	if m.org != "team-b" {
		t.Errorf("org = %q, want team-b", m.org)
	}
	if m.screen != screenDashboard {
		t.Error("the detail screen survived the switch, so it is showing another " +
			"workspace's machine")
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0: it points at a row that is gone", m.cursor)
	}
	if m.logText != "" || m.logFor != "" {
		t.Error("the old workspace's log survived the switch")
	}
	if len(m.snap.Machines) != 0 {
		t.Error("the old workspace's machines survived the switch")
	}
}

// Switching to the one already showing does nothing, so a stray enter does not
// throw away the screen and re-fetch it.
func TestSwitchingToTheCurrentWorkspaceIsANoOp(t *testing.T) {
	m := &Model{org: "team-a", cursor: 3, screen: screenMachine}
	m.client = newTestClient()
	if cmd := m.switchWorkspace("team-a"); cmd != nil {
		t.Error("switching to the current workspace re-fetched")
	}
	if m.cursor != 3 || m.screen != screenMachine {
		t.Error("switching to the current workspace cleared the screen")
	}
}

// The overlay swallows every key. One falling through would run a machine
// action against a row nobody can see.
func TestThePickerSwallowsEveryKey(t *testing.T) {
	m := &Model{workspaces: &workspaces{Orgs: []string{"a", "b"}, Current: "a"}}
	// A client, because "enter" switches and switching builds a new one from
	// this one's key. The model always has one in production.
	m.client = newTestClient()
	for _, key := range []string{"D", "K", "c", "enter", "x", "1"} {
		if _, handled := m.workspaceKeys(key); !handled {
			t.Errorf("%q fell through the picker", key)
		}
		if m.workspaces == nil {
			// enter and esc close it, which is correct; put it back for the
			// rest of the keys.
			m.workspaces = &workspaces{Orgs: []string{"a", "b"}, Current: "a"}
		}
	}
}

// With no picker open, nothing is consumed, or every key in the program would
// stop working.
func TestWithNoPickerNothingIsConsumed(t *testing.T) {
	m := &Model{}
	if _, handled := m.workspaceKeys("D"); handled {
		t.Error("a key was consumed with no picker open")
	}
}

func TestTheCursorMovesWithinTheList(t *testing.T) {
	m := &Model{workspaces: &workspaces{Orgs: []string{"a", "b", "c"}}}
	m.workspaceKeys("down")
	m.workspaceKeys("down")
	if m.workspaces.Cursor != 2 {
		t.Errorf("cursor = %d, want 2", m.workspaces.Cursor)
	}
	// Clamped at the end rather than wrapping: a list this short is read at a
	// glance, and wrapping past the last item reads as a jump.
	m.workspaceKeys("down")
	if m.workspaces.Cursor != 2 {
		t.Errorf("cursor ran past the end to %d", m.workspaces.Cursor)
	}
	m.workspaceKeys("up")
	m.workspaceKeys("up")
	m.workspaceKeys("up")
	if m.workspaces.Cursor != 0 {
		t.Errorf("cursor ran past the start to %d", m.workspaces.Cursor)
	}
}
