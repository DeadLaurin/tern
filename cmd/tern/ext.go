package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/ext"
)

// extListEntry is the JSON/text shape of one extension in `tern ext list`.
type extListEntry struct {
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

func printExtUsage(w io.Writer) {
	fmt.Fprintln(w, `tern ext - manage extensions

Usage:
  tern ext [list] [--json]
  tern ext install <path-or-git-url>
  tern ext remove <name>
  tern ext uninstall <name>
  tern ext rm <name>

Subcommands:
  list        list discovered extensions (default)
  install     install an extension from a local directory or git URL
  remove      remove an installed extension from the global config

Flags:
      --json    emit the extension list as JSON (list only)
  -h, --help    show this help`)
}

// runExtCommand dispatches `tern ext ...`.
func runExtCommand(args []string, o runOptions) int {
	if len(args) == 0 {
		return runExtList(nil, o)
	}
	switch args[0] {
	case "list", "ls":
		return runExtList(args[1:], o)
	case "install", "add":
		return runExtInstall(args[1:], o)
	case "remove", "uninstall", "rm":
		return runExtRemove(args[1:], o)
	case "help", "-h", "--help":
		printExtUsage(o.Out)
		return 0
	default:
		fmt.Fprintf(o.Err, "tern ext: unknown subcommand %q\n", args[0])
		printExtUsage(o.Err)
		return 2
	}
}

func runExtList(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern ext list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonOut, help bool
	fs.BoolVar(&jsonOut, "json", false, "emit the extension list as JSON")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printExtUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern ext list: %v\n", err)
		return 2
	}
	if help {
		printExtUsage(o.Out)
		return 0
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(o.Err, "tern ext list: unexpected argument %q\n", rest[0])
		return 2
	}

	configDir, err := config.ConfigDir()
	if err != nil {
		fmt.Fprintf(o.Err, "tern ext list: %v\n", err)
		return 1
	}
	workDir := strings.TrimSpace(o.WorkDir)
	if workDir == "" {
		if wd, wdErr := os.Getwd(); wdErr == nil {
			workDir = wd
		}
	}

	exts, warnings := ext.ListAllExtensions(workDir, configDir)
	for _, warning := range warnings {
		fmt.Fprintf(o.Err, "tern ext list: warning: %s\n", warning)
	}

	entries := make([]extListEntry, 0, len(exts))
	for _, e := range exts {
		if e == nil {
			continue
		}
		entries = append(entries, extListEntry{
			Name:        e.Manifest.Name,
			Version:     e.Manifest.Version,
			Source:      e.Source,
			Enabled:     e.Manifest.IsEnabled(),
			Description: e.Manifest.Description,
			Dir:         e.Dir,
			Tools:       len(e.Manifest.Tools),
			Hooks:       len(e.Manifest.Hooks),
			MCP:         len(e.Manifest.MCP),
		})
	}

	if jsonOut {
		enc := json.NewEncoder(o.Out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(entries); err != nil {
			fmt.Fprintf(o.Err, "tern ext list: %v\n", err)
			return 1
		}
		return 0
	}

	if len(entries) == 0 {
		fmt.Fprintln(o.Out, "No extensions installed.")
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

func runExtInstall(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern ext install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var help bool
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printExtUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern ext install: %v\n", err)
		return 2
	}
	if help {
		printExtUsage(o.Out)
		return 0
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(o.Err, "tern ext install: a path or git URL is required")
		return 2
	} else if len(rest) > 1 {
		fmt.Fprintf(o.Err, "tern ext install: unexpected argument %q\n", rest[1])
		return 2
	}

	configDir, err := config.ConfigDir()
	if err != nil {
		fmt.Fprintf(o.Err, "tern ext install: %v\n", err)
		return 1
	}
	installed, err := ext.InstallExtension(configDir, rest[0])
	if err != nil {
		fmt.Fprintf(o.Err, "tern ext install: %v\n", err)
		return 1
	}
	fmt.Fprintf(o.Out, "Installed extension %q\n", installed.Manifest.Name)
	if v := strings.TrimSpace(installed.Manifest.Version); v != "" {
		fmt.Fprintf(o.Out, "  Version: %s\n", v)
	}
	if d := strings.TrimSpace(installed.Manifest.Description); d != "" {
		fmt.Fprintf(o.Out, "  Description: %s\n", d)
	}
	fmt.Fprintf(o.Out, "  Location: %s\n", installed.Dir)
	fmt.Fprintf(o.Out, "  Tools: %d, Hooks: %d, MCP servers: %d\n",
		len(installed.Manifest.Tools), len(installed.Manifest.Hooks), len(installed.Manifest.MCP))
	return 0
}

func runExtRemove(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern ext remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var help bool
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printExtUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern ext remove: %v\n", err)
		return 2
	}
	if help {
		printExtUsage(o.Out)
		return 0
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(o.Err, "tern ext remove: an extension name is required")
		return 2
	}
	if len(rest) > 1 {
		fmt.Fprintf(o.Err, "tern ext remove: unexpected argument %q\n", rest[1])
		return 2
	}

	configDir, err := config.ConfigDir()
	if err != nil {
		fmt.Fprintf(o.Err, "tern ext remove: %v\n", err)
		return 1
	}
	name := strings.TrimSpace(rest[0])
	if err := ext.RemoveExtension(configDir, name); err != nil {
		fmt.Fprintf(o.Err, "tern ext remove: %v\n", err)
		return 1
	}
	fmt.Fprintf(o.Out, "Removed extension %q\n", name)
	return 0
}
