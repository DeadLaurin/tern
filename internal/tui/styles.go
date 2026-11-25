package tui

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// noColor reports whether styled output should be disabled. It honours both
// the explicit option and the NO_COLOR environment variable (any value,
// including the empty string, disables color per the NO_COLOR convention).
func noColor(explicit bool) bool {
	if explicit {
		return true
	}
	_, set := os.LookupEnv("NO_COLOR")
	return set
}

// The palette is deliberately muted and cohesive (Tokyo-Night/Slate inspired)
// so long sessions stay calm instead of shouting in the terminal's brightest
// 16 ANSI colours.
const (
	colorAccent  = "#7AA2F7" // soft indigo
	colorTeal    = "#73DACA" // muted teal
	colorSage    = "#9ECE6A" // soft sage
	colorAmber   = "#E0AF68" // warm amber
	colorCoral   = "#F7768E" // soft coral
	colorText    = "#C0CAF5" // soft text
	colorMuted   = "#A9B1D6" // secondary text
	colorSubtle  = "#565F89" // muted slate
	colorSurface = "#1F2335" // subtle surface
	colorPanel   = "#24283B" // raised surface
	colorBorder  = "#3B4261" // subtle border
)

// styles bundles every style used by the TUI. When plain is true all helpers
// return unstyled text, which keeps redirected output and NO_COLOR sessions
// free of ANSI escape sequences.
type styles struct {
	plain bool

	user      lipgloss.Style
	assistant lipgloss.Style
	reasoning lipgloss.Style
	tool      lipgloss.Style
	toolName  lipgloss.Style
	error     lipgloss.Style
	success   lipgloss.Style
	warn      lipgloss.Style
	info      lipgloss.Style
	dim       lipgloss.Style
	bold      lipgloss.Style
	italic    lipgloss.Style
	heading   lipgloss.Style
	quote     lipgloss.Style
	list      lipgloss.Style
	inline    lipgloss.Style
	codeBlock lipgloss.Style
	keyword   lipgloss.Style
	str       lipgloss.Style
	comment   lipgloss.Style
	number    lipgloss.Style

	statusBar    lipgloss.Style
	statusKey    lipgloss.Style
	statusValue  lipgloss.Style
	statusAccent lipgloss.Style
	prompt       lipgloss.Style
	cursor       lipgloss.Style
	placeholder  lipgloss.Style
	border       lipgloss.Style
	selected     lipgloss.Style
	diffAdd      lipgloss.Style
	diffDel      lipgloss.Style
	diffHunk     lipgloss.Style

	// Surfaces and accents used by the welcome banner and modals.
	accent      lipgloss.Style
	teal        lipgloss.Style
	sage        lipgloss.Style
	amber       lipgloss.Style
	coral       lipgloss.Style
	userAccent  lipgloss.Style
	surface     lipgloss.Style
	panel       lipgloss.Style
	modalBorder lipgloss.Style
	modalTitle  lipgloss.Style
}

func newStyles(plain bool) styles {
	s := styles{plain: plain}
	if plain {
		return s
	}
	s.user = lipgloss.NewStyle().Foreground(lipgloss.Color(colorText))
	s.assistant = lipgloss.NewStyle().Foreground(lipgloss.Color(colorText))
	s.reasoning = lipgloss.NewStyle().Foreground(lipgloss.Color(colorSubtle)).Italic(true)
	s.tool = lipgloss.NewStyle().Foreground(lipgloss.Color(colorSubtle))
	s.toolName = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAmber))
	s.error = lipgloss.NewStyle().Foreground(lipgloss.Color(colorCoral))
	s.success = lipgloss.NewStyle().Foreground(lipgloss.Color(colorSage))
	s.warn = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAmber))
	s.info = lipgloss.NewStyle().Foreground(lipgloss.Color(colorTeal))
	s.dim = lipgloss.NewStyle().Foreground(lipgloss.Color(colorSubtle))
	s.bold = lipgloss.NewStyle().Foreground(lipgloss.Color(colorText)).Bold(true)
	s.italic = lipgloss.NewStyle().Italic(true)
	s.heading = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent)).Bold(true)
	s.quote = lipgloss.NewStyle().Foreground(lipgloss.Color(colorMuted))
	s.list = lipgloss.NewStyle().Foreground(lipgloss.Color(colorTeal))
	s.inline = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAmber)).Background(lipgloss.Color(colorSurface))
	s.codeBlock = lipgloss.NewStyle().Foreground(lipgloss.Color(colorMuted))
	s.keyword = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent)).Bold(true)
	s.str = lipgloss.NewStyle().Foreground(lipgloss.Color(colorSage))
	s.comment = lipgloss.NewStyle().Foreground(lipgloss.Color(colorSubtle)).Italic(true)
	s.number = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAmber))

	s.statusBar = lipgloss.NewStyle().Background(lipgloss.Color(colorSurface)).Foreground(lipgloss.Color(colorText))
	s.statusKey = lipgloss.NewStyle().Background(lipgloss.Color(colorSurface)).Foreground(lipgloss.Color(colorSubtle))
	s.statusValue = lipgloss.NewStyle().Background(lipgloss.Color(colorSurface)).Foreground(lipgloss.Color(colorText))
	s.statusAccent = lipgloss.NewStyle().Background(lipgloss.Color(colorSurface)).Foreground(lipgloss.Color(colorAccent)).Bold(true)
	s.prompt = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent)).Bold(true)
	s.cursor = lipgloss.NewStyle().Reverse(true)
	s.placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color(colorSubtle))
	s.border = lipgloss.NewStyle().Foreground(lipgloss.Color(colorBorder))
	s.selected = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent)).Bold(true)
	s.diffAdd = lipgloss.NewStyle().Foreground(lipgloss.Color(colorSage))
	s.diffDel = lipgloss.NewStyle().Foreground(lipgloss.Color(colorCoral))
	s.diffHunk = lipgloss.NewStyle().Foreground(lipgloss.Color(colorTeal))

	s.accent = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent))
	s.teal = lipgloss.NewStyle().Foreground(lipgloss.Color(colorTeal))
	s.sage = lipgloss.NewStyle().Foreground(lipgloss.Color(colorSage))
	s.amber = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAmber))
	s.coral = lipgloss.NewStyle().Foreground(lipgloss.Color(colorCoral))
	s.userAccent = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent))
	s.surface = lipgloss.NewStyle().Background(lipgloss.Color(colorSurface)).Foreground(lipgloss.Color(colorText))
	s.panel = lipgloss.NewStyle().Background(lipgloss.Color(colorPanel)).Foreground(lipgloss.Color(colorText))
	s.modalBorder = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colorBorder)).
		Background(lipgloss.Color(colorPanel)).
		Padding(0, 1)
	s.modalTitle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent)).Bold(true)
	return s
}

func (s styles) paint(style lipgloss.Style, text string) string {
	if s.plain || text == "" {
		return text
	}
	return style.Render(text)
}

// statusSegments renders a status bar with separators. It never emits ANSI
// when styles are plain.
func (s styles) statusSegments(parts ...string) string {
	if len(parts) == 0 {
		return ""
	}
	sep := " · "
	if s.plain {
		sep = " | "
	}
	if s.plain {
		return strings.Join(parts, sep)
	}
	var b strings.Builder
	for i, part := range parts {
		if i > 0 {
			b.WriteString(s.paint(s.statusKey, sep))
		}
		b.WriteString(s.paint(s.statusValue, part))
	}
	return b.String()
}
