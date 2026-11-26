package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tern-agent/tern/internal/config"
)

type uninstallTarget struct {
	Path string
	Kind string
}

type uninstallOptions struct {
	Yes      bool
	Purge    bool
	DryRun   bool
	In       io.Reader
	Out      io.Writer
	Err      io.Writer
	StdinTTY bool
	// Executable overrides the running binary path. Empty means use
	// os.Executable (or the test-injected executablePath).
	Executable string
}

func printUninstallUsage(w io.Writer) {
	fmt.Fprintln(w, `tern uninstall - remove the tern binary and optionally its data

Usage:
  tern uninstall [flags]

Flags:
  -y, --yes       skip the confirmation prompt
      --purge     also remove config and session data
      --all       alias for --purge
      --dry-run   show what would be removed without deleting anything
  -h, --help      show this help`)
}

func runUninstallCommand(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern uninstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var yes, purge, all, dryRun, help bool
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	fs.BoolVar(&yes, "y", false, "skip the confirmation prompt")
	fs.BoolVar(&purge, "purge", false, "also remove config and session data")
	fs.BoolVar(&all, "all", false, "alias for --purge")
	fs.BoolVar(&dryRun, "dry-run", false, "show what would be removed without deleting anything")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUninstallUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern uninstall: %v\n", err)
		return 2
	}
	if help {
		printUninstallUsage(o.Out)
		return 0
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(o.Err, "tern uninstall: unexpected argument %q\n", rest[0])
		return 2
	}

	err := performUninstall(uninstallOptions{
		Yes:      yes,
		Purge:    purge || all,
		DryRun:   dryRun,
		In:       o.In,
		Out:      o.Out,
		Err:      o.Err,
		StdinTTY: o.StdinTTY,
	})
	if err != nil {
		fmt.Fprintf(o.Err, "tern uninstall: %v\n", err)
		return 1
	}
	return 0
}

func performUninstall(o uninstallOptions) error {
	if o.In == nil {
		o.In = strings.NewReader("")
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}

	targets, configDir, dataDir, err := collectUninstallTargets(o.Executable, o.Purge)
	if err != nil {
		return err
	}

	fmt.Fprintln(o.Out, "tern uninstall will remove:")
	if len(targets) == 0 {
		fmt.Fprintln(o.Out, "  (nothing found)")
	}
	for _, target := range targets {
		if target.Kind == "" {
			fmt.Fprintf(o.Out, "  %s\n", target.Path)
			continue
		}
		fmt.Fprintf(o.Out, "  %s (%s)\n", target.Path, target.Kind)
	}

	if o.DryRun {
		fmt.Fprintln(o.Out)
		fmt.Fprintln(o.Out, "Dry run: no changes were made.")
		return nil
	}

	if !o.Yes {
		if !o.StdinTTY {
			return errors.New("refusing to uninstall without confirmation; re-run with --yes (or -y)")
		}
		fmt.Fprint(o.Out, "Uninstall tern? [y/N]: ")
		reader := bufio.NewReader(o.In)
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read confirmation: %w", readErr)
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(o.Out, "Uninstall cancelled.")
			return nil
		}
	}

	for _, target := range targets {
		if err := removeUninstallTarget(target, o.Out); err != nil {
			return err
		}
	}

	if !o.Purge {
		fmt.Fprintln(o.Out)
		fmt.Fprintln(o.Out, "Config and sessions were kept so nothing is lost unexpectedly:")
		fmt.Fprintf(o.Out, "  config:   %s\n", configDir)
		fmt.Fprintf(o.Out, "  sessions: %s\n", dataDir)
		fmt.Fprintln(o.Out, "Run 'tern uninstall --purge' to remove them too.")
	}
	return nil
}

func collectUninstallTargets(execPath string, purge bool) ([]uninstallTarget, string, string, error) {
	exePath := strings.TrimSpace(execPath)
	if exePath == "" {
		exe, err := executablePath()
		if err != nil {
			return nil, "", "", fmt.Errorf("locate current executable: %w", err)
		}
		exePath = exe
	}
	exePath = filepath.Clean(exePath)

	resolved := exePath
	if eval, err := filepath.EvalSymlinks(exePath); err == nil {
		resolved = filepath.Clean(eval)
	}

	var targets []uninstallTarget
	seen := make(map[string]bool)
	add := func(path, kind string) {
		if strings.TrimSpace(path) == "" {
			return
		}
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		seen[path] = true
		targets = append(targets, uninstallTarget{Path: path, Kind: kind})
	}

	// Record the symlink and its target so removing the link does not leave
	// the real binary behind.
	isSymlink := false
	if info, err := os.Lstat(exePath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		isSymlink = true
	}
	if isSymlink {
		add(exePath, "symlink")
		add(resolved, "binary")
	} else {
		add(exePath, "binary")
	}

	// Also catch common install locations when they point at the same binary.
	if home, err := os.UserHomeDir(); err == nil {
		binaryName := "tern"
		if runtime.GOOS == "windows" {
			binaryName = "tern.exe"
		}
		for _, rel := range []string{filepath.Join(".local", "bin", binaryName), filepath.Join("go", "bin", binaryName)} {
			candidate := filepath.Join(home, rel)
			if _, err := os.Lstat(candidate); err != nil {
				continue
			}
			candidateResolved, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				continue
			}
			if !sameBinary(candidateResolved, resolved) {
				continue
			}
			if filepath.Clean(candidateResolved) == filepath.Clean(resolved) {
				// Resolve links before the real binary so the link is removed
				// first.
				if filepath.Clean(candidate) != filepath.Clean(resolved) {
					add(candidate, "symlink")
				}
				continue
			}
			add(candidate, "binary")
		}
	}

	configDir, err := config.ConfigDir()
	if err != nil {
		return nil, "", "", err
	}
	dataDir, err := config.DataDir()
	if err != nil {
		return nil, "", "", err
	}

	if purge {
		if _, err := os.Lstat(configDir); err == nil {
			add(configDir, "config")
		}
		if _, err := os.Lstat(dataDir); err == nil {
			add(dataDir, "sessions")
		}
	}

	return targets, configDir, dataDir, nil
}

func sameBinary(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

func removeUninstallTarget(target uninstallTarget, out io.Writer) error {
	switch target.Kind {
	case "config", "sessions":
		if err := os.RemoveAll(target.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", target.Path, err)
		}
	default:
		note, err := removeBinaryPath(target.Path)
		if err != nil {
			return err
		}
		if note != "" {
			fmt.Fprintln(out, note)
		} else {
			fmt.Fprintf(out, "Removed %s\n", target.Path)
		}
		return nil
	}
	fmt.Fprintf(out, "Removed %s\n", target.Path)
	return nil
}

func removeBinaryPath(path string) (string, error) {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	// Windows cannot unlink a running executable. Rename it out of the way and
	// try to clean it up, otherwise tell the user where it went.
	if runtime.GOOS == "windows" {
		backup := path + ".old"
		_ = os.Remove(backup)
		if renameErr := os.Rename(path, backup); renameErr == nil {
			if os.Remove(backup) == nil {
				return "", nil
			}
			return fmt.Sprintf("Renamed running binary to %s; it can be deleted after tern exits.", backup), nil
		}
	}
	return "", fmt.Errorf("remove %s: %w", path, err)
}
