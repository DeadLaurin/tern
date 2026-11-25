package config

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func ptr[T any](v T) *T { return &v }

// clearConfigEnv isolates tests from the developer's real environment.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		EnvConfigDir,
		EnvDataDir,
		EnvModel,
		EnvBaseURL,
		EnvReasoningEffort,
		EnvPermissionMode,
		EnvWebPermission,
		EnvMaxSteps,
		EnvMaxCostUSD,
		EnvMaxAgents,
		EnvMaxAgentDepth,
		EnvAutoCompactThreshold,
		EnvDebugCache,
		EnvShell,
		EnvSearchBackend,
		EnvSearchEndpoint,
		EnvAPIKey,
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"APPDATA",
	} {
		t.Setenv(key, "")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func loadForTest(t *testing.T, configDir, workDir string, flags *FlagOverrides) *Config {
	t.Helper()
	cfg, err := Load(LoadOptions{ConfigDir: configDir, WorkDir: workDir, Flags: flags})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestDefaultConfig(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must be valid: %v", err)
	}

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"model", cfg.Model, "deepseek-flash"},
		{"base_url", cfg.BaseURL, "https://api.deepseek.com"},
		{"reasoning_effort", cfg.ReasoningEffort, "high"},
		{"permission_mode", cfg.PermissionMode, "ask"},
		{"web_permission", cfg.WebPermission, "ask"},
		{"max_steps", cfg.MaxSteps, 50},
		{"max_cost_usd", cfg.MaxCostUSD, 0.0},
		{"max_agents", cfg.MaxAgents, 4},
		{"max_agent_depth", cfg.MaxAgentDepth, 1},
		{"auto_compact_threshold", cfg.AutoCompactThreshold, 0.78},
		{"debug_cache", cfg.DebugCache, false},
		{"shell", cfg.Shell, ""},
		{"search_backend", cfg.SearchBackend, "duckduckgo"},
		{"search_endpoint", cfg.SearchEndpoint, ""},
		{"api_key", cfg.APIKey, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !reflect.DeepEqual(tt.got, tt.want) {
				t.Errorf("%s = %#v, want %#v", tt.name, tt.got, tt.want)
			}
		})
	}

	flash, ok := cfg.Pricing["deepseek-flash"]
	if !ok {
		t.Fatal("missing default pricing for deepseek-flash")
	}
	if flash.ContextWindow != 1_000_000 || flash.MaxOutput != 384_000 || !flash.SupportsVision {
		t.Errorf("unexpected deepseek-flash limits: %+v", flash)
	}
	if flash.OffPeakCacheHitPerM != 0.003 || flash.OffPeakCacheMissPerM != 0.15 || flash.OffPeakOutputPerM != 0.60 {
		t.Errorf("unexpected deepseek-flash off-peak pricing: %+v", flash)
	}
	if flash.PeakCacheHitPerM != 0.006 || flash.PeakCacheMissPerM != 0.30 || flash.PeakOutputPerM != 1.20 {
		t.Errorf("unexpected deepseek-flash peak pricing: %+v", flash)
	}

	pro, ok := cfg.Pricing["deepseek-v4-pro"]
	if !ok {
		t.Fatal("missing default pricing for deepseek-v4-pro")
	}
	if pro.ContextWindow != 1_000_000 || pro.MaxOutput != 384_000 || pro.SupportsVision {
		t.Errorf("unexpected deepseek-v4-pro limits: %+v", pro)
	}
	if pro.OffPeakCacheHitPerM != 0.022 || pro.OffPeakCacheMissPerM != 0.66 || pro.OffPeakOutputPerM != 1.98 {
		t.Errorf("unexpected deepseek-v4-pro off-peak pricing: %+v", pro)
	}
	if pro.PeakCacheHitPerM != 0.044 || pro.PeakCacheMissPerM != 1.32 || pro.PeakOutputPerM != 3.96 {
		t.Errorf("unexpected deepseek-v4-pro peak pricing: %+v", pro)
	}
}

func TestDefaultPricingMapsAreIndependent(t *testing.T) {
	a := Default()
	b := Default()
	a.Pricing["deepseek-flash"] = ModelPricing{ContextWindow: 1}
	if b.Pricing["deepseek-flash"].ContextWindow != 1_000_000 {
		t.Fatal("Default() must return an independent pricing map")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearConfigEnv(t)
	got := loadForTest(t, t.TempDir(), t.TempDir(), nil)
	if !reflect.DeepEqual(got, Default()) {
		t.Fatalf("Load with no files/env/flags = %#v, want defaults %#v", got, Default())
	}
}

func TestLoadPrecedence(t *testing.T) {
	clearConfigEnv(t)

	const global = "model = \"global\"\nmax_steps = 10\n"
	const project = "model = \"project\"\nmax_steps = 20\n"

	tests := []struct {
		name         string
		global       string
		project      string
		env          map[string]string
		flags        *FlagOverrides
		wantModel    string
		wantMaxSteps int
	}{
		{
			name:         "built-in defaults",
			wantModel:    "deepseek-flash",
			wantMaxSteps: 50,
		},
		{
			name:         "global over defaults",
			global:       global,
			wantModel:    "global",
			wantMaxSteps: 10,
		},
		{
			name:         "project over global",
			global:       global,
			project:      project,
			wantModel:    "project",
			wantMaxSteps: 20,
		},
		{
			name:         "env over project",
			global:       global,
			project:      project,
			env:          map[string]string{EnvModel: "env", EnvMaxSteps: "30"},
			wantModel:    "env",
			wantMaxSteps: 30,
		},
		{
			name:         "flags over env",
			global:       global,
			project:      project,
			env:          map[string]string{EnvModel: "env", EnvMaxSteps: "30"},
			flags:        &FlagOverrides{Model: ptr("flag"), MaxSteps: ptr(40)},
			wantModel:    "flag",
			wantMaxSteps: 40,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := t.TempDir()
			workDir := t.TempDir()
			if tt.global != "" {
				writeFile(t, filepath.Join(configDir, "config.toml"), tt.global)
			}
			if tt.project != "" {
				writeFile(t, filepath.Join(workDir, ".tern", "config.toml"), tt.project)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg := loadForTest(t, configDir, workDir, tt.flags)
			if cfg.Model != tt.wantModel {
				t.Errorf("model = %q, want %q", cfg.Model, tt.wantModel)
			}
			if cfg.MaxSteps != tt.wantMaxSteps {
				t.Errorf("max_steps = %d, want %d", cfg.MaxSteps, tt.wantMaxSteps)
			}
			if cfg.BaseURL != DefaultBaseURL {
				t.Errorf("base_url = %q, want default %q", cfg.BaseURL, DefaultBaseURL)
			}
		})
	}
}

func TestLoadMergesAllLayers(t *testing.T) {
	clearConfigEnv(t)
	configDir := t.TempDir()
	workDir := t.TempDir()

	writeFile(t, filepath.Join(configDir, "config.toml"), `
base_url = "https://global.example"
max_agents = 2
search_backend = "brave"
`)
	writeFile(t, filepath.Join(workDir, ".tern", "config.toml"), `
web_permission = "deny"
max_agent_depth = 3
search_endpoint = "https://searx.example"
`)
	t.Setenv(EnvReasoningEffort, "max")
	t.Setenv(EnvAutoCompactThreshold, "0.91")
	t.Setenv(EnvDebugCache, "true")
	t.Setenv(EnvShell, "/bin/fish")

	flags := &FlagOverrides{
		Model:          ptr("flag-model"),
		PermissionMode: ptr("plan"),
		MaxSteps:       ptr(123),
		MaxCostUSD:     ptr(4.25),
	}
	cfg := loadForTest(t, configDir, workDir, flags)

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"model (flag)", cfg.Model, "flag-model"},
		{"base_url (global)", cfg.BaseURL, "https://global.example"},
		{"max_agents (global)", cfg.MaxAgents, 2},
		{"search_backend (global)", cfg.SearchBackend, "brave"},
		{"web_permission (project)", cfg.WebPermission, "deny"},
		{"max_agent_depth (project)", cfg.MaxAgentDepth, 3},
		{"search_endpoint (project)", cfg.SearchEndpoint, "https://searx.example"},
		{"reasoning_effort (env)", cfg.ReasoningEffort, "max"},
		{"auto_compact_threshold (env)", cfg.AutoCompactThreshold, 0.91},
		{"debug_cache (env)", cfg.DebugCache, true},
		{"shell (env)", cfg.Shell, "/bin/fish"},
		{"permission_mode (flag)", cfg.PermissionMode, "plan"},
		{"max_steps (flag)", cfg.MaxSteps, 123},
		{"max_cost_usd (flag)", cfg.MaxCostUSD, 4.25},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.name, c.got, c.want)
		}
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv(EnvModel, "env-model")
	t.Setenv(EnvBaseURL, "https://env.example")
	t.Setenv(EnvReasoningEffort, "none")
	t.Setenv(EnvPermissionMode, "yolo")
	t.Setenv(EnvWebPermission, "allow")
	t.Setenv(EnvMaxSteps, "77")
	t.Setenv(EnvMaxCostUSD, "2.5")
	t.Setenv(EnvMaxAgents, "8")
	t.Setenv(EnvMaxAgentDepth, "2")
	t.Setenv(EnvAutoCompactThreshold, "0.5")
	t.Setenv(EnvDebugCache, "1")
	t.Setenv(EnvShell, "bash")
	t.Setenv(EnvSearchBackend, "tavily")
	t.Setenv(EnvSearchEndpoint, "https://tavily.example")

	cfg := loadForTest(t, t.TempDir(), t.TempDir(), nil)
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"model", cfg.Model, "env-model"},
		{"base_url", cfg.BaseURL, "https://env.example"},
		{"reasoning_effort", cfg.ReasoningEffort, "none"},
		{"permission_mode", cfg.PermissionMode, "yolo"},
		{"web_permission", cfg.WebPermission, "allow"},
		{"max_steps", cfg.MaxSteps, 77},
		{"max_cost_usd", cfg.MaxCostUSD, 2.5},
		{"max_agents", cfg.MaxAgents, 8},
		{"max_agent_depth", cfg.MaxAgentDepth, 2},
		{"auto_compact_threshold", cfg.AutoCompactThreshold, 0.5},
		{"debug_cache", cfg.DebugCache, true},
		{"shell", cfg.Shell, "bash"},
		{"search_backend", cfg.SearchBackend, "tavily"},
		{"search_endpoint", cfg.SearchEndpoint, "https://tavily.example"},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.name, c.got, c.want)
		}
	}
}

func TestLoadEnvParseErrors(t *testing.T) {
	clearConfigEnv(t)
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"max steps", EnvMaxSteps, "many"},
		{"max agents", EnvMaxAgents, "1.5"},
		{"max agent depth", EnvMaxAgentDepth, "deep"},
		{"max cost", EnvMaxCostUSD, "expensive"},
		{"threshold", EnvAutoCompactThreshold, "often"},
		{"debug cache", EnvDebugCache, "sometimes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			_, err := Load(LoadOptions{ConfigDir: t.TempDir(), WorkDir: t.TempDir()})
			if err == nil {
				t.Fatalf("Load succeeded, want error for %s=%q", tt.key, tt.value)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q does not mention %q", err, tt.key)
			}
		})
	}
}

func TestPricingLayering(t *testing.T) {
	fullFlash := func(peakOutput float64) string {
		return "peak_output_per_m = " + formatFloat(peakOutput) + `
context_window = 111
max_output = 222
supports_vision = true
off_peak_cache_hit_per_m = 1
off_peak_cache_miss_per_m = 2
off_peak_output_per_m = 3
peak_cache_hit_per_m = 4
peak_cache_miss_per_m = 5
`
	}

	tests := []struct {
		name             string
		pricingTOML      string
		configTOML       string
		wantPeakOutput   float64
		wantContext      int
		wantCustomOutput float64
	}{
		{
			name:           "pricing section layout",
			pricingTOML:    "[pricing.deepseek-flash]\n" + fullFlash(9),
			wantPeakOutput: 9, wantContext: 111,
		},
		{
			name:           "top-level table layout",
			pricingTOML:    "[deepseek-flash]\n" + fullFlash(8),
			wantPeakOutput: 8, wantContext: 111,
		},
		{
			name:           "config.toml overrides pricing.toml",
			pricingTOML:    "[pricing.deepseek-flash]\n" + fullFlash(9) + "[pricing.custom-model]\n" + fullFlash(7),
			configTOML:     "[pricing.deepseek-flash]\n" + fullFlash(10),
			wantPeakOutput: 10, wantContext: 111, wantCustomOutput: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			configDir := t.TempDir()
			writeFile(t, filepath.Join(configDir, "pricing.toml"), tt.pricingTOML)
			if tt.configTOML != "" {
				writeFile(t, filepath.Join(configDir, "config.toml"), tt.configTOML)
			}

			cfg := loadForTest(t, configDir, t.TempDir(), nil)
			flash := cfg.Pricing["deepseek-flash"]
			if flash.PeakOutputPerM != tt.wantPeakOutput {
				t.Errorf("deepseek-flash peak_output_per_m = %g, want %g", flash.PeakOutputPerM, tt.wantPeakOutput)
			}
			if flash.ContextWindow != tt.wantContext {
				t.Errorf("deepseek-flash context_window = %d, want %d", flash.ContextWindow, tt.wantContext)
			}
			if tt.wantCustomOutput != 0 {
				custom, ok := cfg.Pricing["custom-model"]
				if !ok {
					t.Fatal("custom-model pricing missing")
				}
				if custom.PeakOutputPerM != tt.wantCustomOutput {
					t.Errorf("custom-model peak_output_per_m = %g, want %g", custom.PeakOutputPerM, tt.wantCustomOutput)
				}
			}
			// Models not mentioned in the files keep their built-in pricing.
			if cfg.Pricing["deepseek-v4-pro"].PeakOutputPerM != 3.96 {
				t.Errorf("deepseek-v4-pro pricing was clobbered: %+v", cfg.Pricing["deepseek-v4-pro"])
			}
		})
	}
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func TestIsPeakUTC(t *testing.T) {
	// 2024-01-01 is a Monday; 2024-01-06 is a Saturday.
	monday := func(hour, min int) time.Time {
		return time.Date(2024, time.January, 1, hour, min, 0, 0, time.UTC)
	}
	saturday := func(hour, min int) time.Time {
		return time.Date(2024, time.January, 6, hour, min, 0, 0, time.UTC)
	}
	sunday := func(hour, min int) time.Time {
		return time.Date(2024, time.January, 7, hour, min, 0, 0, time.UTC)
	}

	tests := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"monday 00:00", monday(0, 0), false},
		{"monday 00:59", monday(0, 59), false},
		{"monday 01:00 starts window", monday(1, 0), true},
		{"monday 03:59 in window", monday(3, 59), true},
		{"monday 04:00 half-open end", monday(4, 0), false},
		{"monday 05:59 between windows", monday(5, 59), false},
		{"monday 06:00 starts window", monday(6, 0), true},
		{"monday 09:59 in window", monday(9, 59), true},
		{"monday 10:00 half-open end", monday(10, 0), false},
		{"friday 02:00", time.Date(2024, time.January, 5, 2, 0, 0, 0, time.UTC), true},
		{"friday 07:00", time.Date(2024, time.January, 5, 7, 0, 0, 0, time.UTC), true},
		{"saturday 02:00", saturday(2, 0), false},
		{"sunday 07:00", sunday(7, 0), false},
		{"non-UTC converted to UTC", time.Date(2024, time.January, 1, 2, 0, 0, 0, time.FixedZone("UTC+5", 5*3600)), false},
		{"non-UTC converted to peak UTC", time.Date(2024, time.January, 1, 8, 0, 0, 0, time.FixedZone("UTC+5", 5*3600)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPeakUTC(tt.t); got != tt.want {
				t.Errorf("IsPeakUTC(%v) = %v, want %v", tt.t, got, tt.want)
			}
		})
	}
}

func TestEstimateCost(t *testing.T) {
	cfg := Default()
	peak := time.Date(2024, time.January, 1, 2, 0, 0, 0, time.UTC)    // Monday 02:00 UTC
	offPeak := time.Date(2024, time.January, 6, 2, 0, 0, 0, time.UTC) // Saturday

	tests := []struct {
		name       string
		model      string
		cacheHit   int
		cacheMiss  int
		completion int
		now        time.Time
		want       float64
	}{
		{
			name: "flash off-peak one million of each", model: "deepseek-flash",
			cacheHit: 1_000_000, cacheMiss: 1_000_000, completion: 1_000_000,
			now: offPeak, want: 0.003 + 0.15 + 0.60,
		},
		{
			name: "flash peak one million of each", model: "deepseek-flash",
			cacheHit: 1_000_000, cacheMiss: 1_000_000, completion: 1_000_000,
			now: peak, want: 0.006 + 0.30 + 1.20,
		},
		{
			name: "flash peak partial tokens", model: "deepseek-flash",
			cacheHit: 500_000, cacheMiss: 250_000, completion: 100_000,
			now: peak, want: 0.5*0.006 + 0.25*0.30 + 0.1*1.20,
		},
		{
			name: "v4-pro peak one million of each", model: "deepseek-v4-pro",
			cacheHit: 1_000_000, cacheMiss: 1_000_000, completion: 1_000_000,
			now: peak, want: 0.044 + 1.32 + 3.96,
		},
		{
			name: "v4-pro off-peak one million of each", model: "deepseek-v4-pro",
			cacheHit: 1_000_000, cacheMiss: 1_000_000, completion: 1_000_000,
			now: offPeak, want: 0.022 + 0.66 + 1.98,
		},
		{
			name: "unknown model", model: "no-such-model",
			cacheHit: 1_000_000, cacheMiss: 1_000_000, completion: 1_000_000,
			now: peak, want: 0,
		},
		{
			name: "zero tokens", model: "deepseek-flash",
			now: peak, want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cfg.EstimateCost(tt.model, tt.cacheHit, tt.cacheMiss, tt.completion, tt.now)
			if math.Abs(got-tt.want) > 1e-12 {
				t.Errorf("EstimateCost = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("uses overridden pricing", func(t *testing.T) {
		cfg := Default()
		cfg.Pricing["custom"] = ModelPricing{
			OffPeakCacheHitPerM:  1,
			OffPeakCacheMissPerM: 2,
			OffPeakOutputPerM:    3,
		}
		got := cfg.EstimateCost("custom", 1_000_000, 1_000_000, 1_000_000, offPeak)
		if math.Abs(got-6) > 1e-12 {
			t.Errorf("EstimateCost = %v, want 6", got)
		}
	})
}

func TestAPIKeySaveLoad(t *testing.T) {
	clearConfigEnv(t)
	base := t.TempDir()
	configDir := filepath.Join(base, "nested", "config")

	if err := SaveAPIKey(configDir, "sk-test-123"); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}

	path := filepath.Join(configDir, CredentialsFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credentials: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("credentials permissions = %o, want 600", got)
		}
		dirInfo, err := os.Stat(configDir)
		if err != nil {
			t.Fatalf("stat config dir: %v", err)
		}
		if got := dirInfo.Mode().Perm(); got != 0o700 {
			t.Errorf("config dir permissions = %o, want 700", got)
		}
	}

	if got, err := LoadAPIKey(configDir); err != nil || got != "sk-test-123" {
		t.Fatalf("LoadAPIKey = %q, %v; want sk-test-123, nil", got, err)
	}

	// No temporary files should be left behind by the atomic write.
	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatalf("read config dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".credentials-") {
			t.Errorf("leftover temporary credentials file %q", entry.Name())
		}
	}

	// Overwrite atomically.
	if err := SaveAPIKey(configDir, "sk-test-456"); err != nil {
		t.Fatalf("SaveAPIKey overwrite: %v", err)
	}
	if got, err := LoadAPIKey(configDir); err != nil || got != "sk-test-456" {
		t.Fatalf("LoadAPIKey after overwrite = %q, %v; want sk-test-456, nil", got, err)
	}
}

func TestLoadAPIKeyPrecedence(t *testing.T) {
	clearConfigEnv(t)
	configDir := t.TempDir()
	if err := SaveAPIKey(configDir, "sk-file"); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}

	if got, err := LoadAPIKey(configDir); err != nil || got != "sk-file" {
		t.Fatalf("LoadAPIKey = %q, %v; want sk-file, nil", got, err)
	}

	t.Setenv(EnvAPIKey, "sk-env")
	if got, err := LoadAPIKey(configDir); err != nil || got != "sk-env" {
		t.Fatalf("LoadAPIKey with env = %q, %v; want sk-env, nil", got, err)
	}

	// Empty env value falls through to the file.
	t.Setenv(EnvAPIKey, "  ")
	if got, err := LoadAPIKey(configDir); err != nil || got != "sk-file" {
		t.Fatalf("LoadAPIKey with blank env = %q, %v; want sk-file, nil", got, err)
	}
}

func TestLoadAPIKeyMissingAndMalformed(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	if got, err := LoadAPIKey(filepath.Join(dir, "missing")); err != nil || got != "" {
		t.Fatalf("LoadAPIKey missing = %q, %v; want empty, nil", got, err)
	}

	writeFile(t, filepath.Join(dir, CredentialsFile), "api_key = [not toml")
	if _, err := LoadAPIKey(dir); err == nil {
		t.Fatal("LoadAPIKey malformed credentials succeeded, want error")
	}
}

func TestLoadReadsAPIKey(t *testing.T) {
	clearConfigEnv(t)
	configDir := t.TempDir()
	if err := SaveAPIKey(configDir, "sk-from-file"); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}

	cfg := loadForTest(t, configDir, t.TempDir(), nil)
	if cfg.APIKey != "sk-from-file" {
		t.Errorf("APIKey = %q, want sk-from-file", cfg.APIKey)
	}

	t.Setenv(EnvAPIKey, "sk-from-env")
	cfg = loadForTest(t, configDir, t.TempDir(), nil)
	if cfg.APIKey != "sk-from-env" {
		t.Errorf("APIKey = %q, want sk-from-env", cfg.APIKey)
	}
}

func TestFormatEffectiveRedactsAPIKey(t *testing.T) {
	clearConfigEnv(t)
	cfg := Default()
	cfg.APIKey = "sk-super-secret-value"

	out := cfg.FormatEffective()
	if strings.Contains(out, cfg.APIKey) {
		t.Fatalf("FormatEffective leaked API key:\n%s", out)
	}
	if !strings.Contains(out, `api_key = "[set]"`) {
		t.Errorf("FormatEffective missing redacted set marker:\n%s", out)
	}
	if !strings.Contains(out, `model = "deepseek-flash"`) {
		t.Errorf("FormatEffective missing model:\n%s", out)
	}
	if !strings.Contains(out, "[pricing.deepseek-flash]") {
		t.Errorf("FormatEffective missing pricing:\n%s", out)
	}

	var parsed Config
	if _, err := toml.Decode(out, &parsed); err != nil {
		t.Fatalf("FormatEffective output is not valid TOML: %v\n%s", err, out)
	}
	if parsed.Model != cfg.Model || parsed.MaxSteps != cfg.MaxSteps {
		t.Errorf("parsed effective config mismatch: %+v", parsed)
	}

	cfg.APIKey = ""
	out = cfg.FormatEffective()
	if !strings.Contains(out, `api_key = "[not set]"`) {
		t.Errorf("FormatEffective missing not-set marker:\n%s", out)
	}
	if strings.Contains(out, "sk-") {
		t.Errorf("FormatEffective leaked a key:\n%s", out)
	}
}

func TestConfigAPIKeyNeverMarshaled(t *testing.T) {
	clearConfigEnv(t)
	cfg := Default()
	cfg.APIKey = "sk-must-not-serialize"

	data, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatalf("toml.Marshal: %v", err)
	}
	if strings.Contains(string(data), cfg.APIKey) || strings.Contains(string(data), "api_key") {
		t.Fatalf("APIKey was serialized:\n%s", data)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid defaults", func(*Config) {}, ""},
		{"invalid reasoning effort", func(c *Config) { c.ReasoningEffort = "turbo" }, "reasoning_effort"},
		{"empty reasoning effort", func(c *Config) { c.ReasoningEffort = "" }, "reasoning_effort"},
		{"invalid permission mode", func(c *Config) { c.PermissionMode = "always" }, "permission_mode"},
		{"invalid web permission", func(c *Config) { c.WebPermission = "maybe" }, "web_permission"},
		{"invalid search backend", func(c *Config) { c.SearchBackend = "google" }, "search_backend"},
		{"empty model", func(c *Config) { c.Model = " " }, "model"},
		{"empty base url", func(c *Config) { c.BaseURL = "" }, "base_url"},
		{"zero max steps", func(c *Config) { c.MaxSteps = 0 }, "max_steps"},
		{"negative max steps", func(c *Config) { c.MaxSteps = -1 }, "max_steps"},
		{"negative max cost", func(c *Config) { c.MaxCostUSD = -0.01 }, "max_cost_usd"},
		{"zero max agents", func(c *Config) { c.MaxAgents = 0 }, "max_agents"},
		{"negative max agent depth", func(c *Config) { c.MaxAgentDepth = -1 }, "max_agent_depth"},
		{"zero threshold", func(c *Config) { c.AutoCompactThreshold = 0 }, "auto_compact_threshold"},
		{"threshold above one", func(c *Config) { c.AutoCompactThreshold = 1.01 }, "auto_compact_threshold"},
		{"negative pricing", func(c *Config) {
			p := c.Pricing["deepseek-flash"]
			p.PeakOutputPerM = -1
			c.Pricing["deepseek-flash"] = p
		}, "peak_output_per_m"},
		{"negative context window", func(c *Config) {
			p := c.Pricing["deepseek-flash"]
			p.ContextWindow = -1
			c.Pricing["deepseek-flash"] = p
		}, "context_window"},
	}

	validReasoning := map[string]bool{"none": true, "low": true, "high": true, "max": true}
	validPermission := map[string]bool{"ask": true, "auto-edit": true, "yolo": true, "plan": true}
	validWeb := map[string]bool{"ask": true, "allow": true, "deny": true}
	validSearch := map[string]bool{"brave": true, "tavily": true, "searxng": true, "duckduckgo": true}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate succeeded, want error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
			// Confirm the enum tables agree with the validator's accepted values.
			switch tt.wantErr {
			case "reasoning_effort":
				if validReasoning[cfg.ReasoningEffort] {
					t.Errorf("test fixture %q is actually valid", cfg.ReasoningEffort)
				}
			case "permission_mode":
				if validPermission[cfg.PermissionMode] {
					t.Errorf("test fixture %q is actually valid", cfg.PermissionMode)
				}
			case "web_permission":
				if validWeb[cfg.WebPermission] {
					t.Errorf("test fixture %q is actually valid", cfg.WebPermission)
				}
			case "search_backend":
				if validSearch[cfg.SearchBackend] {
					t.Errorf("test fixture %q is actually valid", cfg.SearchBackend)
				}
			}
		})
	}
}

func TestAllAllowedEnumsValidate(t *testing.T) {
	reasoning := []string{"none", "low", "high", "max"}
	permissions := []string{"ask", "auto-edit", "yolo", "plan"}
	web := []string{"ask", "allow", "deny"}
	search := []string{"brave", "tavily", "searxng", "duckduckgo"}

	for _, r := range reasoning {
		for _, p := range permissions {
			for _, w := range web {
				for _, s := range search {
					cfg := Default()
					cfg.ReasoningEffort = r
					cfg.PermissionMode = p
					cfg.WebPermission = w
					cfg.SearchBackend = s
					if err := cfg.Validate(); err != nil {
						t.Errorf("Validate(%s,%s,%s,%s): %v", r, p, w, s, err)
					}
				}
			}
		}
	}
}

func TestLoadRejectsInvalidEnums(t *testing.T) {
	clearConfigEnv(t)

	tests := []struct {
		name    string
		config  string
		env     map[string]string
		flags   *FlagOverrides
		wantErr string
	}{
		{
			name:    "invalid permission mode in config",
			config:  "permission_mode = \"always\"\n",
			wantErr: "permission_mode",
		},
		{
			name:    "invalid reasoning effort in env",
			env:     map[string]string{EnvReasoningEffort: "turbo"},
			wantErr: "reasoning_effort",
		},
		{
			name:    "invalid web permission via flag",
			flags:   &FlagOverrides{WebPermission: ptr("maybe")},
			wantErr: "web_permission",
		},
		{
			name:    "invalid search backend via flag",
			flags:   &FlagOverrides{SearchBackend: ptr("google")},
			wantErr: "search_backend",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := t.TempDir()
			if tt.config != "" {
				writeFile(t, filepath.Join(configDir, "config.toml"), tt.config)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := Load(LoadOptions{ConfigDir: configDir, WorkDir: t.TempDir(), Flags: tt.flags})
			if err == nil {
				t.Fatalf("Load succeeded, want error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigDir(t *testing.T) {
	clearConfigEnv(t)

	t.Run("TERN_CONFIG_DIR override", func(t *testing.T) {
		custom := filepath.Join(t.TempDir(), "custom")
		t.Setenv(EnvConfigDir, custom)
		got, err := ConfigDir()
		if err != nil {
			t.Fatalf("ConfigDir: %v", err)
		}
		if got != custom {
			t.Errorf("ConfigDir = %q, want %q", got, custom)
		}
	})

	if runtime.GOOS != "windows" {
		t.Run("XDG_CONFIG_HOME", func(t *testing.T) {
			xdg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", xdg)
			got, err := ConfigDir()
			if err != nil {
				t.Fatalf("ConfigDir: %v", err)
			}
			if want := filepath.Join(xdg, "tern"); got != want {
				t.Errorf("ConfigDir = %q, want %q", got, want)
			}
		})

		t.Run("home fallback", func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			got, err := ConfigDir()
			if err != nil {
				t.Fatalf("ConfigDir: %v", err)
			}
			if want := filepath.Join(home, ".config", "tern"); got != want {
				t.Errorf("ConfigDir = %q, want %q", got, want)
			}
		})
	}
}

func TestDataDir(t *testing.T) {
	clearConfigEnv(t)

	t.Run("TERN_DATA_DIR override", func(t *testing.T) {
		custom := filepath.Join(t.TempDir(), "data")
		t.Setenv(EnvDataDir, custom)
		got, err := DataDir()
		if err != nil {
			t.Fatalf("DataDir: %v", err)
		}
		if got != custom {
			t.Errorf("DataDir = %q, want %q", got, custom)
		}
	})

	if runtime.GOOS != "windows" {
		t.Run("XDG_DATA_HOME", func(t *testing.T) {
			xdg := t.TempDir()
			t.Setenv("XDG_DATA_HOME", xdg)
			got, err := DataDir()
			if err != nil {
				t.Fatalf("DataDir: %v", err)
			}
			if want := filepath.Join(xdg, "tern"); got != want {
				t.Errorf("DataDir = %q, want %q", got, want)
			}
		})

		t.Run("home fallback", func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			got, err := DataDir()
			if err != nil {
				t.Fatalf("DataDir: %v", err)
			}
			if want := filepath.Join(home, ".local", "share", "tern"); got != want {
				t.Errorf("DataDir = %q, want %q", got, want)
			}
		})
	}
}
