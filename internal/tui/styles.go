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
}

func newStyles(plain bool) styles {
	s := styles{plain: plain}
	if plain {
		return s
	}
	s.user = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	s.assistant = lipgloss.NewStyle()
	s.reasoning = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
	s.tool = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	s.toolName = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	s.error = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	s.success = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	s.warn = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	s.info = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	s.dim = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	s.bold = lipgloss.NewStyle().Bold(true)
	s.italic = lipgloss.NewStyle().Italic(true)
	s.heading = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	s.quote = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	s.list = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	s.inline = lipgloss.NewStyle().Foreground(lipgloss.Color("215")).Background(lipgloss.Color("236"))
	s.codeBlock = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	s.keyword = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
	s.str = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	s.comment = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Italic(true)
	s.number = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))

	s.statusBar = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("252"))
	s.statusKey = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("244"))
	s.statusValue = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("252"))
	s.statusAccent = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("12")).Bold(true)
	s.prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	s.cursor = lipgloss.NewStyle().Reverse(true)
	s.placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	s.border = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	s.selected = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	s.diffAdd = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	s.diffDel = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	s.diffHunk = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
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
	sep := " | "
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
