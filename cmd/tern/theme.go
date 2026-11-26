package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/theme"
)

func printThemeUsage(w io.Writer) {
	fmt.Fprintln(w, `tern theme - manage the local CSS-based design system

Usage:
  tern theme [check|status] [--json]   show theme files, schema and warnings
  tern theme init [--light] [--local]  create theme.css from the default template
  tern theme path                      print the global theme.css path
  tern theme reset [--light] [--local] overwrite theme.css with a fresh template

The global theme lives in the tern config directory; --local selects
<workdir>/.tern/theme.css instead. Project tokens override global tokens.
Customizations live in your own file and survive tern updates.

Flags:
      --light   use the light default palette (init/reset)
      --local   act on the project theme (init/reset)
      --force   overwrite an existing theme.css (init)
      --json    emit status as JSON (check/status)
  -h, --help    show this help`)
}

type themeStatus struct {
	GlobalPath           string   `json:"global_path"`
	ProjectPath          string   `json:"project_path"`
	Files                []string `json:"files"`
	SchemaVersion        int      `json:"schema_version"`
	CurrentSchemaVersion int      `json:"current_schema_version"`
	Custom               bool     `json:"custom"`
	CustomTokens         int      `json:"custom_tokens"`
	MissingTokens        []string `json:"missing_tokens,omitempty"`
	Warnings             []string `json:"warnings,omitempty"`
}

// runThemeCommand implements `tern theme ...`.
func runThemeCommand(args []string, o runOptions) int {
	sub := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "help", "-h", "--help":
		printThemeUsage(o.Out)
		return 0
	case "", "check", "status":
		return runThemeStatus(strings.TrimSpace(sub), args, o)
	case "init":
		return runThemeInit(args, o)
	case "path":
		return runThemePath(args, o)
	case "reset":
		return runThemeReset(args, o)
	default:
		fmt.Fprintf(o.Err, "tern theme: unknown subcommand %q\n\n", sub)
		printThemeUsage(o.Err)
		return 2
	}
}

func runThemeStatus(sub string, args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern theme "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonOut, help bool
	fs.BoolVar(&jsonOut, "json", false, "emit status as JSON")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(o.Err, "tern theme: %v\n", err)
		return 2
	}
	if help {
		printThemeUsage(o.Out)
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(o.Err, "tern theme: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	workDir, err := commandWorkDir(o)
	if err != nil {
		fmt.Fprintf(o.Err, "tern theme: determine working directory: %v\n", err)
		return 1
	}
	configDir, err := config.ConfigDir()
	if err != nil {
		fmt.Fprintf(o.Err, "tern theme: %v\n", err)
		return 1
	}
	loaded, err := theme.Load(workDir, configDir)
	if err != nil {
		fmt.Fprintf(o.Err, "tern theme: %v\n", err)
		return 1
	}

	status := themeStatus{
		GlobalPath:           filepath.Join(configDir, theme.ThemeFileName),
		ProjectPath:          filepath.Join(workDir, ".tern", theme.ThemeFileName),
		Files:                loaded.Files,
		SchemaVersion:        loaded.SchemaVersion,
		CurrentSchemaVersion: theme.CurrentSchemaVersion,
		Custom:               loaded.HasCustom(),
		CustomTokens:         len(loaded.Tokens),
		MissingTokens:        loaded.MissingTokens,
		Warnings:             loaded.Warnings,
	}
	if status.Files == nil {
		status.Files = []string{}
	}

	if jsonOut {
		enc := json.NewEncoder(o.Out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(status); err != nil {
			fmt.Fprintf(o.Err, "tern theme: encode status: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(o.Out, "global theme:  %s%s\n", status.GlobalPath, missingSuffix(status.GlobalPath))
	fmt.Fprintf(o.Out, "project theme: %s%s\n", status.ProjectPath, missingSuffix(status.ProjectPath))
	if !status.Custom {
		fmt.Fprintln(o.Out, "using built-in defaults (no custom theme.css found)")
	} else {
		fmt.Fprintf(o.Out, "loaded files:  %s\n", strings.Join(status.Files, ", "))
	}
	fmt.Fprintf(o.Out, "schema version: %d (current %d)\n", status.SchemaVersion, status.CurrentSchemaVersion)
	fmt.Fprintf(o.Out, "custom tokens:  %d\n", status.CustomTokens)
	if len(status.MissingTokens) > 0 {
		fmt.Fprintf(o.Out, "missing tokens: %s\n", strings.Join(status.MissingTokens, ", "))
	}
	if len(status.Warnings) > 0 {
		fmt.Fprintln(o.Out, "warnings:")
		for _, warning := range status.Warnings {
			fmt.Fprintf(o.Out, "  - %s\n", warning)
		}
	}
	return 0
}

func missingSuffix(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return " (not found)"
	}
	return ""
}

func runThemeInit(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern theme init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var light, local, force, help bool
	fs.BoolVar(&light, "light", false, "use the light default palette")
	fs.BoolVar(&local, "local", false, "create the project theme")
	fs.BoolVar(&force, "force", false, "overwrite an existing theme.css")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(o.Err, "tern theme init: %v\n", err)
		return 2
	}
	if help {
		printThemeUsage(o.Out)
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(o.Err, "tern theme init: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	path, err := themeTargetPath(local, o)
	if err != nil {
		fmt.Fprintf(o.Err, "tern theme init: %v\n", err)
		return 1
	}
	base := themeBase(light)
	if err := theme.InitFile(path, base, force); err != nil {
		fmt.Fprintf(o.Err, "tern theme init: %v\n", err)
		return 1
	}
	fmt.Fprintf(o.Out, "created %s (%s theme)\n", path, base)
	fmt.Fprintln(o.Out, "This file is yours: edits survive tern updates. Run `tern theme check` after upgrading.")
	return 0
}

func runThemePath(args []string, o runOptions) int {
	if len(args) > 0 {
		fmt.Fprintf(o.Err, "tern theme path: unexpected argument %q\n", args[0])
		return 2
	}
	configDir, err := config.ConfigDir()
	if err != nil {
		fmt.Fprintf(o.Err, "tern theme path: %v\n", err)
		return 1
	}
	fmt.Fprintln(o.Out, filepath.Join(configDir, theme.ThemeFileName))
	return 0
}

func runThemeReset(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern theme reset", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var light, local, help bool
	fs.BoolVar(&light, "light", false, "use the light default palette")
	fs.BoolVar(&local, "local", false, "reset the project theme")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(o.Err, "tern theme reset: %v\n", err)
		return 2
	}
	if help {
		printThemeUsage(o.Out)
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(o.Err, "tern theme reset: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	path, err := themeTargetPath(local, o)
	if err != nil {
		fmt.Fprintf(o.Err, "tern theme reset: %v\n", err)
		return 1
	}
	base := themeBase(light)
	if err := theme.InitFile(path, base, true); err != nil {
		fmt.Fprintf(o.Err, "tern theme reset: %v\n", err)
		return 1
	}
	fmt.Fprintf(o.Out, "reset %s (%s theme)\n", path, base)
	return 0
}

func themeBase(light bool) string {
	if light {
		return "light"
	}
	return "dark"
}

func themeTargetPath(local bool, o runOptions) (string, error) {
	if local {
		workDir, err := commandWorkDir(o)
		if err != nil {
			return "", err
		}
		return filepath.Join(workDir, ".tern", theme.ThemeFileName), nil
	}
	configDir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, theme.ThemeFileName), nil
}
