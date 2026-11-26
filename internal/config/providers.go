package config

import "strings"

// ProviderPreset describes a built-in model provider.
type ProviderPreset struct {
	Name         string
	DisplayName  string
	BaseURL      string
	EnvVar       string
	DefaultModel string
	RequiresKey  bool
	ExtraHeaders map[string]string
}

// BuiltinProviders returns the built-in provider presets. A fresh slice and
// fresh ExtraHeaders maps are returned on every call so callers can mutate the
// result safely.
func BuiltinProviders() []ProviderPreset {
	presets := []ProviderPreset{
		{
			Name:         "deepseek",
			DisplayName:  "DeepSeek",
			BaseURL:      "https://api.deepseek.com",
			EnvVar:       "DEEPSEEK_API_KEY",
			DefaultModel: "deepseek-flash",
			RequiresKey:  true,
		},
		{
			Name:         "openrouter",
			DisplayName:  "OpenRouter",
			BaseURL:      "https://openrouter.ai/api/v1",
			EnvVar:       "OPENROUTER_API_KEY",
			DefaultModel: "anthropic/claude-sonnet-4",
			RequiresKey:  true,
			ExtraHeaders: map[string]string{
				"HTTP-Referer": "https://tern.disgusting.work",
				"X-Title":      "tern",
			},
		},
		{
			Name:         "openai",
			DisplayName:  "OpenAI",
			BaseURL:      "https://api.openai.com/v1",
			EnvVar:       "OPENAI_API_KEY",
			DefaultModel: "gpt-4.1",
			RequiresKey:  true,
		},
		{
			Name:         "groq",
			DisplayName:  "Groq",
			BaseURL:      "https://api.groq.com/openai/v1",
			EnvVar:       "GROQ_API_KEY",
			DefaultModel: "llama-3.3-70b-versatile",
			RequiresKey:  true,
		},
		{
			Name:         "xai",
			DisplayName:  "xAI",
			BaseURL:      "https://api.x.ai/v1",
			EnvVar:       "XAI_API_KEY",
			DefaultModel: "grok-3",
			RequiresKey:  true,
		},
		{
			Name:         "mistral",
			DisplayName:  "Mistral",
			BaseURL:      "https://api.mistral.ai/v1",
			EnvVar:       "MISTRAL_API_KEY",
			DefaultModel: "mistral-large-latest",
			RequiresKey:  true,
		},
		{
			Name:         "together",
			DisplayName:  "Together AI",
			BaseURL:      "https://api.together.xyz/v1",
			EnvVar:       "TOGETHER_API_KEY",
			DefaultModel: "meta-llama/Llama-3.3-70B-Instruct-Turbo",
			RequiresKey:  true,
		},
		{
			Name:         "ollama",
			DisplayName:  "Ollama",
			BaseURL:      "http://localhost:11434/v1",
			EnvVar:       "",
			DefaultModel: "qwen2.5-coder:14b",
			RequiresKey:  false,
		},
		{
			Name:         "lmstudio",
			DisplayName:  "LM Studio",
			BaseURL:      "http://localhost:1234/v1",
			EnvVar:       "",
			DefaultModel: "local-model",
			RequiresKey:  false,
		},
	}

	for i := range presets {
		if presets[i].ExtraHeaders != nil {
			headers := make(map[string]string, len(presets[i].ExtraHeaders))
			for k, v := range presets[i].ExtraHeaders {
				headers[k] = v
			}
			presets[i].ExtraHeaders = headers
		}
	}
	return presets
}

// LookupProvider finds a preset by name, case-insensitively.
func LookupProvider(name string) (ProviderPreset, bool) {
	name = strings.TrimSpace(name)
	for _, preset := range BuiltinProviders() {
		if strings.EqualFold(preset.Name, name) {
			return preset, true
		}
	}
	return ProviderPreset{}, false
}

// InferProviderFromBaseURL maps a provider base URL to a preset name. An empty
// base URL maps to the default provider, and an unrecognised non-empty base
// URL maps to "custom".
func InferProviderFromBaseURL(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return "deepseek"
	}
	lower := strings.ToLower(trimmed)

	switch {
	case strings.Contains(lower, "openrouter.ai"):
		return "openrouter"
	case strings.Contains(lower, "api.openai.com"):
		return "openai"
	case strings.Contains(lower, "api.groq.com"):
		return "groq"
	case strings.Contains(lower, "api.x.ai"):
		return "xai"
	case strings.Contains(lower, "api.mistral.ai"):
		return "mistral"
	case strings.Contains(lower, "api.together.xyz"):
		return "together"
	case strings.Contains(lower, "api.deepseek.com"):
		return "deepseek"
	case isLoopbackURL(lower) && strings.Contains(lower, ":11434"):
		return "ollama"
	case isLoopbackURL(lower) && strings.Contains(lower, ":1234"):
		return "lmstudio"
	default:
		return "custom"
	}
}

func isLoopbackURL(lower string) bool {
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]", "0.0.0.0"} {
		if strings.Contains(lower, host) {
			return true
		}
	}
	return false
}
