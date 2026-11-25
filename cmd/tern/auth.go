package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/tern-agent/tern/internal/config"
)

// ensureAPIKey loads the DeepSeek API key, prompting exactly once on an
// interactive terminal when none is configured. Non-interactive sessions get a
// clear, actionable error instead of hanging on a hidden prompt.
func ensureAPIKey(in io.Reader, out, errOut io.Writer, stdinTTY bool) (string, error) {
	configDir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	key, err := config.LoadAPIKey(configDir)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(key) != "" {
		return key, nil
	}

	if !stdinTTY {
		return "", errors.New("DEEPSEEK_API_KEY is not set and no stored credentials were found; set DEEPSEEK_API_KEY or run `tern` interactively once to save a key")
	}

	fmt.Fprint(out, "Enter DEEPSEEK_API_KEY: ")
	key, err = readPassword(in)
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("read API key: %w", err)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("API key must not be empty")
	}
	if err := config.SaveAPIKey(configDir, key); err != nil {
		return "", err
	}
	if errOut != nil {
		fmt.Fprintf(errOut, "tern: saved API key to %s\n", configDir)
	}
	return key, nil
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
