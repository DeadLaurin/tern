// Package theme implements tern's local CSS-based design system.
//
// A user can drop a theme.css into the global config directory and/or a
// project's .tern directory. Tokens declared inside :root are layered (global
// first, then project) and exposed to the TUI and to `tern serve`'s web UI.
//
// Customizations live entirely in the user's own files, so they survive tern
// updates. When tern introduces new design tokens or a newer schema version we
// only warn about them; existing customizations always remain active.
package theme

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// CurrentSchemaVersion is the newest theme schema understood by this build.
const CurrentSchemaVersion = 1

// ThemeFileName is the file name used both globally and inside <workdir>/.tern.
const ThemeFileName = "theme.css"

// TokenSpec describes one canonical design token.
type TokenSpec struct {
	Name         string
	DefaultDark  string
	DefaultLight string
	Description  string
}

// CanonicalTokens returns every design token tern knows about, in the order
// they are emitted by DefaultCSS.
func CanonicalTokens() []TokenSpec {
	return []TokenSpec{
		{
			Name:         "--bg",
			DefaultDark:  "#14171c",
			DefaultLight: "#faf9f5",
			Description:  "canvas background (web UI; TUI inherits terminal bg unless set)",
		},
		{
			Name:         "--surface",
			DefaultDark:  "#1b1f27",
			DefaultLight: "#f0eee6",
			Description:  "subtle card/code surface",
		},
		{
			Name:         "--border",
			DefaultDark:  "#2e3440",
			DefaultLight: "#b8b2a6",
			Description:  "hairline borders & dividers",
		},
		{
			Name:         "--text",
			DefaultDark:  "#e6eaf0",
			DefaultLight: "#16150f",
			Description:  "primary text",
		},
		{
			Name:         "--muted",
			DefaultDark:  "#9aa3b2",
			DefaultLight: "#4c4940",
			Description:  "secondary text",
		},
		{
			Name:         "--dim",
			DefaultDark:  "#6c7586",
			DefaultLight: "#767063",
			Description:  "dim metadata & timestamps",
		},
		{
			Name:         "--accent",
			DefaultDark:  "#e6eaf0",
			DefaultLight: "#16150f",
			Description:  "primary highlight",
		},
		{
			Name:         "--sage",
			DefaultDark:  "#8fbc8f",
			DefaultLight: "#2d6a4f",
			Description:  "success, additions, active prompt",
		},
		{
			Name:         "--amber",
			DefaultDark:  "#d8b46a",
			DefaultLight: "#9a6700",
			Description:  "warnings, active tools, approvals",
		},
		{
			Name:         "--blue",
			DefaultDark:  "#88a9d3",
			DefaultLight: "#2c5d8a",
			Description:  "info, links, slash commands",
		},
		{
			Name:         "--red",
			DefaultDark:  "#d97a7a",
			DefaultLight: "#b42318",
			Description:  "errors, deletions, YOLO alert",
		},
		{
			Name:         "--reasoning",
			DefaultDark:  "#8e96a4",
			DefaultLight: "#4c4940",
			Description:  "collapsed/expanded thinking rail",
		},
		{
			Name:         "--radius",
			DefaultDark:  "8px",
			DefaultLight: "8px",
			Description:  "corner radius (web UI)",
		},
		{
			Name:         "--font-mono",
			DefaultDark:  `ui-monospace, "SF Mono", Menlo, Monaco, Consolas, "Liberation Mono", monospace`,
			DefaultLight: `ui-monospace, "SF Mono", Menlo, Monaco, Consolas, "Liberation Mono", monospace`,
			Description:  "monospace font stack (web UI)",
		},
	}
}

// TokenDefault returns the built-in value for token in the given base theme
// ("dark" or "light"). It returns "" for unknown tokens.
func TokenDefault(token, base string) string {
	light := strings.EqualFold(strings.TrimSpace(base), "light")
	for _, spec := range CanonicalTokens() {
		if spec.Name != token {
			continue
		}
		if light {
			return spec.DefaultLight
		}
		return spec.DefaultDark
	}
	return ""
}

// DefaultCSS renders a complete, readable theme.css template for base ("dark"
// or "light"). Unknown base values fall back to dark.
func DefaultCSS(base string) string {
	light := strings.EqualFold(strings.TrimSpace(base), "light")
	baseName := "dark"
	if light {
		baseName = "light"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "/* tern-theme-version: %d */\n", CurrentSchemaVersion)
	fmt.Fprintf(&b, "/* tern %s theme. Edit any value below; your file is never overwritten\n", baseName)
	b.WriteString("   by updates. Run `tern theme check` after upgrading to see new tokens. */\n")
	b.WriteString(":root {\n")
	for _, spec := range CanonicalTokens() {
		value := spec.DefaultDark
		if light {
			value = spec.DefaultLight
		}
		fmt.Fprintf(&b, "  /* %s */\n", spec.Description)
		fmt.Fprintf(&b, "  %s: %s;\n", spec.Name, value)
	}
	b.WriteString("}\n")
	return b.String()
}

// LoadedTheme is the result of layering the user's global and project themes.
type LoadedTheme struct {
	// Files lists the loaded theme.css paths in order: global, then project.
	Files []string
	// SchemaVersion is the oldest schema version found in the loaded files, or
	// CurrentSchemaVersion when no custom file exists.
	SchemaVersion int
	// Tokens holds the merged token declarations found in the loaded files.
	// Project values override global values.
	Tokens map[string]string
	// RawCSS is the concatenated CSS from all loaded files, ready to inject
	// into `tern serve`'s <style> element.
	RawCSS string
	// MissingTokens lists canonical tokens that a custom theme.css never
	// declares. Those tokens keep using tern's built-in defaults.
	MissingTokens []string
	// Warnings explains schema drift, missing version headers and missing
	// tokens. Every warning notes that existing customizations stay active.
	Warnings []string
}

// HasCustom reports whether any custom theme file was loaded.
func (t *LoadedTheme) HasCustom() bool {
	return t != nil && len(t.Files) > 0
}

// Color returns the value of token if it is a valid CSS hex color or ANSI
// color, otherwise fallback.
func (t *LoadedTheme) Color(token, fallback string) string {
	if t == nil {
		return fallback
	}
	value := strings.TrimSpace(t.Tokens[token])
	if IsColorValue(value) {
		return value
	}
	return fallback
}

var (
	versionRe = regexp.MustCompile(`(?i)tern-theme-version\s*:\s*(\d+)`)
	commentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	rootRe    = regexp.MustCompile(`(?is):root\s*\{([^}]*)\}`)
	tokenRe   = regexp.MustCompile(`(?m)--([A-Za-z0-9_-]+)\s*:\s*([^;]+);?`)
)

// Load layers <globalConfigDir>/theme.css and <workDir>/.tern/theme.css. Either
// or both files may be absent; absence is not an error.
func Load(workDir, globalConfigDir string) (*LoadedTheme, error) {
	loaded := &LoadedTheme{
		SchemaVersion: CurrentSchemaVersion,
		Tokens:        make(map[string]string),
	}

	var paths []string
	if globalConfigDir != "" {
		paths = append(paths, filepath.Join(globalConfigDir, ThemeFileName))
	}
	if workDir != "" {
		paths = append(paths, filepath.Join(workDir, ".tern", ThemeFileName))
	}

	versionFound := false
	minVersion := CurrentSchemaVersion
	var cssParts []string

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read theme %s: %w", path, err)
		}
		loaded.Files = append(loaded.Files, path)
		raw := string(data)
		cssParts = append(cssParts, raw)

		// Read the version header before stripping comments, then strip comments
		// so commented-out tokens are ignored.
		if match := versionRe.FindStringSubmatch(raw); match != nil {
			version, err := strconv.Atoi(match[1])
			if err == nil {
				versionFound = true
				if version < minVersion {
					minVersion = version
				}
			}
		} else {
			// A file without a header is treated as the oldest schema.
			versionFound = true
			if 0 < minVersion {
				minVersion = 0
			}
			loaded.Warnings = append(loaded.Warnings, fmt.Sprintf(
				"%s is missing the /* tern-theme-version: %d */ header; treating it as schema version 0. Existing customizations remain active, and missing tokens use tern's built-in defaults.",
				path, CurrentSchemaVersion))
		}

		stripped := commentRe.ReplaceAllString(raw, "")
		for _, root := range rootRe.FindAllStringSubmatch(stripped, -1) {
			for _, token := range tokenRe.FindAllStringSubmatch(root[1], -1) {
				name := "--" + token[1]
				value := strings.TrimSpace(token[2])
				if value != "" {
					loaded.Tokens[name] = value
				}
			}
		}
	}

	loaded.RawCSS = strings.Join(cssParts, "\n")
	if loaded.HasCustom() {
		if versionFound {
			loaded.SchemaVersion = minVersion
		}
		if loaded.SchemaVersion < CurrentSchemaVersion {
			loaded.Warnings = append(loaded.Warnings, fmt.Sprintf(
				"theme schema version %d is older than the current version %d. Existing customizations remain active; new design tokens use tern's built-in defaults. Add the missing tokens or run `tern theme reset` to adopt the new template.",
				loaded.SchemaVersion, CurrentSchemaVersion))
		}
		for _, spec := range CanonicalTokens() {
			if _, ok := loaded.Tokens[spec.Name]; !ok {
				loaded.MissingTokens = append(loaded.MissingTokens, spec.Name)
			}
		}
		if len(loaded.MissingTokens) > 0 {
			sort.Strings(loaded.MissingTokens)
			loaded.Warnings = append(loaded.Warnings, fmt.Sprintf(
				"%s does not declare %d canonical token(s): %s. Existing customizations remain active; the missing tokens use tern's built-in defaults.",
				ThemeFileName, len(loaded.MissingTokens), strings.Join(loaded.MissingTokens, ", ")))
		}
	}

	return loaded, nil
}

// InitFile writes DefaultCSS(base) to path. It refuses to overwrite an existing
// file unless force is true.
func InitFile(path, base string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("theme file already exists: %s (use --force to overwrite)", path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat theme file %s: %w", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create theme directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(DefaultCSS(base)), 0o644); err != nil {
		return fmt.Errorf("write theme file %s: %w", path, err)
	}
	return nil
}

var (
	hexColorRe = regexp.MustCompile(`(?i)^#(?:[0-9a-f]{3}|[0-9a-f]{6})$`)
	ansi256Re  = regexp.MustCompile(`(?i)^ansi(?:256)?\(\s*(?:[0-9]{1,2}|1[0-9]{2}|2[0-4][0-9]|25[0-5])\s*\)$`)
)

var ansiColorNames = map[string]bool{
	"black": true, "red": true, "green": true, "yellow": true,
	"blue": true, "magenta": true, "cyan": true, "white": true,
	"gray": true, "grey": true,
	"bright-black": true, "bright-red": true, "bright-green": true,
	"bright-yellow": true, "bright-blue": true, "bright-magenta": true,
	"bright-cyan": true, "bright-white": true,
}

// IsColorValue reports whether value is a CSS hex color (#rgb or #rrggbb) or an
// ANSI color (a 0-255 index, a named ANSI color, or ansi(N)/ansi256(N)).
func IsColorValue(value string) bool {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return false
	}
	if hexColorRe.MatchString(v) || ansi256Re.MatchString(v) {
		return true
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n >= 0 && n <= 255
	}
	return ansiColorNames[v]
}
