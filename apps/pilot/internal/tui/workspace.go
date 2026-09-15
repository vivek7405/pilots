package tui

import (
	tea "charm.land/bubbletea/v2"
	pilots "github.com/vivek7405/pilots/sdks/go"
)

// Seeing and changing which workspace the screen is showing.
//
// # Why this was worth adding at all
//
// The dashboard has had a workspace switcher since the team work landed. The
// terminal UI had nothing: it inherited whatever `--org` the process started
// with, could not display it, and could not change it. So somebody with two
// teams had a screen full of machines and no way to tell whose, which is the
// one thing a multi-tenant view must never leave ambiguous.
//
// # Why switching rebuilds the client
//
// The org rides on every request as a query parameter, applied by the client
// when it is constructed. Changing it means a new client, which is cheap: it
// holds a key, a base URL and an http.Client, and no connection state worth
// keeping. Mutating the old one in place would be a data race against the fetch
// goroutine that is very likely reading it right now.
//
// # Why it does not persist
//
// Switching here changes THIS session. `pilot orgs use` is what writes a
// workspace to disk, and it says so. A screen that quietly rewrote the
// credentials file because somebody pressed a key to look at something would be
// a surprise with consequences outside the program that caused it.

// workspaces is the picker's state: what a key press is choosing between.
type workspaces struct {
	// Orgs are the choices, and Cursor is which is highlighted.
	Orgs   []string
	Cursor int
	// Current is what the session is acting as now, so the list can mark it.
	Current string
	// Admin says this key may act as a workspace not in the list, which is
	// worth saying rather than leaving somebody to wonder why theirs is
	// missing.
	Admin bool
	// Err is why the list is empty, when it is. An empty picker with no
	// explanation reads as a program that failed silently.
	Err error
}

// workspacesMsg carries the fetched list back to Update.
type workspacesMsg workspaces

// fetchWorkspaces asks which workspaces this key may act as.
//
// On its own goroutine like every other fetch, so opening the picker never
// blocks the UI on a slow fleet.
func (m *Model) fetchWorkspaces() tea.Cmd {
	client, ctx := m.client, m.ctx
	return func() tea.Msg {
		got, err := client.Orgs(ctx)
		if err != nil {
			return workspacesMsg{Err: err}
		}
		out := workspaces{Orgs: got.Orgs, Current: got.Current, Admin: got.Admin}
		// The cursor starts on the current one, because the common reason to
		// open this is to check rather than to change.
		for i, org := range out.Orgs {
			if org == out.Current {
				out.Cursor = i
				break
			}
		}
		return workspacesMsg(out)
	}
}

// switchWorkspace points this session at another workspace.
//
// A NEW client, never a mutation: the org is applied when a client is built,
// and the fetch goroutine is very likely reading the old one right now.
func (m *Model) switchWorkspace(org string) tea.Cmd {
	if org == "" || org == m.org {
		return nil
	}
	m.client = pilots.New(m.client.APIKey(),
		pilots.WithBaseURL(m.baseURL),
		pilots.WithOrg(org))
	m.org = org

	// Everything on screen belongs to the old workspace, so it is cleared
	// rather than left to be replaced row by row: a list that changed one row
	// at a time would show two workspaces' machines at once, and somebody
	// acting on what they saw would act on the wrong one.
	m.snap = snapshot{}
	m.cursor, m.list.off = 0, 0
	m.machine, m.service = nil, nil
	m.logText, m.logFor = "", ""
	m.screen = screenDashboard
	return m.fetch()
}

// workspaceKeys handles the picker's own keys.
//
// Returns true when it consumed the key, so the dashboard's bindings never see
// a keystroke meant for the overlay.
func (m *Model) workspaceKeys(key string) (tea.Cmd, bool) {
	if m.workspaces == nil {
		return nil, false
	}
	switch key {
	case "esc", "q", "w":
		m.workspaces = nil
		return nil, true
	case "up", "k":
		if m.workspaces.Cursor > 0 {
			m.workspaces.Cursor--
		}
		return nil, true
	case "down", "j":
		if m.workspaces.Cursor < len(m.workspaces.Orgs)-1 {
			m.workspaces.Cursor++
		}
		return nil, true
	case "enter":
		picked := ""
		if m.workspaces.Cursor < len(m.workspaces.Orgs) {
			picked = m.workspaces.Orgs[m.workspaces.Cursor]
		}
		m.workspaces = nil
		if picked == "" {
			return nil, true
		}
		m.flash = "showing " + picked
		return m.switchWorkspace(picked), true
	}
	// Every other key is swallowed while the overlay is up. Letting one fall
	// through would run a machine action against a row nobody can see.
	return nil, true
}
