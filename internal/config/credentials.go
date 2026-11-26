package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// CredentialsFile is the file name used for the stored API key.
const CredentialsFile = "credentials.toml"

type credentials struct {
	APIKey    string            `toml:"api_key,omitempty"`
	Providers map[string]string `toml:"providers,omitempty"`
}

// LoadAPIKey returns the DeepSeek API key. The DEEPSEEK_API_KEY environment
// variable takes precedence over <configDir>/credentials.toml. A missing
// credentials file yields an empty key and no error.
func LoadAPIKey(configDir string) (string, error) {
	return LoadAPIKeyForProvider(configDir, "deepseek")
}

// LoadAPIKeyForProvider resolves the API key for providerName. Precedence:
//
//  1. the provider preset's environment variable (for example
//     OPENROUTER_API_KEY or DEEPSEEK_API_KEY),
//  2. TERN_API_KEY, as a fallback for non-DeepSeek providers and when
//     DEEPSEEK_API_KEY is unset,
//  3. <configDir>/credentials.toml.
//
// A missing credentials file yields an empty key and no error.
func LoadAPIKeyForProvider(configDir, providerName string) (string, error) {
	name := normalizeProviderName(providerName)

	if preset, ok := LookupProvider(name); ok && preset.EnvVar != "" {
		if key := strings.TrimSpace(os.Getenv(preset.EnvVar)); key != "" {
			return key, nil
		}
	}

	if name != "deepseek" || strings.TrimSpace(os.Getenv(EnvAPIKey)) == "" {
		if key := strings.TrimSpace(os.Getenv(EnvGenericAPIKey)); key != "" {
			return key, nil
		}
	}

	creds, err := loadCredentials(configDir)
	if err != nil {
		return "", err
	}
	if key := strings.TrimSpace(creds.Providers[name]); key != "" {
		return key, nil
	}
	if name == "deepseek" {
		return strings.TrimSpace(creds.APIKey), nil
	}
	return "", nil
}

// SaveAPIKey writes the DeepSeek API key to <configDir>/credentials.toml with
// 0600 permissions. Existing keys for other providers are preserved. The
// directory is created with 0700 permissions if needed, and the file is written
// atomically via a temporary file and rename.
func SaveAPIKey(configDir, key string) error {
	creds, err := loadCredentials(configDir)
	if err != nil {
		// Preserve the historical behaviour of always overwriting the
		// credentials file with the supplied key, even if it was malformed.
		creds = credentials{}
	}
	if creds.Providers == nil {
		creds.Providers = make(map[string]string)
	}
	creds.APIKey = key
	creds.Providers["deepseek"] = key
	return saveCredentials(configDir, creds)
}

// SaveAPIKeyForProvider writes key for providerName to
// <configDir>/credentials.toml with 0600 permissions, preserving existing keys
// for other providers. The DeepSeek key is also mirrored to the legacy
// top-level api_key field.
func SaveAPIKeyForProvider(configDir, providerName, key string) error {
	name := normalizeProviderName(providerName)
	if name == "" {
		return errors.New("config: provider name must not be empty")
	}

	creds, err := loadCredentials(configDir)
	if err != nil {
		return err
	}
	if creds.Providers == nil {
		creds.Providers = make(map[string]string)
	}
	creds.Providers[name] = key
	if name == "deepseek" {
		creds.APIKey = key
	}
	return saveCredentials(configDir, creds)
}

// SaveGlobalProviderSelection persists the user's chosen provider, model, and
// optional custom base URL to <configDir>/config.toml while preserving all
// other settings in that file.
func SaveGlobalProviderSelection(configDir, providerName, baseURL, model string) error {
	if strings.TrimSpace(configDir) == "" {
		return errors.New("config: config dir must not be empty")
	}
	path := filepath.Join(configDir, "config.toml")
	raw := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil {
		_, _ = toml.Decode(string(data), &raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("config: read %s: %w", path, err)
	}

	name := normalizeProviderName(providerName)
	if name != "" {
		raw["provider"] = name
	}
	if m := strings.TrimSpace(model); m != "" {
		raw["model"] = m
	}
	baseURL = strings.TrimSpace(baseURL)
	if preset, ok := LookupProvider(name); ok && baseURL == preset.BaseURL {
		delete(raw, "base_url")
	} else if baseURL != "" {
		raw["base_url"] = baseURL
	}

	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("config: create %s: %w", configDir, err)
	}
	tmp, err := os.CreateTemp(configDir, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("config: create temp config: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("config: chmod config: %w", err)
	}
	if err := toml.NewEncoder(tmp).Encode(raw); err != nil {
		cleanup()
		return fmt.Errorf("config: encode %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("config: close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("config: install %s: %w", path, err)
	}
	return nil
}

func normalizeProviderName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func credentialsPath(configDir string) string {
	return filepath.Join(configDir, CredentialsFile)
}

func loadCredentials(configDir string) (credentials, error) {
	path := credentialsPath(configDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credentials{}, nil
		}
		return credentials{}, fmt.Errorf("config: read credentials %s: %w", path, err)
	}

	creds, err := parseCredentials(data)
	if err != nil {
		return credentials{}, fmt.Errorf("config: decode credentials %s: %w", path, err)
	}
	return creds, nil
}

// parseCredentials accepts both the flat providers layout:
//
//	[providers]
//	openrouter = "sk-..."
//
// and the nested table layout:
//
//	[providers.openrouter]
//	api_key = "sk-..."
func parseCredentials(data []byte) (credentials, error) {
	var creds credentials
	if _, err := toml.Decode(string(data), &creds); err == nil {
		return creds, nil
	}

	var nested struct {
		APIKey    string `toml:"api_key"`
		Providers map[string]struct {
			APIKey string `toml:"api_key"`
		} `toml:"providers"`
	}
	if _, err := toml.Decode(string(data), &nested); err != nil {
		return credentials{}, err
	}
	creds.APIKey = nested.APIKey
	if len(nested.Providers) > 0 {
		creds.Providers = make(map[string]string, len(nested.Providers))
		for name, entry := range nested.Providers {
			if entry.APIKey != "" {
				creds.Providers[name] = entry.APIKey
			}
		}
	}
	return creds, nil
}

func saveCredentials(configDir string, creds credentials) error {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("config: create config directory %s: %w", configDir, err)
	}
	// Tighten permissions even if the directory already existed.
	if err := os.Chmod(configDir, 0o700); err != nil {
		return fmt.Errorf("config: secure config directory %s: %w", configDir, err)
	}

	tmp, err := os.CreateTemp(configDir, ".credentials-*.toml")
	if err != nil {
		return fmt.Errorf("config: create temporary credentials file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("config: secure temporary credentials file: %w", err)
	}
	if err := toml.NewEncoder(tmp).Encode(creds); err != nil {
		cleanup()
		return fmt.Errorf("config: encode credentials: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("config: sync credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("config: close credentials: %w", err)
	}

	path := credentialsPath(configDir)
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("config: write credentials %s: %w", path, err)
	}
	// Rename preserves the temporary file's 0600 mode, but enforce it in case
	// the platform's rename semantics differ.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("config: secure credentials %s: %w", path, err)
	}
	return nil
}
