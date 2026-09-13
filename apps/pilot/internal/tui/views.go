package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func (m *Model) View() tea.View {
	var content string
	switch {
	case m.width == 0 || m.height == 0:
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
		if m.workspaces != nil {
			content = m.overlay(content, m.viewWorkspaces())
		}
		if m.confirm != nil {
			// LAST, so a confirmation is never drawn under another overlay: it
			// is the one that is asking for a decision.
			content = m.overlay(content, m.viewConfirm())
		}
	}
	v := tea.NewView(fitScreen(content, m.width, m.height))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "pilot"
	return v
}

// titleBar is the top line: where you are, and when the fleet was last read.
func (m *Model) titleBar(where string) string {
	left := m.st.Title.Render("pilot") + "  " + m.st.Muted.Render(where)
	// The workspace, whenever there is one to name. On a screen full of
	// machines, whose they are is the one thing a multi-tenant view must never
	// leave ambiguous, and somebody with two teams cannot tell by looking at
	// the rows.
	if m.org != "" {
		left += "  " + m.st.Muted.Render("·") + "  " + m.st.Key.Render(m.org)
	}
	// An action in flight is the most important thing on the line: a
	// checkpoint takes seconds, and without this the keypress looks dead.
	if m.busy != "" {
		return m.spread(left, m.st.Warn.Render("⟳ "+m.busy+"…"))
	}
	right := "no data yet"
	if !m.snap.At.IsZero() {
		right = "read " + m.snap.At.Format("15:04:05")
	}
	if m.snap.Err != nil {
		return m.spread(left, m.st.Bad.Render("fleet unreachable"))
	}
	return m.spread(left, m.st.Muted.Render(right))
}

// spread puts left and right on one line exactly m.width wide, dropping the
// right half rather than wrapping when there is no room for both.
func (m *Model) spread(left, right string) string {
	lw, rw := lipgloss.Width(left), lipgloss.Width(right)
	if lw+rw+1 > m.width {
		return fitLine(left, m.width)
	}
	return left + strings.Repeat(" ", m.width-lw-rw) + right
}

// helpLine is the bottom line: the keys, and the last action's result. Pairs
// are dropped from the right when the terminal is too narrow for all of them,
// so the most important keys survive a small window.
func (m *Model) helpLine(pairs ...string) string {
	// A result stays up for six seconds; a FAILURE stays until the next key,
	// because the one message a person must not miss is the one that says
	// the thing they asked for did not happen.
	flash := ""
	if m.flash != "" && (m.failed || time.Since(m.flashAt) < 6*time.Second) {
		style := m.st.OK
		if m.failed {
			style = m.st.Bad
		} else if !strings.HasPrefix(m.flash, "✓") {
			style = m.st.Warn
		}
		flash = style.Render(m.flash)
	}
	budget := m.width
	if flash != "" {
		budget -= lipgloss.Width(flash) + 2
	}
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		part := m.st.Key.Render(pairs[i]) + " " + m.st.Help.Render(pairs[i+1])
		sep := ""
		if b.Len() > 0 {
			sep = "  "
		}
		if lipgloss.Width(b.String())+lipgloss.Width(sep+part) > budget {
			break
		}
		b.WriteString(sep + part)
	}
	if flash == "" {
		return fitLine(b.String(), m.width)
	}
	return m.spread(b.String(), flash)
}

func (m *Model) viewDashboard() string {
	title := m.titleBar("dashboard")
	hosts := m.viewHosts()
	tabs := m.viewTabs()
	help := m.helpLine("↑↓", "move", "tab", "switch", "enter", "open", "c", "console", "L", "logs", "w", "workspace", "?", "keys", "q", "quit")

	// Whatever is left after the fixed chrome is the list's, and the list
	// scrolls inside exactly that many lines.
	chrome := lipgloss.Height(title) + lipgloss.Height(hosts) + lipgloss.Height(tabs) + lipgloss.Height(help)
	body := m.viewTable(max(1, m.height-chrome))
	return lipgloss.JoinVertical(lipgloss.Left, title, hosts, tabs, body, help)
}

// viewHosts is the cards row. It is dropped entirely on a short terminal:
// the list is what the screen is for.
func (m *Model) viewHosts() string {
	if m.height < 14 {
		return ""
	}
	if len(m.snap.Hosts) == 0 {
		return fitLine(m.st.Muted.Render("  no hosts reported yet"), m.width)
	}
	var cpuHist, runHist []float64
	for _, h := range m.history {
		cpuHist = append(cpuHist, float64(h.CPUFree))
		runHist = append(runHist, float64(h.Running))
	}
	n := len(m.snap.Hosts) + 1
	cardW := (m.width / n) - 3
	if cardW < 16 {
		// Too narrow for a card per host: one summary line instead.
		var running int
		for _, mc := range m.snap.Machines {
			if mc.State == "running" {
				running++
			}
		}
		return fitLine(m.st.Muted.Render(fmt.Sprintf("  %d hosts · %d of %d machines running",
			len(m.snap.Hosts), running, len(m.snap.Machines))), m.width)
	}
	var cards []string
	for _, h := range m.snap.Hosts {
		alive := m.st.OK.Render("● alive")
		if !h.Alive {
			alive = m.st.Bad.Render("● down")
		}
		body := fitBlock(strings.Join([]string{
			m.st.Title.Render(h.ID) + "  " + alive,
			m.st.Muted.Render(fmt.Sprintf("cpu free %d", h.CPUFree)),
			m.st.Muted.Render("mem free " + mib(h.MemFreeMiB)),
		}, "\n"), cardW)
		cards = append(cards, m.st.Panel.Width(cardW).Render(body))
	}
	var running int
	for _, mc := range m.snap.Machines {
		if mc.State == "running" {
			running++
		}
	}
	trend := fitBlock(strings.Join([]string{
		m.st.Title.Render("fleet") + "  " + m.st.Muted.Render(fmt.Sprintf("%d/%d up", running, len(m.snap.Machines))),
		m.st.Muted.Render("cpu ") + m.st.OK.Render(sparkline(cpuHist, cardW-6)),
		m.st.Muted.Render("run ") + m.st.Warn.Render(sparkline(runHist, cardW-6)),
	}, "\n"), cardW)
	cards = append(cards, m.st.Panel.Width(cardW).Render(trend))
	return fitBlock(lipgloss.JoinHorizontal(lipgloss.Top, cards...), m.width)
}

func (m *Model) viewTabs() string {
	names := []string{
		fmt.Sprintf("machines %d", len(m.snap.Machines)),
		fmt.Sprintf("services %d", len(m.snap.Services)),
	}
	var parts []string
	for i, n := range names {
		if tab(i) == m.tab {
			parts = append(parts, m.st.Selected.Render(" "+n+" "))
		} else {
			parts = append(parts, m.st.Muted.Render(" "+n+" "))
		}
	}
	left := strings.Join(parts, " ")
	from, to := m.lastFrom, m.lastTo
	return m.spread(left, m.st.Muted.Render(scrollHint(from, to, m.rows())))
}

// viewTable draws the selected tab's rows into h lines: one header plus the
// slice of rows the window says is visible, with the cursor kept on screen.
func (m *Model) viewTable(h int) string {
	if m.tab == tabMachines {
		return m.viewMachinesTable(h)
	}
	return m.viewServicesTable(h)
}

func (m *Model) viewMachinesTable(h int) string {
	if len(m.snap.Machines) == 0 {
		return m.st.Muted.Render("  no machines. `pilot machines create` makes one.")
	}
	rowsFit := max(1, h-1) // one line for the header
	from, to := m.list.slice(len(m.snap.Machines), rowsFit, m.cursor)
	m.lastFrom, m.lastTo = from, to

	nameW := 4
	for _, mc := range m.snap.Machines {
		nameW = max(nameW, len(mc.Name))
	}
	nameW = min(nameW, 26)
	const stateW = 9
	hostW := 6
	for _, mc := range m.snap.Machines {
		hostW = max(hostW, len(mc.HostID))
	}
	hostW = min(hostW, 12)
	// Two for the cursor, two spaces between each of the four columns.
	urlW := m.width - 2 - nameW - stateW - hostW - 6
	if urlW < 8 {
		urlW = 8
	}

	lines := []string{m.st.Header.Render(fitLine(fmt.Sprintf("  %-*s  %-*s  %-*s  %s",
		nameW, "NAME", stateW, "STATE", hostW, "HOST", "URL"), m.width))}
	for i := from; i < to; i++ {
		mc := &m.snap.Machines[i]
		line := fmt.Sprintf("%-*s  %s  %-*s  %-*s",
			nameW, trunc(mc.Name, nameW),
			m.st.stateStyle(mc.State).Render(fmt.Sprintf("%-*s", stateW, mc.State)),
			hostW, trunc(mc.HostID, hostW),
			urlW, trunc(trimHost(mc.URL), urlW))
		if i == m.cursor {
			lines = append(lines, m.st.Selected.Render(fitLine("▶ "+line, m.width)))
		} else {
			lines = append(lines, fitLine("  "+line, m.width))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) viewServicesTable(h int) string {
	if len(m.snap.Services) == 0 {
		return m.st.Muted.Render("  no services. `pilot deploy` makes one from a directory.")
	}
	rowsFit := max(1, h-1)
	from, to := m.list.slice(len(m.snap.Services), rowsFit, m.cursor)
	m.lastFrom, m.lastTo = from, to

	nameW := 4
	for _, s := range m.snap.Services {
		nameW = max(nameW, len(s.Name))
	}
	nameW = min(nameW, 24)
	const repW = 8
	urlW := m.width - 2 - nameW - repW - 4
	if urlW < 8 {
		urlW = 8
	}

	lines := []string{m.st.Header.Render(fitLine(fmt.Sprintf("  %-*s  %-*s  %s",
		nameW, "NAME", repW, "REPLICAS", "URL"), m.width))}
	for i := from; i < to; i++ {
		s := &m.snap.Services[i]
		running, _ := replicasOf(m.snap, s)
		rep := fmt.Sprintf("%d/%d", running, s.Replicas)
		style := m.st.OK
		if running < s.Replicas {
			style = m.st.Warn
		}
		if s.Replicas > 0 && running == 0 {
			style = m.st.Bad
		}
		url := s.CustomDomain
		if url == "" {
			url = trimHost(s.URL)
		}
		if url == "" {
			url = "(private)"
		}
		line := fmt.Sprintf("%-*s  %s  %-*s",
			nameW, trunc(s.Name, nameW),
			style.Render(fmt.Sprintf("%-*s", repW, rep)),
			urlW, trunc(url, urlW))
		if i == m.cursor {
			lines = append(lines, m.st.Selected.Render(fitLine("▶ "+line, m.width)))
		} else {
			lines = append(lines, fitLine("  "+line, m.width))
		}
	}
	return strings.Join(lines, "\n")
}

// kvPanel renders label/value rows inside the panel border, scrolled by the
// detail window so a machine with more facts than the terminal has lines is
// still readable.
func (m *Model) kvPanel(rows [][2]string, h int) string {
	inner := max(1, h-2) // the panel's own border
	// The detail screens have no cursor, so the window is the scroll keys'
	// offset, clamped -- NOT window.slice, which takes a cursor and would
	// reset the offset to 0 as a side effect on every render, leaving j/k and
	// the wheel with nothing to show for themselves.
	m.detailRows = len(rows)
	from := m.detail.off
	if from > len(rows)-inner {
		from = len(rows) - inner
	}
	if from < 0 {
		from = 0
	}
	m.detail.off = from
	to := min(len(rows), from+inner)
	var lines []string
	for _, r := range rows[from:to] {
		lines = append(lines, fmt.Sprintf("%s  %s", m.st.Muted.Render(fmt.Sprintf("%-12s", r[0])), r[1]))
	}
	body := fitBlock(strings.Join(lines, "\n"), max(1, m.width-4))
	return m.st.Focus.Width(m.width - 2).Render(body)
}

func (m *Model) viewMachine() string {
	mc := m.machine
	if mc == nil {
		return m.viewDashboard()
	}
	title := m.titleBar("machine " + mc.Name)
	urlAuth := mc.URLAuth
	if urlAuth == "" {
		urlAuth = "public"
	}
	rows := [][2]string{
		{"state", m.st.stateStyle(mc.State).Render(mc.State)},
		{"url", mc.URL},
		{"url auth", urlAuth},
		{"id", mc.ID},
		{"host", mc.HostID},
		{"size", fmt.Sprintf("%d vCPU, %s", mc.VCPUs, mib(mc.MemMiB))},
		{"created", time.Unix(mc.CreatedAt, 0).Local().Format("2006-01-02 15:04")},
		{"auto stop", mc.Knobs.AutoStop},
		{"auto start", strconv.FormatBool(mc.Knobs.AutoStart)},
		{"idle timeout", (time.Duration(mc.Knobs.IdleTimeout) * time.Second).String()},
	}
	for _, s := range mc.Knobs.Schedules {
		target := s.Cmd
		if s.Path != "" {
			target = "GET " + s.Path
		}
		rows = append(rows, [2]string{"schedule", s.Cron + "  " + target})
	}
	if mc.App != "" {
		rows = append(rows, [2]string{"app", mc.App})
	}
	if len(mc.Labels) > 0 {
		keys := make([]string, 0, len(mc.Labels))
		for k := range mc.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+mc.Labels[k])
		}
		rows = append(rows, [2]string{"labels", strings.Join(parts, " ")})
	}
	if mc.ServiceID != "" {
		rows = append(rows, [2]string{"service", mc.ServiceID}, [2]string{"release", mc.ReleaseID})
	}
	if mc.VolumeID != "" {
		rows = append(rows, [2]string{"volume", mc.VolumeID})
	}
	help := m.helpLine("c", "console", "L", "logs", "s/w", "suspend/wake", "K", "checkpoint", "P", "promote", "D", "destroy", "esc", "back")
	panel := m.kvPanel(rows, max(3, m.height-lipgloss.Height(title)-lipgloss.Height(help)))
	return lipgloss.JoinVertical(lipgloss.Left, title, panel, help)
}

func (m *Model) viewService() string {
	s := m.service
	if s == nil {
		return m.viewDashboard()
	}
	title := m.titleBar("service " + s.Name)
	running, _ := replicasOf(m.snap, s)
	urlAuth := s.URLAuth
	if urlAuth == "" {
		urlAuth = "public"
	}
	rows := [][2]string{
		{"url", serviceAddress(s)},
		{"url auth", urlAuth},
		{"id", s.ID},
		{"app", s.App},
		{"replicas", fmt.Sprintf("%d running of %d wanted", running, s.Replicas)},
		{"release", s.ReleaseID},
		{"created", time.Unix(s.CreatedAt, 0).Local().Format("2006-01-02 15:04")},
	}
	if s.Health != nil {
		rows = append(rows, [2]string{"health", s.Health.Type + " " + s.Health.Path})
	}
	if s.Repo != "" {
		rows = append(rows, [2]string{"repo", s.Repo + "@" + s.Branch})
	}
	rows = append(rows, [2]string{"", ""})
	for _, mc := range m.snap.Machines {
		if mc.ServiceID == s.ID {
			rows = append(rows, [2]string{"replica", m.st.stateStyle(mc.State).Render(fmt.Sprintf("%-9s", mc.State)) + " " + mc.Name})
		}
	}
	help := m.helpLine("+/-", "replicas", "R", "rollback", "↑↓", "scroll", "esc", "back")
	panel := m.kvPanel(rows, max(3, m.height-lipgloss.Height(title)-lipgloss.Height(help)))
	return lipgloss.JoinVertical(lipgloss.Left, title, panel, help)
}

// viewLogs scrolls: the window holds an offset into the log's lines, pinned
// to the end while following and released the moment you scroll up.
func (m *Model) viewLogs() string {
	name := m.logFor
	if m.machine != nil {
		name = m.machine.Name
	}
	follow := m.st.OK.Render("following")
	if !m.logAuto {
		follow = m.st.Warn.Render("paused")
	}
	title := m.titleBar(fmt.Sprintf("logs %s · %s", name, follow))
	help := m.helpLine("↑↓", "line", "PgUp/PgDn", "page", "g/G", "top/end", "f", "follow", "esc", "back")

	inner := max(1, m.height-lipgloss.Height(title)-lipgloss.Height(help)-2)
	lines := strings.Split(strings.TrimRight(m.logText, "\n"), "\n")
	// Following pins the window to the end; otherwise the offset stands.
	if m.logAuto {
		m.logWin.off = max(0, len(lines)-inner)
	}
	from, to := m.logWin.off, 0
	if from > max(0, len(lines)-inner) {
		from = max(0, len(lines)-inner)
		m.logWin.off = from
	}
	to = min(len(lines), from+inner)
	body := fitBlock(strings.Join(lines[from:to], "\n"), max(1, m.width-4))
	panel := m.st.Panel.Width(m.width - 2).Render(body)
	if hint := scrollHint(from, to, len(lines)); hint != "" {
		title = m.spread(m.st.Title.Render("pilot")+"  "+m.st.Muted.Render(fmt.Sprintf("logs %s · %s", name, follow)),
			m.st.Muted.Render(hint))
	}
	return lipgloss.JoinVertical(lipgloss.Left, title, panel, help)
}

func (m *Model) viewConfirm() string {
	body := fitBlock(m.confirm.Question, min(m.width-6, 66)) + "\n\n" +
		m.st.Key.Render("y") + m.st.Help.Render(" yes    ") +
		m.st.Key.Render("any other key") + m.st.Help.Render(" no")
	return m.st.Focus.Width(min(m.width-4, 70)).Render(body)
}

func (m *Model) viewHelp() string {
	rows := [][2]string{
		{"↑ ↓ j k", "move"},
		{"PgUp PgDn", "page"},
		{"g / G", "top / end"},
		{"tab ← →", "machines ↔ services"},
		{"enter", "open the selected row"},
		{"c", "console (returns here on exit)"},
		{"w", "which workspace this is showing, and change it"},
		{"L", "logs"},
		{"s / w", "suspend / wake"},
		{"S / T", "stop / start"},
		{"K", "checkpoint"},
		{"P", "promote to a service"},
		{"D", "destroy (asks first)"},
		{"+ / -", "scale a service"},
		{"R", "roll back (asks first)"},
		{"r", "refresh now"},
		{"?", "this help"},
		{"q esc", "back, or quit"},
	}
	var lines []string
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("%s  %s", m.st.Key.Render(fmt.Sprintf("%-11s", r[0])), r[1]))
	}
	inner := max(1, m.height-4)
	if len(lines) > inner {
		lines = lines[:inner]
	}
	panel := m.st.Focus.Width(min(m.width-4, 60)).Render(
		m.st.Title.Render("keys") + "\n\n" + strings.Join(lines, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, panel)
}

// overlay centres a box over a screen, keeping the screen's own lines where
// the box does not cover them.
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

// trunc cuts a plain (unstyled) cell to n columns.
func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return s[:n-1] + "…"
}

func mib(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%.1f GiB", float64(n)/1024)
	}
	return fmt.Sprintf("%d MiB", n)
}

// viewWorkspaces is the picker: which workspace this session is showing.
//
// The current one is marked rather than merely highlighted, because the cursor
// starts on it and a highlight alone would not say which of those two facts the
// reader is looking at.
func (m *Model) viewWorkspaces() string {
	if m.workspaces.Err != nil {
		// The reason, not an empty list. An empty picker with nothing saying
		// why reads as a program that failed silently.
		return m.st.Focus.Width(min(m.width-4, 70)).Render(
			fitBlock("could not read the workspaces: "+m.workspaces.Err.Error(),
				min(m.width-6, 66)) + "\n\n" + m.helpLine("esc", "close"))
	}
	if len(m.workspaces.Orgs) == 0 {
		return m.st.Focus.Width(min(m.width-4, 70)).Render(
			fitBlock("this key acts as no workspace yet", min(m.width-6, 66)) + "\n\n" +
				m.helpLine("esc", "close"))
	}

	var b strings.Builder
	for i, org := range m.workspaces.Orgs {
		line := "  " + org
		if org == m.workspaces.Current {
			line += "  " + m.st.Muted.Render("(showing)")
		}
		if i == m.workspaces.Cursor {
			line = m.st.Selected.Render("> " + org)
			if org == m.workspaces.Current {
				line += "  " + m.st.Muted.Render("(showing)")
			}
		}
		b.WriteString(line + "\n")
	}
	if m.workspaces.Admin {
		// Said, because an admin key's list is "what exists" rather than "what
		// you may use", and somebody whose workspace is missing would
		// otherwise conclude they had lost access to it.
		b.WriteString("\n" + m.st.Muted.Render(
			"this key may act as any workspace, including one not listed") + "\n")
	}
	b.WriteString("\n" + m.helpLine("↑↓", "move", "enter", "show", "esc", "close"))
	return m.st.Focus.Width(min(m.width-4, 70)).Render(b.String())
}
