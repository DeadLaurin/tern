package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tern-agent/tern/internal/config"
	terncontext "github.com/tern-agent/tern/internal/context"
)

func printSkillUsage(w io.Writer) {
	fmt.Fprintln(w, `tern skill - manage Claude / Agent Skills

Usage:
  tern skill [list] [--json]
  tern skill install <url|owner/repo|path> [--local] [--name <name>]
  tern skill remove <name> [--local]

Subcommands:
  list        list all discovered skills (default)
  install     pull a skill from GitHub, a raw URL, or a local path (aliases: add, pull)
  remove      remove an installed skill (aliases: rm, uninstall)

Flags:
      --local          install into / remove from ./.tern/skills instead of ~/.config/tern/skills
      --name <name>    override the installed skill name (single-skill installs)
      --json           emit JSON output (list only)
  -h, --help           show this help

Auto-detected directories:
  global   ~/.config/tern/skills, ~/.claude/skills, ~/.agents/skills
  project  ./.tern/skills, ./.claude/skills, ./.agents/skills`)
}

func runSkillCommand(args []string, o runOptions) int {
	if len(args) == 0 {
		return runSkillList(nil, o)
	}
	switch args[0] {
	case "list", "ls":
		return runSkillList(args[1:], o)
	case "install", "add", "pull":
		return runSkillInstall(args[1:], o)
	case "remove", "rm", "uninstall":
		return runSkillRemove(args[1:], o)
	case "help", "-h", "--help":
		printSkillUsage(o.Out)
		return 0
	default:
		fmt.Fprintf(o.Err, "tern skill: unknown subcommand %q\n", args[0])
		printSkillUsage(o.Err)
		return 2
	}
}

func runSkillList(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern skill list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonOut, help bool
	fs.BoolVar(&jsonOut, "json", false, "emit JSON")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printSkillUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern skill list: %v\n", err)
		return 2
	}
	if help {
		printSkillUsage(o.Out)
		return 0
	}
	configDir, err := config.ConfigDir()
	if err != nil {
		fmt.Fprintf(o.Err, "tern skill list: %v\n", err)
		return 1
	}
	workDir := strings.TrimSpace(o.WorkDir)
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	skills, err := terncontext.ListSkills(workDir, configDir)
	if err != nil {
		fmt.Fprintf(o.Err, "tern skill list: %v\n", err)
		return 1
	}
	if jsonOut {
		enc := json.NewEncoder(o.Out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(skills)
		return 0
	}
	if len(skills) == 0 {
		fmt.Fprintln(o.Out, "No skills found.")
		return 0
	}
	tw := tabwriter.NewWriter(o.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSCOPE\tDESCRIPTION\tPATH")
	for _, s := range skills {
		desc := s.Description
		if desc == "" {
			desc = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name, s.Scope, desc, s.Path)
	}
	_ = tw.Flush()
	return 0
}

func runSkillInstall(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern skill install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var local, help bool
	var nameOverride string
	fs.BoolVar(&local, "local", false, "install into ./.tern/skills")
	fs.StringVar(&nameOverride, "name", "", "override skill name")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(reorderFlags(args, map[string]bool{"name": true})); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printSkillUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern skill install: %v\n", err)
		return 2
	}
	if help {
		printSkillUsage(o.Out)
		return 0
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(o.Err, "tern skill install: a URL, GitHub owner/repo, or local path is required")
		return 2
	}

	targetRoot, err := skillTargetDir(local, o.WorkDir)
	if err != nil {
		fmt.Fprintf(o.Err, "tern skill install: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	installed, err := terncontext.InstallSkills(ctx, targetRoot, rest[0], nameOverride, nil)
	if err != nil {
		fmt.Fprintf(o.Err, "tern skill install: %v\n", err)
		return 1
	}
	for _, s := range installed {
		fmt.Fprintf(o.Out, "Installed skill %q → %s\n", s.Name, s.Path)
	}
	return 0
}

func runSkillRemove(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern skill remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var local, help bool
	fs.BoolVar(&local, "local", false, "remove from ./.tern/skills")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(reorderFlags(args, nil)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printSkillUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern skill remove: %v\n", err)
		return 2
	}
	if help {
		printSkillUsage(o.Out)
		return 0
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(o.Err, "tern skill remove: a skill name is required")
		return 2
	}
	targetRoot, err := skillTargetDir(local, o.WorkDir)
	if err != nil {
		fmt.Fprintf(o.Err, "tern skill remove: %v\n", err)
		return 1
	}
	if err := terncontext.RemoveSkill(targetRoot, rest[0]); err != nil {
		fmt.Fprintf(o.Err, "tern skill remove: %v\n", err)
		return 1
	}
	fmt.Fprintf(o.Out, "Removed skill %q\n", rest[0])
	return 0
}

func skillTargetDir(local bool, workDir string) (string, error) {
	if local {
		wd := strings.TrimSpace(workDir)
		if wd == "" {
			var err error
			wd, err = os.Getwd()
			if err != nil {
				return "", err
			}
		}
		return filepath.Join(wd, ".tern", "skills"), nil
	}
	cfgDir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "skills"), nil
}

// reorderFlags lets flags appear before or after positional arguments.
func reorderFlags(args []string, takesValue map[string]bool) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && takesValue[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}
