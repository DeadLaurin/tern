package theme

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTheme(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultCSSRoundTripsThroughLoad(t *testing.T) {
	for _, base := range []string{"dark", "light"} {
		t.Run(base, func(t *testing.T) {
			workDir := t.TempDir()
			configDir := t.TempDir()
			path := filepath.Join(configDir, ThemeFileName)
			if err := InitFile(path, base, false); err != nil {
				t.Fatalf("InitFile: %v", err)
			}

			loaded, err := Load(workDir, configDir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !loaded.HasCustom() {
				t.Fatal("expected a custom theme to be loaded")
			}
			if loaded.SchemaVersion != CurrentSchemaVersion {
				t.Fatalf("SchemaVersion = %d, want %d", loaded.SchemaVersion, CurrentSchemaVersion)
			}
			if len(loaded.Warnings) != 0 {
				t.Fatalf("unexpected warnings: %v", loaded.Warnings)
			}
			if len(loaded.MissingTokens) != 0 {
				t.Fatalf("unexpected missing tokens: %v", loaded.MissingTokens)
			}
			if !strings.Contains(loaded.RawCSS, "tern-theme-version: 1") {
				t.Fatalf("RawCSS is missing the version header:\n%s", loaded.RawCSS)
			}

			light := base == "light"
			for _, spec := range CanonicalTokens() {
				value, ok := loaded.Tokens[spec.Name]
				if !ok {
					t.Errorf("token %s missing from loaded theme", spec.Name)
					continue
				}
				want := spec.DefaultDark
				if light {
					want = spec.DefaultLight
				}
				if value != want {
					t.Errorf("%s = %q, want %q", spec.Name, value, want)
				}
			}
			if len(loaded.Tokens) != len(CanonicalTokens()) {
				t.Errorf("loaded %d tokens, want %d", len(loaded.Tokens), len(CanonicalTokens()))
			}
		})
	}
}

func TestLoadLayersGlobalThenProject(t *testing.T) {
	workDir := t.TempDir()
	configDir := t.TempDir()
	writeTheme(t, filepath.Join(configDir, ThemeFileName), `/* tern-theme-version: 1 */
:root {
  --bg: #111111;
  --text: #222222;
}
`)
	writeTheme(t, filepath.Join(workDir, ".tern", ThemeFileName), `/* tern-theme-version: 1 */
:root {
  --bg: #333333;
}
`)

	loaded, err := Load(workDir, configDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Files) != 2 {
		t.Fatalf("Files = %v, want global then project", loaded.Files)
	}
	if got := loaded.Tokens["--bg"]; got != "#333333" {
		t.Errorf("--bg = %q, want project override #333333", got)
	}
	if got := loaded.Tokens["--text"]; got != "#222222" {
		t.Errorf("--text = %q, want global #222222", got)
	}
	if got := loaded.Color("--bg", "#abcdef"); got != "#333333" {
		t.Errorf("Color(--bg) = %q, want #333333", got)
	}
	if !strings.Contains(loaded.RawCSS, "global") && !strings.Contains(loaded.RawCSS, "#111111") {
		t.Errorf("RawCSS should include both files, got:\n%s", loaded.RawCSS)
	}
	if !strings.Contains(loaded.RawCSS, "#333333") {
		t.Errorf("RawCSS should include the project file, got:\n%s", loaded.RawCSS)
	}
}

func TestLoadWarnsForOldSchemaAndMissingTokens(t *testing.T) {
	workDir := t.TempDir()
	configDir := t.TempDir()
	writeTheme(t, filepath.Join(configDir, ThemeFileName), `/* tern-theme-version: 0 */
:root {
  --bg: #123456;
  /* --text: #000000; commented out tokens are ignored */
}
`)

	loaded, err := Load(workDir, configDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.SchemaVersion != 0 {
		t.Fatalf("SchemaVersion = %d, want 0", loaded.SchemaVersion)
	}
	if got := loaded.Tokens["--bg"]; got != "#123456" {
		t.Fatalf("custom --bg = %q, want #123456 (custom values must stay active)", got)
	}
	if _, ok := loaded.Tokens["--text"]; ok {
		t.Fatal("commented-out --text token must not be loaded")
	}
	if len(loaded.MissingTokens) == 0 {
		t.Fatal("expected missing canonical tokens")
	}
	foundText := false
	for _, name := range loaded.MissingTokens {
		if name == "--text" {
			foundText = true
		}
	}
	if !foundText {
		t.Fatalf("MissingTokens = %v, want --text", loaded.MissingTokens)
	}

	joined := strings.Join(loaded.Warnings, "\n")
	if !strings.Contains(joined, "older than the current version") {
		t.Errorf("warnings should explain the old schema version: %s", joined)
	}
	if !strings.Contains(joined, "does not declare") {
		t.Errorf("warnings should list missing tokens: %s", joined)
	}
	if !strings.Contains(joined, "Existing customizations remain active") {
		t.Errorf("warnings should explain that customizations stay active: %s", joined)
	}
}

func TestLoadWarnsWhenVersionHeaderMissing(t *testing.T) {
	workDir := t.TempDir()
	configDir := t.TempDir()
	writeTheme(t, filepath.Join(configDir, ThemeFileName), ":root { --bg: #010203; }\n")

	loaded, err := Load(workDir, configDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.SchemaVersion != 0 {
		t.Fatalf("SchemaVersion = %d, want 0 for a missing header", loaded.SchemaVersion)
	}
	joined := strings.Join(loaded.Warnings, "\n")
	if !strings.Contains(joined, "missing the /* tern-theme-version") {
		t.Errorf("warnings should mention the missing header: %s", joined)
	}
	if got := loaded.Tokens["--bg"]; got != "#010203" {
		t.Errorf("custom --bg = %q, want #010203", got)
	}
}

func TestLoadWithoutCustomFiles(t *testing.T) {
	loaded, err := Load(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.HasCustom() {
		t.Fatal("expected no custom theme")
	}
	if loaded.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", loaded.SchemaVersion, CurrentSchemaVersion)
	}
	if len(loaded.Warnings) != 0 || len(loaded.MissingTokens) != 0 {
		t.Fatalf("unexpected warnings/missing tokens: %v / %v", loaded.Warnings, loaded.MissingTokens)
	}
	if len(loaded.Tokens) != 0 {
		t.Fatalf("Tokens = %v, want empty without custom files", loaded.Tokens)
	}
}

func TestInitFileRefusesOverwriteWithoutForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "theme.css")
	if err := InitFile(path, "dark", false); err != nil {
		t.Fatalf("first InitFile: %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := InitFile(path, "light", false); err == nil {
		t.Fatal("InitFile should refuse to overwrite without force")
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != string(original) {
		t.Fatal("file changed despite refused overwrite")
	}

	if err := InitFile(path, "light", true); err != nil {
		t.Fatalf("InitFile with force: %v", err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(updated) == string(original) {
		t.Fatal("expected force to overwrite with the light template")
	}
	if !strings.Contains(string(updated), "#faf9f5") {
		t.Fatalf("expected light defaults after force, got:\n%s", updated)
	}
}

func TestColorValidation(t *testing.T) {
	loaded := &LoadedTheme{Tokens: map[string]string{
		"--short":   "#abc",
		"--long":    "#112233",
		"--upper":   "#AABBCC",
		"--ansi":    "ansi256(200)",
		"--named":   "bright-blue",
		"--index":   "42",
		"--bad":     "not-a-color",
		"--badhex":  "#abcd",
		"--empty":   "",
		"--spaced":  "  #123456  ",
		"--varref":  "var(--text)",
		"--toolong": "#11223344",
	}}

	tests := []struct {
		token string
		want  string
	}{
		{"--short", "#abc"},
		{"--long", "#112233"},
		{"--upper", "#AABBCC"},
		{"--ansi", "ansi256(200)"},
		{"--named", "bright-blue"},
		{"--index", "42"},
		{"--spaced", "#123456"},
		{"--bad", "fallback"},
		{"--badhex", "fallback"},
		{"--empty", "fallback"},
		{"--varref", "fallback"},
		{"--toolong", "fallback"},
		{"--absent", "fallback"},
	}
	for _, tt := range tests {
		if got := loaded.Color(tt.token, "fallback"); got != tt.want {
			t.Errorf("Color(%q) = %q, want %q", tt.token, got, tt.want)
		}
	}

	var nilTheme *LoadedTheme
	if got := nilTheme.Color("--short", "fallback"); got != "fallback" {
		t.Errorf("nil theme Color = %q, want fallback", got)
	}
}
