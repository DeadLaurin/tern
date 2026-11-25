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
	APIKey string `toml:"api_key"`
}

// LoadAPIKey returns the DeepSeek API key. The DEEPSEEK_API_KEY environment
// variable takes precedence over <configDir>/credentials.toml. A missing
// credentials file yields an empty key and no error.
func LoadAPIKey(configDir string) (string, error) {
	if key := strings.TrimSpace(os.Getenv(EnvAPIKey)); key != "" {
		return key, nil
	}

	path := filepath.Join(configDir, CredentialsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("config: read credentials %s: %w", path, err)
	}

	var creds credentials
	if _, err := toml.Decode(string(data), &creds); err != nil {
		return "", fmt.Errorf("config: decode credentials %s: %w", path, err)
	}
	return strings.TrimSpace(creds.APIKey), nil
}

// SaveAPIKey writes the API key to <configDir>/credentials.toml with 0600
// permissions. The directory is created with 0700 permissions if needed, and
// the file is written atomically via a temporary file and rename.
func SaveAPIKey(configDir, key string) error {
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
	if err := toml.NewEncoder(tmp).Encode(credentials{APIKey: key}); err != nil {
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

	path := filepath.Join(configDir, CredentialsFile)
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
