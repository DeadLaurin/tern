package tui

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"time"
)

// clipboardTimeout bounds how long the TUI will wait for a system clipboard
// helper. A stuck helper must never freeze the event loop, so copy commands are
// given a short deadline and otherwise treated as unavailable.
const clipboardTimeout = 2 * time.Second

// clipboardCandidates lists the helper commands used on each platform, in
// preference order. Linux has no single standard, so Wayland and both X11
// clipboard tools are tried before giving up.
type clipboardCommand struct {
	name string
	args []string
}

func clipboardCommands(goos string) []clipboardCommand {
	switch goos {
	case "darwin":
		return []clipboardCommand{{name: "pbcopy"}}
	case "windows":
		return []clipboardCommand{{name: "clip"}}
	default:
		return []clipboardCommand{
			{name: "wl-copy"},
			{name: "xclip", args: []string{"-selection", "clipboard"}},
			{name: "xsel", args: []string{"--clipboard", "--input"}},
		}
	}
}

// copyToClipboard writes text to the host clipboard using the platform's
// native helper. It returns an error when no helper is installed or every
// candidate fails, letting the caller surface a useful status message.
func copyToClipboard(text string) error {
	return copyToClipboardWithGOOS(text, runtime.GOOS)
}

func copyToClipboardWithGOOS(text, goos string) error {
	var lastErr error
	for _, candidate := range clipboardCommands(goos) {
		path, err := exec.LookPath(candidate.name)
		if err != nil {
			lastErr = err
			continue
		}
		if err := runClipboardCommand(path, candidate.args, text); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no clipboard helper available")
	}
	return fmt.Errorf("clipboard: %w", lastErr)
}

func runClipboardCommand(path string, args []string, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if _, err := io.WriteString(stdin, text); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return err
	}
	if err := stdin.Close(); err != nil {
		_ = cmd.Wait()
		return err
	}
	return cmd.Wait()
}
