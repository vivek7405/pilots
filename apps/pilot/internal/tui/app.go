package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	pilots "github.com/vivek7405/pilots/sdks/go"
)

// Exit is why the program ended. A console hand-off asks the caller to open
// a shell on a machine and then come back: the terminal's raw mode is the
// CLI's job, and doing it in one place keeps a stuck terminal impossible.
type Exit struct {
	ConsoleMachine string
}

type screen int

const (
	screenDashboard screen = iota
	screenMachine
	screenService
	screenLogs
)

type tab int

const (
	tabMachines tab = iota
	tabServices
	// tabCount is not a tab. It is what the cycling arithmetic reads, so
	// adding one above is one line rather than one line plus two literals
	// somebody has to remember to find.
	tabCount
)

// snapshot is one fetch of the fleet, taken every tick on its own goroutine
// so the UI never blocks on the network.
type snapshot struct {
	Hosts    []pilots.Host
	Machines []pilots.Machine
	Services []pilots.Service
	At       time.Time
	Err      error
}

type snapshotMsg snapshot
type tickMsg time.Time
type logsMsg struct {
	id   string
	text string
	err  error
}

// actionMsg is how an action reports back. Detail is what actually happened
// -- the checkpoint's id, the release rolled back to -- because "done" alone
// leaves a person wondering what it did.
type actionMsg struct {
	what   string
	detail string
	err    error
}

const refreshEvery = 2 * time.Second

// Model is the whole TUI. One model rather than one per screen: the screens
// share the snapshot and the palette, and a single Update is easier to read
// than a router forwarding to five.
type Model struct {
	client *pilots.Client
	ctx    context.Context

	st     styles
	width  int
	height int

	screen  screen
	tab     tab
	cursor  int    // row in the current tab's list
	list    window // the list's scroll offset
	detail  window // the machine/service panel's scroll offset
	logWin  window // the log's scroll offset
	snap    snapshot
	history []hostSample

	// lastFrom/lastTo are what the table drew, so the tab line can say
	// "showing 12-31 of 40" without recomputing the window.
	lastFrom, lastTo int
	// detailRows is how many rows the detail panel last drew, so a scroll key
	// clamps against the real length instead of a guess.
	detailRows int

	// Detail screens.
	machine *pilots.Machine
	service *pilots.Service
	logText string
	logFor  string
	logAuto bool

	// The workspace this session is showing, and where to reach the fleet.
	//
	// Held rather than asked for on every frame: the org is applied when the
	// client is built, so the only way to know which one is showing is to
	// remember which one was chosen. The base URL is kept because switching
	// builds a NEW client and has to build it against the same fleet.
	org     string
	baseURL string

	// Overlays and feedback.
	confirm    *confirmation
	workspaces *workspaces
	help       bool
	// busy is an action in flight, shown the moment a key is pressed so a
	// slow one (a checkpoint takes seconds) never looks like a dead keypress.
	busy    string
	flash   string
	flashAt time.Time
	failed  bool

	exit Exit
	quit bool
}

type hostSample struct {
	At      time.Time
	CPUFree int
	MemFree int
	Running int
}

type confirmation struct {
	Question string
	Run      func() tea.Cmd
}

// New builds the model. The first snapshot is fetched on Init so the first
// frame already has content rather than a spinner.
// New builds the model.
//
// org and baseURL are handed in rather than read off the client, because a
// client does not expose either: the org is folded into every request's query
// string when the client is built, and switching workspace has to build a new
// client against the same fleet.
func New(ctx context.Context, client *pilots.Client, org, baseURL string) *Model {
	return &Model{
		ctx: ctx, client: client, org: org, baseURL: baseURL,
		st: newStyles(newPalette(true)), logAuto: true,
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.fetch(), tickAfter())
}

func tickAfter() tea.Cmd {
	return tea.Tick(refreshEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) fetch() tea.Cmd {
	client, ctx := m.client, m.ctx
	return func() tea.Msg {
		var s snapshot
		var wg sync.WaitGroup
		var errs [3]error
		wg.Add(3)
		go func() { defer wg.Done(); s.Hosts, errs[0] = client.Hosts.List(ctx) }()
		go func() { defer wg.Done(); s.Machines, errs[1] = client.Machines.List(ctx) }()
		go func() { defer wg.Done(); s.Services, errs[2] = client.Services.List(ctx) }()
		wg.Wait()
		for _, e := range errs {
			if e != nil {
				s.Err = e
				break
			}
		}
		s.At = time.Now()
		return snapshotMsg(s)
	}
}

func (m *Model) fetchLogs(id string) tea.Cmd {
	client, ctx := m.client, m.ctx
	return func() tea.Msg {
		text, err := client.Machines.Logs(ctx, id)
		return logsMsg{id: id, text: text, err: err}
	}
}

// act runs one action, reporting what happened. The caller says what it is
// in the present tense ("checkpoint web") so the in-flight line reads as
// "checkpointing" and the result as "checkpoint web: ck-1234".
func (m *Model) act(what string, fn func(context.Context) (string, error)) tea.Cmd {
	ctx := m.ctx
	m.busy = what
	m.flash = ""
	return func() tea.Msg {
		detail, err := fn(ctx)
		return actionMsg{what: what, detail: detail, err: err}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.BackgroundColorMsg:
		m.st = newStyles(newPalette(isDark(msg.Color)))
		return m, nil
	case tickMsg:
		cmds := []tea.Cmd{m.fetch(), tickAfter()}
		if m.screen == screenLogs && m.logAuto && m.logFor != "" {
			cmds = append(cmds, m.fetchLogs(m.logFor))
		}
		return m, tea.Batch(cmds...)
	case workspacesMsg:
		// Only while the overlay is still open: a list that arrived after
		// somebody dismissed it would reopen an overlay they closed.
		if m.workspaces != nil {
			got := workspaces(msg)
			m.workspaces = &got
		}
		return m, nil

	case snapshotMsg:
		m.snap = snapshot(msg)
		m.record()
		m.clampCursor()
		m.refreshDetail()
		return m, nil
	case logsMsg:
		if msg.id == m.logFor {
			if msg.err != nil {
				m.logText = "could not read logs: " + msg.err.Error()
			} else {
				m.logText = msg.text
			}
		}
		return m, nil
	case actionMsg:
		m.busy = ""
		m.failed = msg.err != nil
		if msg.err != nil {
			m.setFlash("✗ " + msg.what + ": " + oneLine(msg.err.Error()))
		} else if msg.detail != "" {
			m.setFlash("✓ " + msg.what + ": " + msg.detail)
		} else {
			m.setFlash("✓ " + msg.what)
		}
		return m, m.fetch()
	case tea.MouseWheelMsg:
		return m.wheel(msg)
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	return m, nil
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 120 {
		s = s[:119] + "…"
	}
	return s
}

func (m *Model) setFlash(s string) { m.flash, m.flashAt = s, time.Now() }

// record keeps a minute of host samples for the sparklines.
func (m *Model) record() {
	if m.snap.Err != nil {
		return
	}
	var cpu, mem, running int
	for _, h := range m.snap.Hosts {
		cpu += h.CPUFree
		mem += h.MemFreeMiB
	}
	for _, mc := range m.snap.Machines {
		if mc.State == "running" {
			running++
		}
	}
	m.history = append(m.history, hostSample{At: m.snap.At, CPUFree: cpu, MemFree: mem, Running: running})
	if len(m.history) > 60 {
		m.history = m.history[len(m.history)-60:]
	}
}

func (m *Model) rows() int {
	if m.tab == tabMachines {
		return len(m.snap.Machines)
	}
	return len(m.snap.Services)
}

func (m *Model) clampCursor() {
	if n := m.rows(); m.cursor >= n {
		m.cursor = max(0, n-1)
	}
}

// refreshDetail re-points the detail screens at the fresh snapshot, so a
// state change shows without leaving the screen.
func (m *Model) refreshDetail() {
	if m.machine != nil {
		for i := range m.snap.Machines {
			if m.snap.Machines[i].ID == m.machine.ID {
				m.machine = &m.snap.Machines[i]
			}
		}
	}
	if m.service != nil {
		for i := range m.snap.Services {
			if m.snap.Services[i].ID == m.service.ID {
				m.service = &m.snap.Services[i]
			}
		}
	}
}

func (m *Model) selectedMachine() *pilots.Machine {
	if m.tab != tabMachines || m.cursor >= len(m.snap.Machines) {
		return nil
	}
	return &m.snap.Machines[m.cursor]
}

func (m *Model) selectedService() *pilots.Service {
	if m.tab != tabServices || m.cursor >= len(m.snap.Services) {
		return nil
	}
	return &m.snap.Services[m.cursor]
}

// page is how many rows a PgUp/PgDn moves: the visible body, less a line of
// overlap so you keep your place.
func (m *Model) page() int { return max(1, m.height-8) }

// wheel scrolls whichever screen is showing.
func (m *Model) wheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	up := strings.Contains(msg.String(), "up")
	step := 3
	if !up {
		step = -3
	}
	switch m.screen {
	case screenLogs:
		m.logAuto = false
		m.logWin.scrollBy(-step, len(strings.Split(m.logText, "\n")), max(1, m.height-5))
	case screenMachine, screenService:
		m.detail.scrollBy(-step, m.detailRows, max(1, m.height-4))
	default:
		m.moveCursor(-step)
	}
	return m, nil
}

func (m *Model) moveCursor(delta int) {
	n := m.rows()
	if n == 0 {
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor > n-1 {
		m.cursor = n - 1
	}
}

func (m *Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()

	// Overlays eat every key until dismissed.
	if m.confirm != nil {
		switch k {
		case "y", "Y", "enter":
			run := m.confirm.Run
			m.confirm = nil
			return m, run()
		default:
			m.confirm = nil
			m.setFlash("cancelled")
			return m, nil
		}
	}
	if m.help {
		m.help = false
		return m, nil
	}
	if cmd, handled := m.workspaceKeys(k); handled {
		return m, cmd
	}

	switch k {
	case "ctrl+c":
		m.quit = true
		return m, tea.Quit
	case "?":
		m.help = true
		return m, nil
	case "w":
		// Global, like ? and r: which workspace you are looking at is a
		// question that can occur on any screen, and a binding that only
		// worked on the dashboard would be one somebody learns twice.
		m.workspaces = &workspaces{Current: m.org}
		return m, m.fetchWorkspaces()
	case "r":
		m.setFlash("refreshing…")
		return m, m.fetch()
	}

	switch m.screen {
	case screenDashboard:
		return m.keyDashboard(k)
	case screenMachine:
		return m.keyMachine(k)
	case screenService:
		return m.keyService(k)
	case screenLogs:
		return m.keyLogs(k)
	}
	return m, nil
}

func (m *Model) keyDashboard(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "q", "esc":
		m.quit = true
		return m, tea.Quit
	case "tab", "right", "l":
		m.tab = (m.tab + 1) % tabCount
		m.cursor, m.list.off = 0, 0
	case "shift+tab", "left", "h":
		// BACKWARDS. This read `(m.tab + 1)` too, so shift+tab did exactly
		// what tab did: with two tabs both directions look the same, and the
		// bug would only have shown itself on the day a third arrived.
		m.tab = (m.tab + tabCount - 1) % tabCount
		m.cursor, m.list.off = 0, 0
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "pgup", "ctrl+u":
		m.moveCursor(-m.page())
	case "pgdown", "ctrl+d", " ":
		m.moveCursor(m.page())
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		m.cursor = max(0, m.rows()-1)
	case "enter":
		if mc := m.selectedMachine(); mc != nil {
			m.machine, m.screen, m.detail.off = mc, screenMachine, 0
		} else if s := m.selectedService(); s != nil {
			m.service, m.screen, m.detail.off = s, screenService, 0
		}
	case "L":
		if mc := m.selectedMachine(); mc != nil {
			return m, m.openLogs(mc.ID)
		}
	case "c":
		if mc := m.selectedMachine(); mc != nil {
			m.exit.ConsoleMachine = mc.ID
			return m, tea.Quit
		}
	default:
		if mc := m.selectedMachine(); mc != nil {
			return m.machineAction(k, mc)
		}
		if s := m.selectedService(); s != nil {
			return m.serviceAction(k, s)
		}
	}
	return m, nil
}

func (m *Model) keyMachine(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "q", "esc", "backspace":
		m.screen, m.machine = screenDashboard, nil
	case "up", "k":
		m.detail.scrollBy(-1, m.detailRows, max(1, m.height-4))
	case "down", "j":
		m.detail.scrollBy(1, m.detailRows, max(1, m.height-4))
	case "L", "enter":
		return m, m.openLogs(m.machine.ID)
	case "c":
		m.exit.ConsoleMachine = m.machine.ID
		return m, tea.Quit
	default:
		return m.machineAction(k, m.machine)
	}
	return m, nil
}

func (m *Model) keyService(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "q", "esc", "backspace":
		m.screen, m.service = screenDashboard, nil
	case "up", "k":
		m.detail.scrollBy(-1, m.detailRows, max(1, m.height-4))
	case "down", "j":
		m.detail.scrollBy(1, m.detailRows, max(1, m.height-4))
	default:
		return m.serviceAction(k, m.service)
	}
	return m, nil
}

// keyLogs scrolls the log. Any upward movement stops following, because a
// log that yanks you back to the bottom while you are reading is useless.
func (m *Model) keyLogs(k string) (tea.Model, tea.Cmd) {
	lines := len(strings.Split(strings.TrimRight(m.logText, "\n"), "\n"))
	h := max(1, m.height-5)
	switch k {
	case "q", "esc", "backspace":
		if m.machine != nil {
			m.screen = screenMachine
		} else {
			m.screen = screenDashboard
		}
		m.logFor, m.logAuto = "", true
	case "up", "k":
		m.logAuto = false
		m.logWin.scrollBy(-1, lines, h)
	case "down", "j":
		m.logWin.scrollBy(1, lines, h)
		if m.logWin.atBottom(lines, h) {
			m.logAuto = true
		}
	case "pgup", "ctrl+u":
		m.logAuto = false
		m.logWin.scrollBy(-h, lines, h)
	case "pgdown", "ctrl+d", " ":
		m.logWin.scrollBy(h, lines, h)
		if m.logWin.atBottom(lines, h) {
			m.logAuto = true
		}
	case "g", "home":
		m.logAuto = false
		m.logWin.off = 0
	case "G", "end":
		m.logAuto = true
		m.logWin.off = max(0, lines-h)
	case "f":
		m.logAuto = !m.logAuto
		if m.logAuto {
			m.logWin.off = max(0, lines-h)
		}
	}
	return m, nil
}

func (m *Model) openLogs(id string) tea.Cmd {
	m.screen, m.logFor, m.logText = screenLogs, id, "loading…"
	m.logAuto, m.logWin.off = true, 0
	return m.fetchLogs(id)
}

// machineAction maps a key to a lifecycle change. Every one reports what it
// did; the two with nothing to undo ask first.
func (m *Model) machineAction(k string, mc *pilots.Machine) (tea.Model, tea.Cmd) {
	client := m.client
	id, name, state := mc.ID, mc.Name, mc.State
	// Some actions only make sense in some states, and the fleet answers a
	// doomed one with a bare 404. Saying which state it is in, and which key
	// gets there, beats relaying that.
	refuse := func(msg string) (tea.Model, tea.Cmd) {
		m.failed = false
		m.setFlash(msg)
		return m, nil
	}
	switch k {
	case "s":
		if state == "suspended" {
			return refuse(name + " is already suspended")
		}
		return m, m.act("suspend "+name, func(ctx context.Context) (string, error) {
			return "", client.Machines.Suspend(ctx, id)
		})
	case "w":
		if state == "running" {
			return refuse(name + " is already running")
		}
		return m, m.act("wake "+name, func(ctx context.Context) (string, error) {
			return "", client.Machines.Wake(ctx, id)
		})
	case "S":
		return m, m.act("stop "+name, func(ctx context.Context) (string, error) {
			return "", client.Machines.Stop(ctx, id)
		})
	case "T":
		return m, m.act("start "+name, func(ctx context.Context) (string, error) {
			return "", client.Machines.Start(ctx, id)
		})
	case "K":
		// A checkpoint photographs live memory, so the machine has to be
		// running: hostd answers 404 otherwise, which reads as "no such
		// machine" and sends you looking for the wrong thing.
		if state != "running" {
			return refuse(fmt.Sprintf("%s is %s; a checkpoint needs it running — press w to wake it first", name, state))
		}
		return m, m.act("checkpoint "+name, func(ctx context.Context) (string, error) {
			cp, err := client.Machines.Checkpoint(ctx, id, "from pilot tui")
			if err != nil {
				return "", err
			}
			return cp.ID, nil
		})
	case "P":
		m.confirm = &confirmation{
			Question: fmt.Sprintf("Promote %s to a service?\nIts URL does not change, and every link to it keeps working.", name),
			Run: func() tea.Cmd {
				return m.act("promote "+name, func(ctx context.Context) (string, error) {
					s, err := client.Machines.Promote(ctx, id, pilots.PromoteRequest{Replicas: 1})
					if err != nil {
						return "", err
					}
					return s.Name + " (" + s.ID + ")", nil
				})
			},
		}
	case "D":
		m.confirm = &confirmation{
			Question: fmt.Sprintf("Destroy %s?\nIts disk, its checkpoints and its URL are gone for good.", name),
			Run: func() tea.Cmd {
				m.screen, m.machine = screenDashboard, nil
				return m.act("destroy "+name, func(ctx context.Context) (string, error) {
					return "", client.Machines.Destroy(ctx, id)
				})
			},
		}
	}
	return m, nil
}

func (m *Model) serviceAction(k string, s *pilots.Service) (tea.Model, tea.Cmd) {
	client := m.client
	id, name := s.ID, s.Name
	switch k {
	case "R":
		m.confirm = &confirmation{
			Question: fmt.Sprintf("Roll %s back to its previous healthy release?\nThis changes what is serving.", name),
			Run: func() tea.Cmd {
				return m.act("rollback "+name, func(ctx context.Context) (string, error) {
					r, err := client.Services.Rollback(ctx, id)
					if err != nil {
						return "", err
					}
					return "now on " + r.ID, nil
				})
			},
		}
	case "+", "=":
		n := s.Replicas + 1
		return m, m.act(fmt.Sprintf("scale %s to %d", name, n), func(ctx context.Context) (string, error) {
			_, err := client.Services.Patch(ctx, id, pilots.UpdateServiceRequest{Replicas: &n})
			return "", err
		})
	case "-", "_":
		if s.Replicas == 0 {
			m.setFlash(name + " is already at zero replicas")
			return m, nil
		}
		n := s.Replicas - 1
		return m, m.act(fmt.Sprintf("scale %s to %d", name, n), func(ctx context.Context) (string, error) {
			_, err := client.Services.Patch(ctx, id, pilots.UpdateServiceRequest{Replicas: &n})
			return "", err
		})
	}
	return m, nil
}

// Result is what the caller reads after Run returns.
func (m *Model) Result() Exit { return m.exit }

// Run drives the program in the alternate screen and returns why it ended.
func Run(ctx context.Context, client *pilots.Client, org, baseURL string) (Exit, error) {
	m := New(ctx, client, org, baseURL)
	p := tea.NewProgram(m, tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		return Exit{}, err
	}
	return m.exit, nil
}

// replicasOf counts the machines a service currently runs.
func replicasOf(snap snapshot, s *pilots.Service) (running, total int) {
	for _, mc := range snap.Machines {
		if mc.ServiceID == s.ID {
			total++
			if mc.State == "running" {
				running++
			}
		}
	}
	return
}

func trimHost(url string) string {
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	return url
}

// serviceAddress is the hostname a person would open: the custom domain
// when there is one, else the platform address.
func serviceAddress(s *pilots.Service) string {
	if s.CustomDomain != "" {
		return s.CustomDomain
	}
	return trimHost(s.URL)
}
