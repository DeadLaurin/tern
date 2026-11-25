package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ConfigDir returns Tern's global configuration directory.
//
// Resolution order:
//   - TERN_CONFIG_DIR
//   - Windows: os.UserConfigDir()/tern (normally %APPDATA%/tern)
//   - Unix:   $XDG_CONFIG_HOME/tern, else ~/.config/tern
func ConfigDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(EnvConfigDir)); dir != "" {
		return dir, nil
	}

	if runtime.GOOS == "windows" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("config: determine user config directory: %w", err)
		}
		return filepath.Join(base, "tern"), nil
	}

	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return filepath.Join(dir, "tern"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: determine home directory: %w", err)
	}
	return filepath.Join(home, ".config", "tern"), nil
}

// DataDir returns Tern's data directory.
//
// Resolution order:
//   - TERN_DATA_DIR
//   - Windows: %APPDATA%/tern, else os.UserConfigDir()/tern
//   - Unix:   $XDG_DATA_HOME/tern, else ~/.local/share/tern
func DataDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(EnvDataDir)); dir != "" {
		return dir, nil
	}

	if runtime.GOOS == "windows" {
		base := strings.TrimSpace(os.Getenv("APPDATA"))
		if base == "" {
			var err error
			base, err = os.UserConfigDir()
			if err != nil {
				return "", fmt.Errorf("config: determine user data directory: %w", err)
			}
		}
		return filepath.Join(base, "tern"), nil
	}

	if dir := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dir != "" {
		return filepath.Join(dir, "tern"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: determine home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "tern"), nil
}
