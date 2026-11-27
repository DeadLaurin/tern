package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/provider"
)

// defaultLocalBaseURL is the OpenAI-compatible endpoint exposed by a default
// Ollama installation.
const defaultLocalBaseURL = "http://localhost:11434/v1"

// isLoopbackBaseURL reports whether rawURL targets a loopback host. Local
// OpenAI-compatible servers (Ollama, LM Studio, vLLM, llama.cpp) usually run
// without authentication, so tern must not demand DEEPSEEK_API_KEY for them.
func isLoopbackBaseURL(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// ensureAPIKey loads the DeepSeek API key, prompting once on an interactive
// terminal when none is configured.
func ensureAPIKey(in io.Reader, out, errOut io.Writer, stdinTTY bool) (string, error) {
	return ensureAPIKeyForProvider("deepseek", in, out, errOut, stdinTTY)
}

// ensureAPIKeyForProvider loads the API key for providerName, prompting once
// on an interactive terminal when none is configured.
func ensureAPIKeyForProvider(providerName string, in io.Reader, out, errOut io.Writer, stdinTTY bool) (string, error) {
	configDir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	key, err := config.LoadAPIKeyForProvider(configDir, providerName)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(key) != "" {
		return key, nil
	}

	envName := config.EnvAPIKey
	if preset, ok := config.LookupProvider(providerName); ok && preset.EnvVar != "" {
		envName = preset.EnvVar
	} else if providerName != "" && !strings.EqualFold(providerName, "deepseek") {
		envName = config.EnvGenericAPIKey
	}

	if !stdinTTY {
		return "", fmt.Errorf("%s is not set and no stored credentials were found for %q; set %s or run `tern` interactively once to save a key", envName, providerName, envName)
	}

	fmt.Fprintf(out, "Enter %s: ", envName)
	key, err = readPassword(in)
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("read API key: %w", err)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("API key must not be empty")
	}
	if err := config.SaveAPIKeyForProvider(configDir, providerName, key); err != nil {
		return "", err
	}
	if errOut != nil {
		fmt.Fprintf(errOut, "tern: saved %s API key to %s\n", providerName, configDir)
	}
	return key, nil
}

// providerNeedsNoKey reports whether cfg targets a provider that does not
// require a hosted API key (loopback servers, free public endpoints, or local
// CLI-agent backends like Claude Code, Codex, and Antigravity).
func providerNeedsNoKey(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if isLoopbackBaseURL(cfg.BaseURL) {
		return true
	}
	if preset, ok := config.LookupProvider(cfg.Provider); ok && !preset.RequiresKey {
		return true
	}
	return false
}

// newProviderClient builds an HTTP streaming client configured for cfg's
// provider preset (base URL, API key, and provider-specific headers such as
// OpenRouter attribution headers).
func newProviderClient(cfg *config.Config) provider.Provider {
	var extraHeaders map[string]string
	if preset, ok := config.LookupProvider(cfg.Provider); ok && len(preset.ExtraHeaders) > 0 {
		extraHeaders = preset.ExtraHeaders
	}
	return provider.NewOpenAICompatClient(cfg.Provider, cfg.BaseURL, cfg.APIKey, extraHeaders)
}

func readPassword(in io.Reader) (string, error) {
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		data, err := term.ReadPassword(int(f.Fd()))
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return line, nil
}
