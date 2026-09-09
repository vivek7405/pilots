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
type actionMsg struct {
	what string
	err  error
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
	cursor  int // row in the current tab's list
	snap    snapshot
	history []hostSample

	// Detail screens.
	machine *pilots.Machine
	service *pilots.Service
	logText string
	logFor  string
	logAuto bool

	// Overlays.
	confirm *confirmation
	help    bool
	flash   string
	flashAt time.Time

	exit Exit
	quit bool

	mu sync.Mutex
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
func New(ctx context.Context, client *pilots.Client) *Model {
	return &Model{client: client, ctx: ctx, st: newStyles(newPalette(true)), logAuto: true}
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

func (m *Model) act(what string, fn func(context.Context) error) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg { return actionMsg{what: what, err: fn(ctx)} }
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
		if msg.err != nil {
			m.setFlash(fmt.Sprintf("%s failed: %v", msg.what, msg.err))
		} else {
			m.setFlash(msg.what + " done")
		}
		return m, m.fetch()
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *Model) setFlash(s string) {
	m.flash, m.flashAt = s, time.Now()
}

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
			return m, nil
		}
	}
	if m.help {
		m.help = false
		return m, nil
	}

	switch k {
	case "ctrl+c":
		m.quit = true
		return m, tea.Quit
	case "?":
		m.help = true
		return m, nil
	case "r":
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
		m.tab = (m.tab + 1) % 2
		m.cursor = 0
	case "shift+tab", "left", "h":
		m.tab = (m.tab + 1) % 2
		m.cursor = 0
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < m.rows()-1 {
			m.cursor++
		}
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		m.cursor = max(0, m.rows()-1)
	case "enter":
		if mc := m.selectedMachine(); mc != nil {
			m.machine, m.screen = mc, screenMachine
		} else if s := m.selectedService(); s != nil {
			m.service, m.screen = s, screenService
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
	default:
		return m.serviceAction(k, m.service)
	}
	return m, nil
}

func (m *Model) keyLogs(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "q", "esc", "backspace":
		if m.machine != nil {
			m.screen = screenMachine
		} else {
			m.screen = screenDashboard
		}
		m.logFor = ""
	case "f":
		m.logAuto = !m.logAuto
	}
	return m, nil
}

func (m *Model) openLogs(id string) tea.Cmd {
	m.screen, m.logFor, m.logText = screenLogs, id, "loading…"
	return m.fetchLogs(id)
}

// machineAction maps a key to a lifecycle change. Destroy asks first: it is
// the one action here with nothing to undo.
func (m *Model) machineAction(k string, mc *pilots.Machine) (tea.Model, tea.Cmd) {
	client := m.client
	id, name := mc.ID, mc.Name
	switch k {
	case "s":
		return m, m.act("suspend "+name, func(ctx context.Context) error { return client.Machines.Suspend(ctx, id) })
	case "w":
		return m, m.act("wake "+name, func(ctx context.Context) error { return client.Machines.Wake(ctx, id) })
	case "S":
		return m, m.act("stop "+name, func(ctx context.Context) error { return client.Machines.Stop(ctx, id) })
	case "T":
		return m, m.act("start "+name, func(ctx context.Context) error { return client.Machines.Start(ctx, id) })
	case "K":
		return m, m.act("checkpoint "+name, func(ctx context.Context) error {
			_, err := client.Machines.Checkpoint(ctx, id, "from pilot tui")
			return err
		})
	case "P":
		m.confirm = &confirmation{
			Question: fmt.Sprintf("promote %s to a service? Its URL stays the same.", name),
			Run: func() tea.Cmd {
				return m.act("promote "+name, func(ctx context.Context) error {
					_, err := client.Machines.Promote(ctx, id, pilots.PromoteRequest{Replicas: 1})
					return err
				})
			},
		}
	case "D":
		m.confirm = &confirmation{
			Question: fmt.Sprintf("destroy %s? Its disk, checkpoints and URL are gone for good.", name),
			Run: func() tea.Cmd {
				m.screen, m.machine = screenDashboard, nil
				return m.act("destroy "+name, func(ctx context.Context) error { return client.Machines.Destroy(ctx, id) })
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
			Question: fmt.Sprintf("roll %s back to its previous healthy release? This changes what is serving.", name),
			Run: func() tea.Cmd {
				return m.act("rollback "+name, func(ctx context.Context) error {
					_, err := client.Services.Rollback(ctx, id)
					return err
				})
			},
		}
	case "+", "=":
		n := s.Replicas + 1
		return m, m.act(fmt.Sprintf("scale %s to %d", name, n), func(ctx context.Context) error {
			_, err := client.Services.Patch(ctx, id, pilots.UpdateServiceRequest{Replicas: &n})
			return err
		})
	case "-", "_":
		if s.Replicas == 0 {
			return m, nil
		}
		n := s.Replicas - 1
		return m, m.act(fmt.Sprintf("scale %s to %d", name, n), func(ctx context.Context) error {
			_, err := client.Services.Patch(ctx, id, pilots.UpdateServiceRequest{Replicas: &n})
			return err
		})
	}
	return m, nil
}

// Result is what the caller reads after Run returns.
func (m *Model) Result() Exit { return m.exit }

// Run drives the program in the alternate screen and returns why it ended.
func Run(ctx context.Context, client *pilots.Client) (Exit, error) {
	m := New(ctx, client)
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

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func trimHost(url string) string {
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	return url
}
