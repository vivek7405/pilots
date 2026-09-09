package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func (m *Model) View() tea.View {
	var content string
	switch {
	case m.width == 0:
		content = "starting…"
	case m.help:
		content = m.viewHelp()
	default:
		switch m.screen {
		case screenMachine:
			content = m.viewMachine()
		case screenService:
			content = m.viewService()
		case screenLogs:
			content = m.viewLogs()
		default:
			content = m.viewDashboard()
		}
		if m.confirm != nil {
			content = m.overlay(content, m.viewConfirm())
		}
	}
	v := tea.NewView(content)
	v.AltScreen = true
	v.WindowTitle = "pilot"
	return v
}

// chrome is the top line and the bottom line every screen shares: where you
// are, when the fleet was last read, and what the keys do.
func (m *Model) titleBar(where string) string {
	left := m.st.Title.Render("pilot") + "  " + m.st.Muted.Render(where)
	age := "no data yet"
	if !m.snap.At.IsZero() {
		age = "read " + m.snap.At.Format("15:04:05")
	}
	if m.snap.Err != nil {
		age = m.st.Bad.Render("fleet unreachable: " + short(m.snap.Err.Error(), 50))
	}
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(age))
	return left + strings.Repeat(" ", gap) + m.st.Muted.Render(age)
}

func (m *Model) helpLine(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(m.st.Key.Render(pairs[i]) + " " + m.st.Help.Render(pairs[i+1]))
	}
	line := b.String()
	if m.flash != "" && time.Since(m.flashAt) < 4*time.Second {
		flash := m.st.Warn.Render(m.flash)
		gap := max(1, m.width-lipgloss.Width(line)-lipgloss.Width(flash))
		line += strings.Repeat(" ", gap) + flash
	}
	return line
}

func (m *Model) viewDashboard() string {
	title := m.titleBar("dashboard")
	hosts := m.viewHosts()
	tabs := m.viewTabs()
	var body string
	if m.tab == tabMachines {
		body = m.viewMachinesTable()
	} else {
		body = m.viewServicesTable()
	}
	// The table takes what is left, so it never pushes the help line off.
	used := lipgloss.Height(title) + lipgloss.Height(hosts) + lipgloss.Height(tabs) + 2
	body = clampHeight(body, max(3, m.height-used))
	help := m.helpLine("↑↓", "move", "tab", "machines/services", "enter", "open", "c", "console", "L", "logs", "s/w", "suspend/wake", "?", "all keys", "q", "quit")
	return lipgloss.JoinVertical(lipgloss.Left, title, hosts, tabs, body, help)
}

// viewHosts is the cards row: one per host, plus the fleet's own minute of
// history. What the API reports is free CPU and memory and liveness, so
// that is what the cards show.
func (m *Model) viewHosts() string {
	if len(m.snap.Hosts) == 0 {
		return m.st.Panel.Width(m.width - 2).Render(m.st.Muted.Render("no hosts reported yet"))
	}
	var cpuHist, memHist, runHist []float64
	for _, h := range m.history {
		cpuHist = append(cpuHist, float64(h.CPUFree))
		memHist = append(memHist, float64(h.MemFree))
		runHist = append(runHist, float64(h.Running))
	}
	n := len(m.snap.Hosts) + 1
	cardW := max(18, (m.width-n*3)/n)
	var cards []string
	for _, h := range m.snap.Hosts {
		alive := m.st.OK.Render("● alive")
		if !h.Alive {
			alive = m.st.Bad.Render("● down")
		}
		body := fmt.Sprintf("%s  %s\n%s\n%s",
			m.st.Title.Render(h.ID), alive,
			m.st.Muted.Render(fmt.Sprintf("cpu free %d · mem free %s", h.CPUFree, mib(h.MemFreeMiB))),
			m.st.Muted.Render(short(h.CPUVendor, cardW-4)))
		cards = append(cards, m.st.Panel.Width(cardW).Render(body))
	}
	var running int
	for _, mc := range m.snap.Machines {
		if mc.State == "running" {
			running++
		}
	}
	trend := fmt.Sprintf("%s  %s\n%s %s\n%s %s",
		m.st.Title.Render("fleet"), m.st.Muted.Render(fmt.Sprintf("%d running of %d", running, len(m.snap.Machines))),
		m.st.Muted.Render("cpu "), m.st.OK.Render(sparkline(cpuHist, cardW-6)),
		m.st.Muted.Render("run "), m.st.Warn.Render(sparkline(runHist, cardW-6)))
	cards = append(cards, m.st.Panel.Width(cardW).Render(trend))
	_ = memHist
	return lipgloss.JoinHorizontal(lipgloss.Top, cards...)
}

func (m *Model) viewTabs() string {
	names := []string{fmt.Sprintf("machines %d", len(m.snap.Machines)), fmt.Sprintf("services %d", len(m.snap.Services))}
	var parts []string
	for i, n := range names {
		if tab(i) == m.tab {
			parts = append(parts, m.st.Selected.Render(" "+n+" "))
		} else {
			parts = append(parts, m.st.Muted.Render(" "+n+" "))
		}
	}
	return strings.Join(parts, " ")
}

func (m *Model) viewMachinesTable() string {
	if len(m.snap.Machines) == 0 {
		return m.st.Muted.Render("  no machines. `pilot machines create` makes one.")
	}
	nameW, stateW := 4, 9
	for _, mc := range m.snap.Machines {
		nameW = max(nameW, len(mc.Name))
	}
	nameW = min(nameW, 28)
	urlW := max(10, m.width-nameW-stateW-8-2-6)
	header := m.st.Header.Render(fmt.Sprintf("  %-*s  %-*s  %-*s  %s", nameW, "NAME", stateW, "STATE", urlW, "URL", "HOST"))
	rows := []string{header}
	for i, mc := range m.snap.Machines {
		line := fmt.Sprintf("%-*s  %s  %-*s  %s",
			nameW, short(mc.Name, nameW),
			m.st.stateStyle(mc.State).Render(fmt.Sprintf("%-*s", stateW, mc.State)),
			urlW, short(trimHost(mc.URL), urlW), mc.HostID)
		if i == m.cursor {
			line = m.st.Selected.Render("▶ " + line)
		} else {
			line = "  " + line
		}
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n")
}

func (m *Model) viewServicesTable() string {
	if len(m.snap.Services) == 0 {
		return m.st.Muted.Render("  no services. `pilot deploy` makes one from a directory.")
	}
	nameW := 4
	for _, s := range m.snap.Services {
		nameW = max(nameW, len(s.Name))
	}
	nameW = min(nameW, 24)
	urlW := max(10, m.width-nameW-10-2-6-16)
	header := m.st.Header.Render(fmt.Sprintf("  %-*s  %-9s  %-*s  %s", nameW, "NAME", "REPLICAS", urlW, "URL", "RELEASE"))
	rows := []string{header}
	for i, s := range m.snap.Services {
		running, total := replicasOf(m.snap, &s)
		rep := fmt.Sprintf("%d/%d", running, s.Replicas)
		repStyle := m.st.OK
		if running < s.Replicas {
			repStyle = m.st.Warn
		}
		if s.Replicas > 0 && running == 0 {
			repStyle = m.st.Bad
		}
		_ = total
		url := s.CustomDomain
		if url == "" {
			url = trimHost(s.URL)
		}
		if url == "" {
			url = m.st.Muted.Render("(private)")
		}
		line := fmt.Sprintf("%-*s  %s  %-*s  %s", nameW, short(s.Name, nameW), repStyle.Render(fmt.Sprintf("%-9s", rep)), urlW, short(url, urlW), short(s.ReleaseID, 14))
		if i == m.cursor {
			line = m.st.Selected.Render("▶ " + line)
		} else {
			line = "  " + line
		}
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n")
}

func (m *Model) viewMachine() string {
	mc := m.machine
	if mc == nil {
		return m.viewDashboard()
	}
	title := m.titleBar("machine " + mc.Name)
	kv := [][2]string{
		{"state", mc.State}, {"url", mc.URL}, {"id", mc.ID}, {"host", mc.HostID},
		{"size", fmt.Sprintf("%d vCPU, %s", mc.VCPUs, mib(mc.MemMiB))},
		{"created", time.Unix(mc.CreatedAt, 0).Local().Format("2006-01-02 15:04")},
		{"auto stop", mc.Knobs.AutoStop}, {"auto start", strconv.FormatBool(mc.Knobs.AutoStart)},
	}
	if mc.App != "" {
		kv = append(kv, [2]string{"app", mc.App})
	}
	if mc.ServiceID != "" {
		kv = append(kv, [2]string{"service", mc.ServiceID}, [2]string{"release", mc.ReleaseID})
	}
	if mc.VolumeID != "" {
		kv = append(kv, [2]string{"volume", mc.VolumeID})
	}
	var lines []string
	for _, p := range kv {
		v := p[1]
		if p[0] == "state" {
			v = m.st.stateStyle(v).Render(v)
		}
		lines = append(lines, fmt.Sprintf("%s  %s", m.st.Muted.Render(fmt.Sprintf("%-10s", p[0])), v))
	}
	panel := m.st.Focus.Width(m.width - 2).Render(strings.Join(lines, "\n"))
	help := m.helpLine("c", "console", "L", "logs", "s", "suspend", "w", "wake", "S", "stop", "T", "start", "K", "checkpoint", "P", "promote", "D", "destroy", "esc", "back")
	return lipgloss.JoinVertical(lipgloss.Left, title, panel, help)
}

func (m *Model) viewService() string {
	s := m.service
	if s == nil {
		return m.viewDashboard()
	}
	title := m.titleBar("service " + s.Name)
	running, _ := replicasOf(m.snap, s)
	kv := [][2]string{
		{"url", s.URL}, {"id", s.ID}, {"app", s.App},
		{"replicas", fmt.Sprintf("%d running of %d wanted", running, s.Replicas)},
		{"release", s.ReleaseID},
		{"created", time.Unix(s.CreatedAt, 0).Local().Format("2006-01-02 15:04")},
	}
	if s.CustomDomain != "" {
		kv = append(kv, [2]string{"domain", s.CustomDomain})
	}
	if s.Health != nil {
		kv = append(kv, [2]string{"health", s.Health.Type + " " + s.Health.Path})
	}
	if s.Repo != "" {
		kv = append(kv, [2]string{"repo", s.Repo + "@" + s.Branch})
	}
	var lines []string
	for _, p := range kv {
		lines = append(lines, fmt.Sprintf("%s  %s", m.st.Muted.Render(fmt.Sprintf("%-10s", p[0])), p[1]))
	}
	lines = append(lines, "", m.st.Header.Render("REPLICAS"))
	for _, mc := range m.snap.Machines {
		if mc.ServiceID == s.ID {
			lines = append(lines, fmt.Sprintf("  %s  %s  %s", m.st.stateStyle(mc.State).Render(fmt.Sprintf("%-9s", mc.State)), mc.Name, m.st.Muted.Render(mc.ID)))
		}
	}
	panel := m.st.Focus.Width(m.width - 2).Render(strings.Join(lines, "\n"))
	help := m.helpLine("+/-", "replicas", "R", "rollback", "esc", "back")
	return lipgloss.JoinVertical(lipgloss.Left, title, panel, help)
}

func (m *Model) viewLogs() string {
	name := m.logFor
	if m.machine != nil {
		name = m.machine.Name
	}
	follow := "following"
	if !m.logAuto {
		follow = "paused"
	}
	title := m.titleBar(fmt.Sprintf("logs %s · %s", name, follow))
	body := clampHeight(tailText(m.logText, max(3, m.height-4)), max(3, m.height-4))
	help := m.helpLine("f", "follow on/off", "esc", "back")
	return lipgloss.JoinVertical(lipgloss.Left, title, m.st.Panel.Width(m.width-2).Render(body), help)
}

func (m *Model) viewConfirm() string {
	body := m.confirm.Question + "\n\n" + m.st.Key.Render("y") + m.st.Help.Render(" yes    ") + m.st.Key.Render("any other key") + m.st.Help.Render(" no")
	return m.st.Focus.Width(min(m.width-4, 70)).Render(body)
}

func (m *Model) viewHelp() string {
	rows := [][2]string{
		{"↑ ↓ j k", "move"}, {"tab / ← →", "switch machines and services"}, {"enter", "open the selected row"},
		{"c", "open a console on the machine (returns here after)"}, {"L", "logs"},
		{"s / w", "suspend / wake"}, {"S / T", "stop / start"}, {"K", "checkpoint"}, {"P", "promote to a service"},
		{"D", "destroy (asks first)"}, {"+ / -", "scale a service"}, {"R", "roll a service back (asks first)"},
		{"r", "refresh now"}, {"?", "this help"}, {"q / esc", "back, or quit from the dashboard"},
	}
	var lines []string
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("%s  %s", m.st.Key.Render(fmt.Sprintf("%-12s", r[0])), r[1]))
	}
	panel := m.st.Focus.Width(min(m.width-4, 72)).Render(m.st.Title.Render("keys") + "\n\n" + strings.Join(lines, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, panel)
}

// overlay centres a box over a screen. Text underneath is not dimmed: a
// dashboard that goes grey every time it asks a question is a dashboard
// that flickers.
func (m *Model) overlay(under, box string) string {
	placed := lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	underLines := strings.Split(under, "\n")
	boxLines := strings.Split(placed, "\n")
	for i := range boxLines {
		if strings.TrimSpace(boxLines[i]) == "" && i < len(underLines) {
			boxLines[i] = underLines[i]
		}
	}
	return strings.Join(boxLines, "\n")
}

func clampHeight(s string, h int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

func tailText(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func mib(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%.1f GiB", float64(n)/1024)
	}
	return fmt.Sprintf("%d MiB", n)
}
