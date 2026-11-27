package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// clearProviderEnv isolates provider and per-provider credential tests from the
// developer's real environment.
func clearProviderEnv(t *testing.T) {
	t.Helper()
	keys := []string{EnvProvider, EnvGenericAPIKey, EnvAPIKey}
	for _, preset := range BuiltinProviders() {
		if preset.EnvVar != "" {
			keys = append(keys, preset.EnvVar)
		}
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}
}

func TestBuiltinProviders(t *testing.T) {
	want := []ProviderPreset{
		{Name: "deepseek", BaseURL: "https://api.deepseek.com", EnvVar: "DEEPSEEK_API_KEY", DefaultModel: "deepseek-flash", RequiresKey: true},
		{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", EnvVar: "OPENROUTER_API_KEY", DefaultModel: "anthropic/claude-sonnet-4", RequiresKey: true, FreeTier: true},
		{Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", EnvVar: "GEMINI_API_KEY", DefaultModel: "gemini-2.5-flash", RequiresKey: true, FreeTier: true},
		{Name: "groq", BaseURL: "https://api.groq.com/openai/v1", EnvVar: "GROQ_API_KEY", DefaultModel: "llama-3.3-70b-versatile", RequiresKey: true, FreeTier: true},
		{Name: "openai", BaseURL: "https://api.openai.com/v1", EnvVar: "OPENAI_API_KEY", DefaultModel: "gpt-4.1", RequiresKey: true},
		{Name: "xai", BaseURL: "https://api.x.ai/v1", EnvVar: "XAI_API_KEY", DefaultModel: "grok-3", RequiresKey: true},
		{Name: "mistral", BaseURL: "https://api.mistral.ai/v1", EnvVar: "MISTRAL_API_KEY", DefaultModel: "mistral-large-latest", RequiresKey: true},
		{Name: "together", BaseURL: "https://api.together.xyz/v1", EnvVar: "TOGETHER_API_KEY", DefaultModel: "meta-llama/Llama-3.3-70B-Instruct-Turbo", RequiresKey: true},
		{Name: "ollama", BaseURL: "http://localhost:11434/v1", EnvVar: "", DefaultModel: "qwen2.5-coder:14b", RequiresKey: false},
		{Name: "lmstudio", BaseURL: "http://localhost:1234/v1", EnvVar: "", DefaultModel: "local-model", RequiresKey: false},
	}

	got := BuiltinProviders()
	if len(got) != len(want) {
		t.Fatalf("BuiltinProviders() returned %d presets, want %d", len(got), len(want))
	}
	for i, preset := range got {
		w := want[i]
		if preset.Name != w.Name || preset.BaseURL != w.BaseURL || preset.EnvVar != w.EnvVar ||
			preset.DefaultModel != w.DefaultModel || preset.RequiresKey != w.RequiresKey {
			t.Errorf("preset %d = %+v, want %+v", i, preset, w)
		}
		if preset.DisplayName == "" {
			t.Errorf("preset %q has empty DisplayName", preset.Name)
		}
	}
}

func TestBuiltinProvidersIsolation(t *testing.T) {
	openrouter, ok := LookupProvider("openrouter")
	if !ok {
		t.Fatal("missing openrouter preset")
	}
	if openrouter.ExtraHeaders["HTTP-Referer"] != "https://tern.disgusting.work" {
		t.Errorf("HTTP-Referer = %q", openrouter.ExtraHeaders["HTTP-Referer"])
	}
	if openrouter.ExtraHeaders["X-Title"] != "tern" {
		t.Errorf("X-Title = %q", openrouter.ExtraHeaders["X-Title"])
	}

	openrouter.ExtraHeaders["X-Title"] = "mutated"
	again, _ := LookupProvider("openrouter")
	if again.ExtraHeaders["X-Title"] != "tern" {
		t.Error("BuiltinProviders must return fresh ExtraHeaders maps")
	}
}

func TestLookupProvider(t *testing.T) {
	for _, name := range []string{"openrouter", "OpenRouter", "OPENROUTER", " openrouter "} {
		preset, ok := LookupProvider(name)
		if !ok || preset.Name != "openrouter" {
			t.Errorf("LookupProvider(%q) = %+v, %v; want openrouter", name, preset, ok)
		}
	}
	if _, ok := LookupProvider("not-a-provider"); ok {
		t.Error("LookupProvider found an unknown provider")
	}
}

func TestInferProviderFromBaseURL(t *testing.T) {
	tests := []struct {
		baseURL string
		want    string
	}{
		{"", "deepseek"},
		{"   ", "deepseek"},
		{"https://api.deepseek.com", "deepseek"},
		{"https://openrouter.ai/api/v1", "openrouter"},
		{"https://api.openai.com/v1", "openai"},
		{"https://api.groq.com/openai/v1", "groq"},
		{"https://api.x.ai/v1", "xai"},
		{"https://api.mistral.ai/v1", "mistral"},
		{"https://api.together.xyz/v1", "together"},
		{"http://localhost:11434/v1", "ollama"},
		{"http://127.0.0.1:11434/v1", "ollama"},
		{"http://localhost:1234/v1", "lmstudio"},
		{"https://example.com/v1", "custom"},
	}
	for _, tt := range tests {
		if got := InferProviderFromBaseURL(tt.baseURL); got != tt.want {
			t.Errorf("InferProviderFromBaseURL(%q) = %q, want %q", tt.baseURL, got, tt.want)
		}
	}
}

func TestLoadProviderLayering(t *testing.T) {
	clearConfigEnv(t)
	clearProviderEnv(t)

	t.Run("env selects openrouter preset", func(t *testing.T) {
		t.Setenv(EnvProvider, "openrouter")
		cfg := loadForTest(t, t.TempDir(), t.TempDir(), nil)
		if cfg.Provider != "openrouter" {
			t.Errorf("provider = %q, want openrouter", cfg.Provider)
		}
		if cfg.BaseURL != "https://openrouter.ai/api/v1" {
			t.Errorf("base_url = %q, want openrouter URL", cfg.BaseURL)
		}
		if cfg.Model != "anthropic/claude-sonnet-4" {
			t.Errorf("model = %q, want openrouter default model", cfg.Model)
		}
	})

	t.Run("project over global and env over project and flags over env", func(t *testing.T) {
		configDir := t.TempDir()
		workDir := t.TempDir()
		writeFile(t, filepath.Join(configDir, "config.toml"), "provider = \"deepseek\"\n")
		writeFile(t, filepath.Join(workDir, ".tern", "config.toml"), "provider = \"openrouter\"\n")

		cfg := loadForTest(t, configDir, workDir, nil)
		if cfg.Provider != "openrouter" || cfg.BaseURL != "https://openrouter.ai/api/v1" {
			t.Errorf("project provider = %q, base_url = %q; want openrouter preset", cfg.Provider, cfg.BaseURL)
		}

		t.Setenv(EnvProvider, "groq")
		cfg = loadForTest(t, configDir, workDir, nil)
		if cfg.Provider != "groq" || cfg.BaseURL != "https://api.groq.com/openai/v1" {
			t.Errorf("env provider = %q, base_url = %q; want groq preset", cfg.Provider, cfg.BaseURL)
		}

		cfg = loadForTest(t, configDir, workDir, &FlagOverrides{Provider: ptr("openai")})
		if cfg.Provider != "openai" || cfg.BaseURL != "https://api.openai.com/v1" || cfg.Model != "gpt-4.1" {
			t.Errorf("flag provider = %q, base_url = %q, model = %q; want openai preset", cfg.Provider, cfg.BaseURL, cfg.Model)
		}
	})

	t.Run("explicit base_url and model are preserved", func(t *testing.T) {
		configDir := t.TempDir()
		writeFile(t, filepath.Join(configDir, "config.toml"), `
provider = "openrouter"
base_url = "https://proxy.example/v1"
model = "my-model"
`)
		cfg := loadForTest(t, configDir, t.TempDir(), nil)
		if cfg.BaseURL != "https://proxy.example/v1" {
			t.Errorf("base_url = %q, want explicit proxy URL", cfg.BaseURL)
		}
		if cfg.Model != "my-model" {
			t.Errorf("model = %q, want explicit model", cfg.Model)
		}
	})

	t.Run("provider inferred from base_url", func(t *testing.T) {
		configDir := t.TempDir()
		writeFile(t, filepath.Join(configDir, "config.toml"), "base_url = \"https://api.openai.com/v1\"\n")
		cfg := loadForTest(t, configDir, t.TempDir(), nil)
		if cfg.Provider != "openai" {
			t.Errorf("provider = %q, want inferred openai", cfg.Provider)
		}
		if cfg.Model != "gpt-4.1" {
			t.Errorf("model = %q, want openai default model", cfg.Model)
		}
	})
}

func TestProviderCredentialsIsolation(t *testing.T) {
	clearConfigEnv(t)
	clearProviderEnv(t)

	configDir := t.TempDir()
	if err := SaveAPIKeyForProvider(configDir, "openrouter", "sk-or"); err != nil {
		t.Fatalf("SaveAPIKeyForProvider(openrouter): %v", err)
	}
	if err := SaveAPIKeyForProvider(configDir, "groq", "sk-groq"); err != nil {
		t.Fatalf("SaveAPIKeyForProvider(groq): %v", err)
	}
	if err := SaveAPIKeyForProvider(configDir, "deepseek", "sk-ds"); err != nil {
		t.Fatalf("SaveAPIKeyForProvider(deepseek): %v", err)
	}

	checks := map[string]string{
		"openrouter": "sk-or",
		"groq":       "sk-groq",
		"deepseek":   "sk-ds",
		"missing":    "",
	}
	for provider, want := range checks {
		got, err := LoadAPIKeyForProvider(configDir, provider)
		if err != nil {
			t.Fatalf("LoadAPIKeyForProvider(%s): %v", provider, err)
		}
		if got != want {
			t.Errorf("LoadAPIKeyForProvider(%s) = %q, want %q", provider, got, want)
		}
	}

	// Legacy functions still address the DeepSeek key.
	if got, err := LoadAPIKey(configDir); err != nil || got != "sk-ds" {
		t.Errorf("LoadAPIKey = %q, %v; want sk-ds", got, err)
	}
	if err := SaveAPIKey(configDir, "sk-ds-legacy"); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}
	if got, err := LoadAPIKey(configDir); err != nil || got != "sk-ds-legacy" {
		t.Errorf("LoadAPIKey after save = %q, %v; want sk-ds-legacy", got, err)
	}
	if got, err := LoadAPIKeyForProvider(configDir, "deepseek"); err != nil || got != "sk-ds-legacy" {
		t.Errorf("deepseek key after legacy save = %q, %v; want sk-ds-legacy", got, err)
	}
	// Legacy saves must not wipe provider-specific keys.
	if got, err := LoadAPIKeyForProvider(configDir, "openrouter"); err != nil || got != "sk-or" {
		t.Errorf("openrouter key after legacy save = %q, %v; want sk-or", got, err)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(configDir, CredentialsFile))
		if err != nil {
			t.Fatalf("stat credentials: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("credentials permissions = %o, want 600", got)
		}
	}
}

func TestLoadAPIKeyForProviderNestedTable(t *testing.T) {
	clearConfigEnv(t)
	clearProviderEnv(t)

	configDir := t.TempDir()
	writeFile(t, filepath.Join(configDir, CredentialsFile), "[providers.openrouter]\napi_key = \"sk-nested\"\n")
	if got, err := LoadAPIKeyForProvider(configDir, "openrouter"); err != nil || got != "sk-nested" {
		t.Fatalf("LoadAPIKeyForProvider = %q, %v; want sk-nested", got, err)
	}

	// Saving another provider preserves the nested entry.
	if err := SaveAPIKeyForProvider(configDir, "groq", "sk-groq"); err != nil {
		t.Fatalf("SaveAPIKeyForProvider(groq): %v", err)
	}
	if got, err := LoadAPIKeyForProvider(configDir, "openrouter"); err != nil || got != "sk-nested" {
		t.Errorf("openrouter after save = %q, %v; want sk-nested", got, err)
	}
	if got, err := LoadAPIKeyForProvider(configDir, "groq"); err != nil || got != "sk-groq" {
		t.Errorf("groq after save = %q, %v; want sk-groq", got, err)
	}
}

func TestLoadAPIKeyForProviderPrecedence(t *testing.T) {
	clearConfigEnv(t)
	clearProviderEnv(t)

	configDir := t.TempDir()
	if err := SaveAPIKeyForProvider(configDir, "openrouter", "sk-or-file"); err != nil {
		t.Fatalf("SaveAPIKeyForProvider: %v", err)
	}
	if err := SaveAPIKey(configDir, "sk-ds-file"); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}

	t.Run("preset env over generic env and file", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "sk-or-env")
		t.Setenv(EnvGenericAPIKey, "sk-generic")
		got, err := LoadAPIKeyForProvider(configDir, "openrouter")
		if err != nil || got != "sk-or-env" {
			t.Errorf("openrouter = %q, %v; want sk-or-env", got, err)
		}
	})

	t.Run("generic env over file", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "")
		t.Setenv(EnvGenericAPIKey, "sk-generic")
		got, err := LoadAPIKeyForProvider(configDir, "openrouter")
		if err != nil || got != "sk-generic" {
			t.Errorf("openrouter = %q, %v; want sk-generic", got, err)
		}
	})

	t.Run("file fallback", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "")
		t.Setenv(EnvGenericAPIKey, "")
		got, err := LoadAPIKeyForProvider(configDir, "openrouter")
		if err != nil || got != "sk-or-file" {
			t.Errorf("openrouter = %q, %v; want sk-or-file", got, err)
		}
	})

	t.Run("deepseek preset env over generic env", func(t *testing.T) {
		t.Setenv(EnvAPIKey, "sk-ds-env")
		t.Setenv(EnvGenericAPIKey, "sk-generic")
		got, err := LoadAPIKeyForProvider(configDir, "deepseek")
		if err != nil || got != "sk-ds-env" {
			t.Errorf("deepseek = %q, %v; want sk-ds-env", got, err)
		}
	})

	t.Run("generic env when deepseek env unset", func(t *testing.T) {
		t.Setenv(EnvAPIKey, "")
		t.Setenv(EnvGenericAPIKey, "sk-generic")
		got, err := LoadAPIKeyForProvider(configDir, "deepseek")
		if err != nil || got != "sk-generic" {
			t.Errorf("deepseek = %q, %v; want sk-generic", got, err)
		}
	})

	t.Run("deepseek file fallback", func(t *testing.T) {
		t.Setenv(EnvAPIKey, "")
		t.Setenv(EnvGenericAPIKey, "")
		got, err := LoadAPIKeyForProvider(configDir, "deepseek")
		if err != nil || got != "sk-ds-file" {
			t.Errorf("deepseek = %q, %v; want sk-ds-file", got, err)
		}
	})

	t.Run("unknown provider uses generic env", func(t *testing.T) {
		t.Setenv(EnvGenericAPIKey, "sk-custom")
		got, err := LoadAPIKeyForProvider(configDir, "custom")
		if err != nil || got != "sk-custom" {
			t.Errorf("custom = %q, %v; want sk-custom", got, err)
		}
	})
}

func TestLoadUsesProviderAPIKey(t *testing.T) {
	clearConfigEnv(t)
	clearProviderEnv(t)

	configDir := t.TempDir()
	if err := SaveAPIKeyForProvider(configDir, "openrouter", "sk-or-config"); err != nil {
		t.Fatalf("SaveAPIKeyForProvider: %v", err)
	}
	t.Setenv(EnvProvider, "openrouter")
	cfg := loadForTest(t, configDir, t.TempDir(), nil)
	if cfg.APIKey != "sk-or-config" {
		t.Errorf("APIKey = %q, want sk-or-config", cfg.APIKey)
	}

	t.Setenv("OPENROUTER_API_KEY", "sk-or-env")
	cfg = loadForTest(t, configDir, t.TempDir(), nil)
	if cfg.APIKey != "sk-or-env" {
		t.Errorf("APIKey = %q, want sk-or-env", cfg.APIKey)
	}

	// The provider is included in serialized output.
	if !strings.Contains(cfg.FormatEffective(), `provider = "openrouter"`) {
		t.Errorf("FormatEffective missing provider:\n%s", cfg.FormatEffective())
	}
}
