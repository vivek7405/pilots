// Package tui is `pilot tui`: the fleet as a dashboard you leave open.
//
// The shape is borrowed from basecamp/once (Bubble Tea v2 + lipgloss v2, one
// App routing between screens, a tick that refreshes state, keyboard help at
// the bottom). The content is pilots': hosts, machines, services, releases,
// logs, and the actions a machine or service takes -- there is no app
// installer here and no per-container metrics API, so the cards show what
// the fleet actually reports rather than pretending.
package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// palette is chosen once the terminal has said whether its background is
// dark; until then the dark set is used, which reads acceptably on both. A
// theme is not something a person should have to configure for a dashboard.
type palette struct {
	Text, Muted, Border, Primary, Focused, OK, Warn, Bad, Tint color.Color
	dark                                                       bool
}

func newPalette(dark bool) palette {
	ld := lipgloss.LightDark(dark)
	return palette{
		dark:    dark,
		Text:    ld(lipgloss.Color("#1f2328"), lipgloss.Color("#e6edf3")),
		Muted:   ld(lipgloss.Color("#6e7781"), lipgloss.Color("#8b949e")),
		Border:  ld(lipgloss.Color("#b6bec8"), lipgloss.Color("#3d444d")),
		Primary: ld(lipgloss.Color("#0969da"), lipgloss.Color("#58a6ff")),
		Focused: ld(lipgloss.Color("#0969da"), lipgloss.Color("#79c0ff")),
		OK:      ld(lipgloss.Color("#1a7f37"), lipgloss.Color("#3fb950")),
		Warn:    ld(lipgloss.Color("#9a6700"), lipgloss.Color("#d29922")),
		Bad:     ld(lipgloss.Color("#cf222e"), lipgloss.Color("#f85149")),
		Tint:    ld(lipgloss.Color("#f6f8fa"), lipgloss.Color("#161b22")),
	}
}

// isDark reports whether a terminal background is dark, by luminance. A
// terminal that never answers the query keeps the dark default.
func isDark(c color.Color) bool {
	if c == nil {
		return true
	}
	r, g, b, _ := c.RGBA()
	lum := 0.2126*float64(r>>8) + 0.7152*float64(g>>8) + 0.0722*float64(b>>8)
	return lum < 128
}

type styles struct {
	p        palette
	Title    lipgloss.Style
	Muted    lipgloss.Style
	Header   lipgloss.Style
	Selected lipgloss.Style
	Panel    lipgloss.Style
	Focus    lipgloss.Style
	Key      lipgloss.Style
	Help     lipgloss.Style
	Bad      lipgloss.Style
	OK       lipgloss.Style
	Warn     lipgloss.Style
}

func newStyles(p palette) styles {
	return styles{
		p:        p,
		Title:    lipgloss.NewStyle().Foreground(p.Primary).Bold(true),
		Muted:    lipgloss.NewStyle().Foreground(p.Muted),
		Header:   lipgloss.NewStyle().Foreground(p.Muted).Bold(true),
		Selected: lipgloss.NewStyle().Foreground(p.Text).Background(p.Tint).Bold(true),
		Panel:    lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(p.Border).Padding(0, 1),
		Focus:    lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(p.Focused).Padding(0, 1),
		Key:      lipgloss.NewStyle().Foreground(p.Primary).Bold(true),
		Help:     lipgloss.NewStyle().Foreground(p.Muted),
		Bad:      lipgloss.NewStyle().Foreground(p.Bad),
		OK:       lipgloss.NewStyle().Foreground(p.OK),
		Warn:     lipgloss.NewStyle().Foreground(p.Warn),
	}
}

// stateStyle colours a machine state the way a person reads it: green is
// serving, amber is parked, red needs a look.
func (s styles) stateStyle(state string) lipgloss.Style {
	switch state {
	case "running":
		return s.OK
	case "suspended", "stopped", "creating":
		return s.Warn
	case "error":
		return s.Bad
	}
	return s.Muted
}

// sparkline draws a series in eight-level block characters, scaled to the
// series' own maximum, so a card shows the shape of the last minute at a
// glance. Not braille: block characters survive every font.
func sparkline(data []float64, width int) string {
	if width <= 0 {
		return ""
	}
	if len(data) > width {
		data = data[len(data)-width:]
	}
	maxV := 0.0
	for _, v := range data {
		if v > maxV {
			maxV = v
		}
	}
	levels := []rune("▁▂▃▄▅▆▇█")
	out := make([]rune, 0, width)
	for i := 0; i < width-len(data); i++ {
		out = append(out, ' ')
	}
	for _, v := range data {
		idx := 0
		if maxV > 0 {
			idx = int((v / maxV) * float64(len(levels)-1))
		}
		out = append(out, levels[idx])
	}
	return string(out)
}
