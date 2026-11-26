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

// The palette is deliberately minimalist and neutral so the terminal's own
// background stays completely even across every pane, bar, and modal. Only
// quiet semantic tints carry color; everything else is crisp neutral text.
const (
	colorAccent  = "#E6EAF0" // crisp neutral highlight (no blue)
	colorTeal    = "#B8C0CC" // soft silver secondary accent
	colorSage    = "#8FBC8F" // quiet success / sage
	colorAmber   = "#D8B46A" // quiet warning / active tool
	colorCoral   = "#D47A84" // quiet error / YOLO alert
	colorSlate   = "#8E96A4" // muted reasoning rail
	colorText    = "#E2E6EC" // primary text
	colorMuted   = "#A0A8B5" // secondary text
	colorSubtle  = "#6D7685" // dim metadata
	colorSurface = ""        // transparent: always inherit terminal background
	colorPanel   = ""        // transparent: always inherit terminal background
	colorBorder  = "#434B59" // subtle neutral hairline border
)

// The light palette is Swiss-paper inspired: dense ink on paper, sage for
// additions, amber for tools, and rose for errors. It never paints a surface
// either, so the terminal background stays even and the contrast comes purely
// from foreground ink.
const (
	lightAccent = "#16150F" // primary ink
	lightTeal   = "#4C4940" // secondary ink
	lightSage   = "#2D6A4F" // sage add / success
	lightAmber  = "#9A6700" // amber tool / warning
	lightCoral  = "#B42318" // rose error / YOLO
	lightSlate  = "#4C4940" // muted reasoning rail
	lightText   = "#16150F" // primary text
	lightMuted  = "#4C4940" // secondary text
	lightSubtle = "#767063" // dim metadata
	lightBorder = "#B8B2A6" // hairline border
)

// palette holds the semantic foreground colors for one theme. Empty values are
// transparent and always inherit the terminal background.
type palette struct {
	accent string
	teal   string
	sage   string
	amber  string
	coral  string
	slate  string
	text   string
	muted  string
	subtle string
	border string
}

func paletteForTheme(theme string) palette {
	if strings.EqualFold(strings.TrimSpace(theme), "light") {
		return palette{
			accent: lightAccent,
			teal:   lightTeal,
			sage:   lightSage,
			amber:  lightAmber,
			coral:  lightCoral,
			slate:  lightSlate,
			text:   lightText,
			muted:  lightMuted,
			subtle: lightSubtle,
			border: lightBorder,
		}
	}
	return palette{
		accent: colorAccent,
		teal:   colorTeal,
		sage:   colorSage,
		amber:  colorAmber,
		coral:  colorCoral,
		slate:  colorSlate,
		text:   colorText,
		muted:  colorMuted,
		subtle: colorSubtle,
		border: colorBorder,
	}
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
	statusAlert  lipgloss.Style
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

// newStyles keeps every existing caller on the calm neutral dark palette.
func newStyles(plain bool) styles {
	return newStylesForTheme(plain, "dark")
}

// newStylesForTheme builds the style bundle for "dark" or "light". Both
// palettes only ever set foreground colors, so the terminal background shows
// through evenly and neither theme produces patchy filled rectangles.
func newStylesForTheme(plain bool, theme string) styles {
	s := styles{plain: plain}
	if plain {
		return s
	}
	p := paletteForTheme(theme)
	color := func(hex string) lipgloss.Color { return lipgloss.Color(hex) }

	s.user = lipgloss.NewStyle().Foreground(color(p.text))
	s.assistant = lipgloss.NewStyle().Foreground(color(p.text))
	s.reasoning = lipgloss.NewStyle().Foreground(color(p.slate)).Italic(true)
	s.tool = lipgloss.NewStyle().Foreground(color(p.subtle))
	s.toolName = lipgloss.NewStyle().Foreground(color(p.amber))
	s.error = lipgloss.NewStyle().Foreground(color(p.coral))
	s.success = lipgloss.NewStyle().Foreground(color(p.sage))
	s.warn = lipgloss.NewStyle().Foreground(color(p.amber))
	s.info = lipgloss.NewStyle().Foreground(color(p.teal))
	s.dim = lipgloss.NewStyle().Foreground(color(p.subtle))
	s.bold = lipgloss.NewStyle().Foreground(color(p.text)).Bold(true)
	s.italic = lipgloss.NewStyle().Italic(true)
	s.heading = lipgloss.NewStyle().Foreground(color(p.accent)).Bold(true)
	s.quote = lipgloss.NewStyle().Foreground(color(p.muted))
	s.list = lipgloss.NewStyle().Foreground(color(p.teal))
	s.inline = lipgloss.NewStyle().Foreground(color(p.amber))
	s.codeBlock = lipgloss.NewStyle().Foreground(color(p.muted))
	s.keyword = lipgloss.NewStyle().Foreground(color(p.accent)).Bold(true)
	s.str = lipgloss.NewStyle().Foreground(color(p.sage))
	s.comment = lipgloss.NewStyle().Foreground(color(p.subtle)).Italic(true)
	s.number = lipgloss.NewStyle().Foreground(color(p.amber))

	s.statusBar = lipgloss.NewStyle().Foreground(color(p.muted))
	s.statusKey = lipgloss.NewStyle().Foreground(color(p.subtle))
	s.statusValue = lipgloss.NewStyle().Foreground(color(p.muted))
	s.statusAccent = lipgloss.NewStyle().Foreground(color(p.accent)).Bold(true)
	s.statusAlert = lipgloss.NewStyle().Foreground(color(p.coral)).Bold(true)
	s.prompt = lipgloss.NewStyle().Foreground(color(p.accent)).Bold(true)
	s.cursor = lipgloss.NewStyle().Reverse(true)
	s.placeholder = lipgloss.NewStyle().Foreground(color(p.subtle))
	s.border = lipgloss.NewStyle().Foreground(color(p.border))
	s.selected = lipgloss.NewStyle().Foreground(color(p.accent)).Bold(true)
	s.diffAdd = lipgloss.NewStyle().Foreground(color(p.sage))
	s.diffDel = lipgloss.NewStyle().Foreground(color(p.coral))
	s.diffHunk = lipgloss.NewStyle().Foreground(color(p.teal))

	s.accent = lipgloss.NewStyle().Foreground(color(p.accent))
	s.teal = lipgloss.NewStyle().Foreground(color(p.teal))
	s.sage = lipgloss.NewStyle().Foreground(color(p.sage))
	s.amber = lipgloss.NewStyle().Foreground(color(p.amber))
	s.coral = lipgloss.NewStyle().Foreground(color(p.coral))
	s.userAccent = lipgloss.NewStyle().Foreground(color(p.muted))
	s.surface = lipgloss.NewStyle().Foreground(color(p.text))
	s.panel = lipgloss.NewStyle().Foreground(color(p.text))
	s.modalBorder = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(color(p.border)).
		Padding(0, 1)
	s.modalTitle = lipgloss.NewStyle().Foreground(color(p.accent)).Bold(true)
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
		style := s.statusValue
		if part == yoloLabel {
			style = s.statusAlert
		}
		b.WriteString(s.paint(style, part))
	}
	return b.String()
}
