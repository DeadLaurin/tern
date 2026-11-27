// Package config implements Tern's layered configuration.
//
// Configuration is resolved in strict precedence order:
//
//	defaults < global config < project config < environment < CLI flags
//
// The global configuration lives in ConfigDir()/config.toml, the project
// configuration in <workdir>/.tern/config.toml. Model pricing is loaded from
// built-in defaults, then an optional pricing.toml next to the global config,
// then any [pricing.*] tables in the layered config.toml files.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Default values for Config fields.
const (
	DefaultModel                = "deepseek-flash"
	DefaultBaseURL              = "https://api.deepseek.com"
	DefaultReasoningEffort      = "high"
	DefaultPermissionMode       = "ask"
	DefaultWebPermission        = "ask"
	DefaultMaxSteps             = 250
	DefaultMaxCostUSD           = 0.0
	DefaultMaxAgents            = 4
	DefaultMaxAgentDepth        = 1
	DefaultAutoCompactThreshold = 0.78
	DefaultDebugCache           = false
	DefaultShell                = ""
	DefaultSearchBackend        = "duckduckgo"
)

// Environment variable names understood by Load. Every variable is the field's
// TOML name upper-cased and prefixed with TERN_.
const (
	EnvConfigDir            = "TERN_CONFIG_DIR"
	EnvDataDir              = "TERN_DATA_DIR"
	EnvModel                = "TERN_MODEL"
	EnvBaseURL              = "TERN_BASE_URL"
	EnvReasoningEffort      = "TERN_REASONING_EFFORT"
	EnvPermissionMode       = "TERN_PERMISSION_MODE"
	EnvWebPermission        = "TERN_WEB_PERMISSION"
	EnvMaxSteps             = "TERN_MAX_STEPS"
	EnvMaxCostUSD           = "TERN_MAX_COST_USD"
	EnvMaxAgents            = "TERN_MAX_AGENTS"
	EnvMaxAgentDepth        = "TERN_MAX_AGENT_DEPTH"
	EnvAutoCompactThreshold = "TERN_AUTO_COMPACT_THRESHOLD"
	EnvDebugCache           = "TERN_DEBUG_CACHE"
	EnvShell                = "TERN_SHELL"
	EnvSearchBackend        = "TERN_SEARCH_BACKEND"
	EnvSearchEndpoint       = "TERN_SEARCH_ENDPOINT"
	EnvProvider             = "TERN_PROVIDER"
	EnvGenericAPIKey        = "TERN_API_KEY"

	// EnvAPIKey is checked by LoadAPIKey before credentials.toml.
	EnvAPIKey = "DEEPSEEK_API_KEY"
)

// ModelPricing holds token prices in USD per million tokens together with the
// model's context limits. Off-peak and peak prices are kept separately because
// some providers (notably DeepSeek) discount off-peak usage.
type ModelPricing struct {
	ContextWindow  int  `toml:"context_window"`
	MaxOutput      int  `toml:"max_output"`
	SupportsVision bool `toml:"supports_vision"`

	OffPeakCacheHitPerM  float64 `toml:"off_peak_cache_hit_per_m"`
	OffPeakCacheMissPerM float64 `toml:"off_peak_cache_miss_per_m"`
	OffPeakOutputPerM    float64 `toml:"off_peak_output_per_m"`

	PeakCacheHitPerM  float64 `toml:"peak_cache_hit_per_m"`
	PeakCacheMissPerM float64 `toml:"peak_cache_miss_per_m"`
	PeakOutputPerM    float64 `toml:"peak_output_per_m"`
}

// Config is Tern's effective configuration.
type Config struct {
	Provider        string `toml:"provider,omitempty"`
	Model           string `toml:"model"`
	BaseURL         string `toml:"base_url"`
	ReasoningEffort string `toml:"reasoning_effort"`
	PermissionMode  string `toml:"permission_mode"`
	WebPermission   string `toml:"web_permission"`

	MaxSteps      int     `toml:"max_steps"`
	MaxCostUSD    float64 `toml:"max_cost_usd"`
	MaxAgents     int     `toml:"max_agents"`
	MaxAgentDepth int     `toml:"max_agent_depth"`

	AutoCompactThreshold float64 `toml:"auto_compact_threshold"`
	DebugCache           bool    `toml:"debug_cache"`

	Shell          string `toml:"shell"`
	SearchBackend  string `toml:"search_backend"`
	SearchEndpoint string `toml:"search_endpoint,omitempty"`

	Pricing map[string]ModelPricing `toml:"pricing"`

	// APIKey is never serialized or printed unredacted.
	APIKey string `toml:"-"`
}

// LoadOptions controls Load.
type LoadOptions struct {
	// WorkDir is the directory used to find <workdir>/.tern/config.toml.
	// Defaults to the current working directory.
	WorkDir string
	// ConfigDir overrides the global config directory. When empty, ConfigDir()
	// is used. Useful in tests.
	ConfigDir string
	// Flags are the highest-precedence overrides, normally parsed from CLI
	// flags. Nil fields are ignored.
	Flags *FlagOverrides
}

// FlagOverrides mirrors Config for CLI flags. Each non-nil field overrides the
// value resolved from lower-precedence layers.
type FlagOverrides struct {
	Provider             *string
	Model                *string
	BaseURL              *string
	ReasoningEffort      *string
	PermissionMode       *string
	WebPermission        *string
	MaxSteps             *int
	MaxCostUSD           *float64
	MaxAgents            *int
	MaxAgentDepth        *int
	AutoCompactThreshold *float64
	DebugCache           *bool
	Shell                *string
	SearchBackend        *string
	SearchEndpoint       *string
}

// Default returns a Config populated with built-in defaults.
func Default() *Config {
	return &Config{
		Provider:             InferProviderFromBaseURL(DefaultBaseURL),
		Model:                DefaultModel,
		BaseURL:              DefaultBaseURL,
		ReasoningEffort:      DefaultReasoningEffort,
		PermissionMode:       DefaultPermissionMode,
		WebPermission:        DefaultWebPermission,
		MaxSteps:             DefaultMaxSteps,
		MaxCostUSD:           DefaultMaxCostUSD,
		MaxAgents:            DefaultMaxAgents,
		MaxAgentDepth:        DefaultMaxAgentDepth,
		AutoCompactThreshold: DefaultAutoCompactThreshold,
		DebugCache:           DefaultDebugCache,
		Shell:                DefaultShell,
		SearchBackend:        DefaultSearchBackend,
		Pricing:              defaultPricing(),
	}
}

// defaultPricing returns a fresh copy of the built-in pricing table. A new map
// is returned on every call so callers can mutate their config safely.
func defaultPricing() map[string]ModelPricing {
	return map[string]ModelPricing{
		"deepseek-flash": {
			ContextWindow:  1_000_000,
			MaxOutput:      384_000,
			SupportsVision: true,

			OffPeakCacheHitPerM:  0.003,
			OffPeakCacheMissPerM: 0.15,
			OffPeakOutputPerM:    0.60,

			PeakCacheHitPerM:  0.006,
			PeakCacheMissPerM: 0.30,
			PeakOutputPerM:    1.20,
		},
		"deepseek-v4-pro": {
			ContextWindow:  1_000_000,
			MaxOutput:      384_000,
			SupportsVision: false,

			OffPeakCacheHitPerM:  0.022,
			OffPeakCacheMissPerM: 0.66,
			OffPeakOutputPerM:    1.98,

			PeakCacheHitPerM:  0.044,
			PeakCacheMissPerM: 1.32,
			PeakOutputPerM:    3.96,
		},
	}
}

// Load resolves the effective configuration.
func Load(opts LoadOptions) (*Config, error) {
	workDir := strings.TrimSpace(opts.WorkDir)
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("config: determine working directory: %w", err)
		}
		workDir = wd
	}

	configDir := strings.TrimSpace(opts.ConfigDir)
	if configDir == "" {
		dir, err := ConfigDir()
		if err != nil {
			return nil, err
		}
		configDir = dir
	}

	cfg := Default()

	// Track the precedence of explicit provider and base URL overrides so
	// provider presets only fill in values that were not deliberately set by a
	// user-facing layer with at least as much authority.
	providerPrecedence := 0
	baseURLPrecedence := 0

	// Pricing defaults < pricing.toml < global config.toml < project config.toml.
	if err := mergePricingFile(filepath.Join(configDir, "pricing.toml"), cfg.Pricing); err != nil {
		return nil, err
	}
	globalPath := filepath.Join(configDir, "config.toml")
	projectPath := filepath.Join(workDir, ".tern", "config.toml")
	globalRaw := readRawConfig(globalPath)
	projectRaw := readRawConfig(projectPath)
	if globalRaw.Provider != nil {
		providerPrecedence = 1
	}
	if projectRaw.Provider != nil {
		providerPrecedence = 2
	}
	if globalRaw.BaseURL != nil {
		baseURLPrecedence = 1
	}
	if projectRaw.BaseURL != nil {
		baseURLPrecedence = 2
	}

	if err := decodeInto(cfg, globalPath); err != nil {
		return nil, err
	}
	if err := decodeInto(cfg, projectPath); err != nil {
		return nil, err
	}

	if _, ok := lookupNonEmptyEnv(EnvProvider); ok {
		providerPrecedence = 3
	}
	if _, ok := lookupNonEmptyEnv(EnvBaseURL); ok {
		baseURLPrecedence = 3
	}
	if err := applyEnv(cfg); err != nil {
		return nil, err
	}
	if opts.Flags != nil {
		if opts.Flags.Provider != nil {
			providerPrecedence = 4
		}
		if opts.Flags.BaseURL != nil {
			baseURLPrecedence = 4
		}
	}
	applyFlags(cfg, opts.Flags)

	if providerPrecedence == 0 || strings.TrimSpace(cfg.Provider) == "" {
		cfg.Provider = InferProviderFromBaseURL(cfg.BaseURL)
	}
	if preset, ok := LookupProvider(cfg.Provider); ok {
		if baseURLPrecedence < providerPrecedence || cfg.BaseURL == DefaultBaseURL {
			cfg.BaseURL = preset.BaseURL
		}
		if cfg.Model == DefaultModel && preset.Name != "deepseek" {
			cfg.Model = preset.DefaultModel
		}
	}

	key, err := LoadAPIKeyForProvider(configDir, cfg.Provider)
	if err != nil {
		return nil, err
	}
	cfg.APIKey = key

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// rawConfig captures the fields whose presence matters for layering. A nil
// pointer means the file did not set the key.
type rawConfig struct {
	Provider *string `toml:"provider"`
	BaseURL  *string `toml:"base_url"`
}

// readRawConfig returns the layer-sensitive fields defined in path. Missing or
// malformed files yield a zero rawConfig; decodeInto reports any real error
// later.
func readRawConfig(path string) rawConfig {
	var raw rawConfig
	if _, err := os.Stat(path); err != nil {
		return raw
	}
	_, _ = toml.DecodeFile(path, &raw)
	return raw
}

// decodeInto decodes a TOML file into cfg, leaving fields absent from the file
// untouched. A missing file is not an error.
func decodeInto(cfg *Config, path string) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: stat %s: %w", path, err)
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return fmt.Errorf("config: decode %s: %w", path, err)
	}
	return nil
}

// mergePricingFile loads an optional pricing.toml. Both layouts are supported:
//
//	[pricing.deepseek-flash]
//	...
//
// and
//
//	[deepseek-flash]
//	...
//
// Entries merge into dst, replacing entries with the same model name.
func mergePricingFile(path string, dst map[string]ModelPricing) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	if dst == nil {
		dst = make(map[string]ModelPricing)
	}

	var wrapped struct {
		Pricing map[string]ModelPricing `toml:"pricing"`
	}
	if _, err := toml.Decode(string(data), &wrapped); err != nil {
		return fmt.Errorf("config: decode %s: %w", path, err)
	}
	if len(wrapped.Pricing) > 0 {
		for model, pricing := range wrapped.Pricing {
			dst[model] = pricing
		}
		return nil
	}

	var direct map[string]ModelPricing
	if _, err := toml.Decode(string(data), &direct); err != nil {
		return fmt.Errorf("config: decode %s: %w", path, err)
	}
	for model, pricing := range direct {
		dst[model] = pricing
	}
	return nil
}

func applyEnv(cfg *Config) error {
	setStringFromEnv(&cfg.Provider, EnvProvider)
	setStringFromEnv(&cfg.Model, EnvModel)
	setStringFromEnv(&cfg.BaseURL, EnvBaseURL)
	setStringFromEnv(&cfg.ReasoningEffort, EnvReasoningEffort)
	setStringFromEnv(&cfg.PermissionMode, EnvPermissionMode)
	setStringFromEnv(&cfg.WebPermission, EnvWebPermission)
	setStringFromEnv(&cfg.Shell, EnvShell)
	setStringFromEnv(&cfg.SearchBackend, EnvSearchBackend)
	setStringFromEnv(&cfg.SearchEndpoint, EnvSearchEndpoint)

	if err := setIntFromEnv(&cfg.MaxSteps, EnvMaxSteps); err != nil {
		return err
	}
	if err := setIntFromEnv(&cfg.MaxAgents, EnvMaxAgents); err != nil {
		return err
	}
	if err := setIntFromEnv(&cfg.MaxAgentDepth, EnvMaxAgentDepth); err != nil {
		return err
	}
	if err := setFloatFromEnv(&cfg.MaxCostUSD, EnvMaxCostUSD); err != nil {
		return err
	}
	if err := setFloatFromEnv(&cfg.AutoCompactThreshold, EnvAutoCompactThreshold); err != nil {
		return err
	}
	if err := setBoolFromEnv(&cfg.DebugCache, EnvDebugCache); err != nil {
		return err
	}
	return nil
}

func setStringFromEnv(dst *string, key string) {
	if v, ok := os.LookupEnv(key); ok {
		if v = strings.TrimSpace(v); v != "" {
			*dst = v
		}
	}
}

func setIntFromEnv(dst *int, key string) error {
	v, ok := lookupNonEmptyEnv(key)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("config: invalid %s (%s=%q): expected integer: %w", key, envFieldName(key), v, err)
	}
	*dst = n
	return nil
}

func setFloatFromEnv(dst *float64, key string) error {
	v, ok := lookupNonEmptyEnv(key)
	if !ok {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fmt.Errorf("config: invalid %s (%s=%q): expected number: %w", key, envFieldName(key), v, err)
	}
	*dst = f
	return nil
}

func setBoolFromEnv(dst *bool, key string) error {
	v, ok := lookupNonEmptyEnv(key)
	if !ok {
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("config: invalid %s (%s=%q): expected boolean: %w", key, envFieldName(key), v, err)
	}
	*dst = b
	return nil
}

func lookupNonEmptyEnv(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}

// envFieldName maps TERN_MAX_STEPS to max_steps for error messages.
func envFieldName(key string) string {
	return strings.ToLower(strings.TrimPrefix(key, "TERN_"))
}

func applyFlags(cfg *Config, flags *FlagOverrides) {
	if flags == nil {
		return
	}
	if flags.Provider != nil {
		cfg.Provider = *flags.Provider
	}
	if flags.Model != nil {
		cfg.Model = *flags.Model
	}
	if flags.BaseURL != nil {
		cfg.BaseURL = *flags.BaseURL
	}
	if flags.ReasoningEffort != nil {
		cfg.ReasoningEffort = *flags.ReasoningEffort
	}
	if flags.PermissionMode != nil {
		cfg.PermissionMode = *flags.PermissionMode
	}
	if flags.WebPermission != nil {
		cfg.WebPermission = *flags.WebPermission
	}
	if flags.MaxSteps != nil {
		cfg.MaxSteps = *flags.MaxSteps
	}
	if flags.MaxCostUSD != nil {
		cfg.MaxCostUSD = *flags.MaxCostUSD
	}
	if flags.MaxAgents != nil {
		cfg.MaxAgents = *flags.MaxAgents
	}
	if flags.MaxAgentDepth != nil {
		cfg.MaxAgentDepth = *flags.MaxAgentDepth
	}
	if flags.AutoCompactThreshold != nil {
		cfg.AutoCompactThreshold = *flags.AutoCompactThreshold
	}
	if flags.DebugCache != nil {
		cfg.DebugCache = *flags.DebugCache
	}
	if flags.Shell != nil {
		cfg.Shell = *flags.Shell
	}
	if flags.SearchBackend != nil {
		cfg.SearchBackend = *flags.SearchBackend
	}
	if flags.SearchEndpoint != nil {
		cfg.SearchEndpoint = *flags.SearchEndpoint
	}
}

var (
	validReasoningEfforts = []string{"none", "low", "high", "max"}
	validPermissionModes  = []string{"ask", "auto-edit", "yolo", "plan"}
	validWebPermissions   = []string{"ask", "allow", "deny"}
	validSearchBackends   = []string{"brave", "tavily", "searxng", "duckduckgo"}
)

// Validate checks enum fields and numeric ranges. It should be called after
// all layers have been merged.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config: nil config")
	}
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("config: model must not be empty")
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("config: base_url must not be empty")
	}
	if err := validateEnum("reasoning_effort", c.ReasoningEffort, validReasoningEfforts); err != nil {
		return err
	}
	if err := validateEnum("permission_mode", c.PermissionMode, validPermissionModes); err != nil {
		return err
	}
	if err := validateEnum("web_permission", c.WebPermission, validWebPermissions); err != nil {
		return err
	}
	if err := validateEnum("search_backend", c.SearchBackend, validSearchBackends); err != nil {
		return err
	}
	if c.MaxSteps <= 0 {
		return fmt.Errorf("config: max_steps must be greater than 0, got %d", c.MaxSteps)
	}
	if c.MaxCostUSD < 0 {
		return fmt.Errorf("config: max_cost_usd must not be negative, got %g", c.MaxCostUSD)
	}
	if c.MaxAgents < 1 {
		return fmt.Errorf("config: max_agents must be at least 1, got %d", c.MaxAgents)
	}
	if c.MaxAgentDepth < 0 {
		return fmt.Errorf("config: max_agent_depth must not be negative, got %d", c.MaxAgentDepth)
	}
	if c.AutoCompactThreshold <= 0 || c.AutoCompactThreshold > 1 {
		return fmt.Errorf("config: auto_compact_threshold must be in (0, 1], got %g", c.AutoCompactThreshold)
	}
	for model, pricing := range c.Pricing {
		if pricing.ContextWindow < 0 {
			return fmt.Errorf("config: pricing.%s.context_window must not be negative, got %d", model, pricing.ContextWindow)
		}
		if pricing.MaxOutput < 0 {
			return fmt.Errorf("config: pricing.%s.max_output must not be negative, got %d", model, pricing.MaxOutput)
		}
		for name, value := range map[string]float64{
			"off_peak_cache_hit_per_m":  pricing.OffPeakCacheHitPerM,
			"off_peak_cache_miss_per_m": pricing.OffPeakCacheMissPerM,
			"off_peak_output_per_m":     pricing.OffPeakOutputPerM,
			"peak_cache_hit_per_m":      pricing.PeakCacheHitPerM,
			"peak_cache_miss_per_m":     pricing.PeakCacheMissPerM,
			"peak_output_per_m":         pricing.PeakOutputPerM,
		} {
			if value < 0 {
				return fmt.Errorf("config: pricing.%s.%s must not be negative, got %g", model, name, value)
			}
		}
	}
	return nil
}

func validateEnum(field, value string, allowed []string) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("config: invalid %s %q (allowed: %s)", field, value, strings.Join(allowed, ", "))
}
