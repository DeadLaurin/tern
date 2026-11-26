package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/ext"
)

// pluginListEntry is the JSON/text shape of one plugin in `tern plugin list`.
type pluginListEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Source      string `json:"source"`
	Enabled     bool   `json:"enabled"`
	Description string `json:"description,omitempty"`
	Dir         string `json:"dir"`
	Tools       int    `json:"tools"`
	Hooks       int    `json:"hooks"`
	MCP         int    `json:"mcp_servers"`
}

// extListEntry is kept as an alias for backwards-compatible callers/tests.
type extListEntry = pluginListEntry

func printPluginUsage(w io.Writer) {
	fmt.Fprintln(w, `tern plugin - manage plugins (formerly extensions)

Usage:
  tern plugin [list] [--json]
  tern plugin new <name> [--local] [--description "..."]
  tern plugin install <path-or-git-url>
  tern plugin remove <name>
  tern plugin guide

Subcommands:
  list        list discovered plugins (default)
  new         scaffold a new plugin
  install     install a plugin from a local directory or git URL
  remove      remove an installed plugin from the global config
                (aliases: rm, uninstall)
  guide       print the plugin.toml specification (alias: schema)

Flags:
      --json                  emit the plugin list as JSON (list only)
      --local                 scaffold into ./.tern/plugins (new only)
      --description "..."     plugin description (new only)
  -h, --help                  show this help

Plugin locations:
  ~/.config/tern/plugins/<name>   global plugins (primary)
  ./.tern/plugins/<name>          project plugins (primary)
  ~/.config/tern/extensions/<name> and ./.tern/extensions/<name> are still
  read as legacy fallbacks.`)
}

// printExtUsage is the legacy name for the plugin usage text.
func printExtUsage(w io.Writer) { printPluginUsage(w) }

// runPluginCommand dispatches `tern plugin ...` (and the legacy `tern ext ...`).
func runPluginCommand(args []string, o runOptions) int {
	if len(args) == 0 {
		return runPluginList(nil, o)
	}
	switch args[0] {
	case "list", "ls":
		return runPluginList(args[1:], o)
	case "new", "init", "create":
		return runPluginNew(args[1:], o)
	case "install", "add":
		return runPluginInstall(args[1:], o)
	case "remove", "uninstall", "rm":
		return runPluginRemove(args[1:], o)
	case "guide", "schema":
		return runPluginGuide(args[1:], o)
	case "help", "-h", "--help":
		printPluginUsage(o.Out)
		return 0
	default:
		fmt.Fprintf(o.Err, "tern plugin: unknown subcommand %q\n", args[0])
		printPluginUsage(o.Err)
		return 2
	}
}

func pluginConfigDir(o runOptions, errPrefix string) (string, bool) {
	configDir, err := config.ConfigDir()
	if err != nil {
		fmt.Fprintf(o.Err, "%s: %v\n", errPrefix, err)
		return "", false
	}
	return configDir, true
}

func pluginWorkDir(o runOptions) string {
	workDir := strings.TrimSpace(o.WorkDir)
	if workDir == "" {
		if wd, err := os.Getwd(); err == nil {
			workDir = wd
		}
	}
	return workDir
}

func runPluginList(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern plugin list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonOut, help bool
	fs.BoolVar(&jsonOut, "json", false, "emit the plugin list as JSON")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printPluginUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern plugin list: %v\n", err)
		return 2
	}
	if help {
		printPluginUsage(o.Out)
		return 0
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(o.Err, "tern plugin list: unexpected argument %q\n", rest[0])
		return 2
	}

	configDir, ok := pluginConfigDir(o, "tern plugin list")
	if !ok {
		return 1
	}

	plugins, warnings := ext.ListAllExtensions(pluginWorkDir(o), configDir)
	for _, warning := range warnings {
		fmt.Fprintf(o.Err, "tern plugin list: warning: %s\n", warning)
	}

	entries := make([]pluginListEntry, 0, len(plugins))
	for _, p := range plugins {
		if p == nil {
			continue
		}
		entries = append(entries, pluginListEntry{
			Name:        p.Manifest.Name,
			Version:     p.Manifest.Version,
			Source:      p.Source,
			Enabled:     p.Manifest.IsEnabled(),
			Description: p.Manifest.Description,
			Dir:         p.Dir,
			Tools:       len(p.Manifest.Tools),
			Hooks:       len(p.Manifest.Hooks),
			MCP:         len(p.Manifest.MCP),
		})
	}

	if jsonOut {
		enc := json.NewEncoder(o.Out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(entries); err != nil {
			fmt.Fprintf(o.Err, "tern plugin list: %v\n", err)
			return 1
		}
		return 0
	}

	if len(entries) == 0 {
		fmt.Fprintln(o.Out, "No plugins installed.")
		fmt.Fprintln(o.Out, "Create one with: tern plugin new my-plugin")
		return 0
	}
	tw := tabwriter.NewWriter(o.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tSOURCE\tSTATUS\tTOOLS\tHOOKS\tMCP\tDESCRIPTION")
	for _, e := range entries {
		status := "disabled"
		if e.Enabled {
			status = "enabled"
		}
		version := e.Version
		if version == "" {
			version = "-"
		}
		desc := e.Description
		if desc == "" {
			desc = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			e.Name, version, e.Source, status, e.Tools, e.Hooks, e.MCP, desc)
	}
	_ = tw.Flush()
	return 0
}

func runPluginNew(args []string, o runOptions) int {
	// Flags may appear before or after the plugin name, which the standard
	// flag package does not support, so parse manually.
	var (
		local       bool
		description string
		rest        []string
	)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			printPluginUsage(o.Out)
			return 0
		case arg == "--local" || arg == "-local":
			local = true
		case arg == "--description" || arg == "-description":
			if i+1 >= len(args) {
				fmt.Fprintln(o.Err, "tern plugin new: --description requires a value")
				return 2
			}
			i++
			description = args[i]
		case strings.HasPrefix(arg, "--description="):
			description = strings.TrimPrefix(arg, "--description=")
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(o.Err, "tern plugin new: unknown flag %q\n", arg)
			return 2
		default:
			rest = append(rest, arg)
		}
	}
	if len(rest) == 0 {
		fmt.Fprintln(o.Err, "tern plugin new: a plugin name is required")
		return 2
	}
	if len(rest) > 1 {
		fmt.Fprintf(o.Err, "tern plugin new: unexpected argument %q\n", rest[1])
		return 2
	}

	var parentDir string
	if local {
		workDir := pluginWorkDir(o)
		if workDir == "" {
			fmt.Fprintln(o.Err, "tern plugin new: cannot determine working directory for --local")
			return 1
		}
		parentDir = filepath.Join(workDir, filepath.FromSlash(ext.ProjectPluginsDir))
	} else {
		configDir, ok := pluginConfigDir(o, "tern plugin new")
		if !ok {
			return 1
		}
		parentDir = filepath.Join(configDir, ext.PluginsDirName)
	}

	dir, err := ext.ScaffoldPlugin(parentDir, strings.TrimSpace(rest[0]), description)
	if err != nil {
		fmt.Fprintf(o.Err, "tern plugin new: %v\n", err)
		return 1
	}
	fmt.Fprintf(o.Out, "Created plugin %q\n", strings.TrimSpace(rest[0]))
	for _, name := range []string{ext.PluginManifestFile, "tool.sh", "README.md"} {
		fmt.Fprintf(o.Out, "  %s\n", filepath.Join(dir, name))
	}
	fmt.Fprintln(o.Out, "Next: tern plugin list   (then ask the agent to call the tool)")
	return 0
}

func runPluginInstall(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern plugin install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var help bool
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printPluginUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern plugin install: %v\n", err)
		return 2
	}
	if help {
		printPluginUsage(o.Out)
		return 0
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(o.Err, "tern plugin install: a path or git URL is required")
		return 2
	} else if len(rest) > 1 {
		fmt.Fprintf(o.Err, "tern plugin install: unexpected argument %q\n", rest[1])
		return 2
	}

	configDir, ok := pluginConfigDir(o, "tern plugin install")
	if !ok {
		return 1
	}
	installed, err := ext.InstallExtension(configDir, rest[0])
	if err != nil {
		fmt.Fprintf(o.Err, "tern plugin install: %v\n", err)
		return 1
	}
	fmt.Fprintf(o.Out, "Installed plugin %q\n", installed.Manifest.Name)
	if v := strings.TrimSpace(installed.Manifest.Version); v != "" {
		fmt.Fprintf(o.Out, "  Version: %s\n", v)
	}
	if d := strings.TrimSpace(installed.Manifest.Description); d != "" {
		fmt.Fprintf(o.Out, "  Description: %s\n", d)
	}
	fmt.Fprintf(o.Out, "  Location: %s\n", filepath.Join(configDir, ext.PluginsDirName, installed.Manifest.Name))
	fmt.Fprintf(o.Out, "  Tools: %d, Hooks: %d, MCP servers: %d\n",
		len(installed.Manifest.Tools), len(installed.Manifest.Hooks), len(installed.Manifest.MCP))
	return 0
}

func runPluginRemove(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern plugin remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var help bool
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printPluginUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern plugin remove: %v\n", err)
		return 2
	}
	if help {
		printPluginUsage(o.Out)
		return 0
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(o.Err, "tern plugin remove: a plugin name is required")
		return 2
	}
	if len(rest) > 1 {
		fmt.Fprintf(o.Err, "tern plugin remove: unexpected argument %q\n", rest[1])
		return 2
	}

	configDir, ok := pluginConfigDir(o, "tern plugin remove")
	if !ok {
		return 1
	}
	name := strings.TrimSpace(rest[0])
	if err := ext.RemoveExtension(configDir, name); err != nil {
		fmt.Fprintf(o.Err, "tern plugin remove: %v\n", err)
		return 1
	}
	fmt.Fprintf(o.Out, "Removed plugin %q\n", name)
	return 0
}

func runPluginGuide(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern plugin guide", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var help bool
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(o.Out, ext.PluginGuide())
			return 0
		}
		fmt.Fprintf(o.Err, "tern plugin guide: %v\n", err)
		return 2
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(o.Err, "tern plugin guide: unexpected argument %q\n", rest[0])
		return 2
	}
	fmt.Fprint(o.Out, ext.PluginGuide())
	return 0
}
