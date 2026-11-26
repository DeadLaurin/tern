package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/tern-agent/tern/internal/checkpoint"
	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/session"
)

// commandWorkDir resolves the workspace used by the direct subcommands.
func commandWorkDir(o runOptions) (string, error) {
	workDir := strings.TrimSpace(o.WorkDir)
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		workDir = wd
	}
	if abs, err := filepath.Abs(workDir); err == nil {
		workDir = abs
	}
	return workDir, nil
}

// openLatestSessionCheckpoints opens the checkpoint store belonging to the
// latest session for workDir.
func openLatestSessionCheckpoints(workDir string) (*session.Session, *checkpoint.Store, error) {
	dataDir, err := config.DataDir()
	if err != nil {
		return nil, nil, err
	}
	store, err := session.NewStore(dataDir)
	if err != nil {
		return nil, nil, err
	}
	sess, err := store.Latest(workDir)
	if err != nil {
		return nil, nil, err
	}
	checkpoints, err := checkpoint.NewStore(filepath.Join(dataDir, "checkpoints"), sess.ID())
	if err != nil {
		return nil, nil, err
	}
	return sess, checkpoints, nil
}

func printUndoUsage(w io.Writer) {
	fmt.Fprintln(w, `tern undo - roll back checkpointed turns

Usage:
  tern undo [n]

Rolls back the last n turns (default 1) of the latest session for the
current workspace. Created files are deleted; modified or deleted files are
restored.

Flags:
  -h, --help    show this help`)
}

// runUndoCommand rolls back the last n checkpoint turns of the latest session.
func runUndoCommand(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern undo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var help bool
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUndoUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern undo: %v\n", err)
		return 2
	}
	if help {
		printUndoUsage(o.Out)
		return 0
	}

	count := 1
	rest := fs.Args()
	if len(rest) > 1 {
		fmt.Fprintf(o.Err, "tern undo: unexpected argument %q\n", rest[1])
		return 2
	}
	if len(rest) == 1 {
		n, err := strconv.Atoi(strings.TrimSpace(rest[0]))
		if err != nil || n < 1 {
			fmt.Fprintf(o.Err, "tern undo: invalid turn count %q (want a positive integer)\n", rest[0])
			return 2
		}
		count = n
	}

	workDir, err := commandWorkDir(o)
	if err != nil {
		fmt.Fprintf(o.Err, "tern undo: determine working directory: %v\n", err)
		return 1
	}
	sess, checkpoints, err := openLatestSessionCheckpoints(workDir)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			fmt.Fprintf(o.Err, "tern undo: no session found for %s\n", workDir)
			return 1
		}
		fmt.Fprintf(o.Err, "tern undo: %v\n", err)
		return 1
	}

	var (
		undone   []int
		restored []string
		deleted  []string
	)
	for i := 0; i < count; i++ {
		result, undoErr := checkpoints.Undo()
		if errors.Is(undoErr, checkpoint.ErrNoTurns) {
			break
		}
		if undoErr != nil {
			fmt.Fprintf(o.Err, "tern undo: %v\n", undoErr)
			return 1
		}
		undone = append(undone, result.TurnIDs...)
		restored = appendUnique(restored, result.Restored...)
		deleted = appendUnique(deleted, result.Deleted...)
	}

	if len(undone) == 0 {
		fmt.Fprintf(o.Out, "Nothing to undo: no checkpoints recorded for session %s.\n", sess.ID())
		return 0
	}

	fmt.Fprintf(o.Out, "Undid %d checkpoint turn(s) in session %s\n", len(undone), sess.ID())
	if len(restored) == 0 && len(deleted) == 0 {
		fmt.Fprintln(o.Out, "  No files changed on disk.")
	}
	if len(restored) > 0 {
		fmt.Fprintln(o.Out, "  Restored:")
		for _, path := range restored {
			fmt.Fprintf(o.Out, "    - %s\n", path)
		}
	}
	if len(deleted) > 0 {
		fmt.Fprintln(o.Out, "  Deleted:")
		for _, path := range deleted {
			fmt.Fprintf(o.Out, "    - %s\n", path)
		}
	}
	if len(undone) < count {
		fmt.Fprintf(o.Out, "No more checkpoint turns to undo (%d requested, %d undone).\n", count, len(undone))
	}
	return 0
}

func appendUnique(dst []string, values ...string) []string {
	seen := make(map[string]bool, len(dst)+len(values))
	for _, v := range dst {
		seen[v] = true
	}
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		dst = append(dst, v)
	}
	return dst
}

func printLogUsage(w io.Writer) {
	fmt.Fprintln(w, `tern log - list checkpointed turns

Usage:
  tern log

Lists the checkpoint turns of the latest session for the current workspace,
including turn ID, timestamp, summary, and the files each turn touched.

Flags:
  -h, --help    show this help`)
}

// runLogCommand lists the checkpoint turns of the latest session.
func runLogCommand(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern log", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var help bool
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printLogUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern log: %v\n", err)
		return 2
	}
	if help {
		printLogUsage(o.Out)
		return 0
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(o.Err, "tern log: unexpected argument %q\n", rest[0])
		return 2
	}

	workDir, err := commandWorkDir(o)
	if err != nil {
		fmt.Fprintf(o.Err, "tern log: determine working directory: %v\n", err)
		return 1
	}
	sess, checkpoints, err := openLatestSessionCheckpoints(workDir)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			fmt.Fprintf(o.Out, "No sessions found for %s.\n", workDir)
			return 0
		}
		fmt.Fprintf(o.Err, "tern log: %v\n", err)
		return 1
	}

	turns := checkpoints.List()
	name := strings.TrimSpace(sess.Name())
	if name != "" {
		fmt.Fprintf(o.Out, "Checkpoints for session %s (%s)\n", sess.ID(), name)
	} else {
		fmt.Fprintf(o.Out, "Checkpoints for session %s\n", sess.ID())
	}
	fmt.Fprintf(o.Out, "Workspace: %s\n", workDir)
	if len(turns) == 0 {
		fmt.Fprintln(o.Out, "No checkpoint turns recorded yet.")
		return 0
	}
	fmt.Fprintln(o.Out)

	tw := tabwriter.NewWriter(o.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  TURN\tWHEN\tFILES\tSUMMARY")
	for _, turn := range turns {
		when := "-"
		if !turn.Timestamp.IsZero() {
			when = turn.Timestamp.Local().Format("2006-01-02 15:04:05")
		}
		summary := strings.TrimSpace(turn.Summary)
		if summary == "" {
			summary = "-"
		}
		fmt.Fprintf(tw, "  %d\t%s\t%d\t%s\n", turn.TurnID, when, turn.FileCount, summary)
	}
	_ = tw.Flush()

	for _, turn := range turns {
		if len(turn.Paths) == 0 {
			continue
		}
		fmt.Fprintln(o.Out)
		fmt.Fprintf(o.Out, "  turn %d files:\n", turn.TurnID)
		for _, path := range turn.Paths {
			fmt.Fprintf(o.Out, "    - %s\n", path)
		}
	}
	return 0
}

func printPolicyUsage(w io.Writer) {
	fmt.Fprintln(w, `tern policy - show the effective permission policy

Usage:
  tern policy [flags]

Prints the effective permission mode, web policy, workspace root, and the
built-in safety deny rules.

Flags:
      --permission <mode>   ask, auto-edit, yolo or plan
      --plan                show the read-only plan policy
  -h, --help                show this help`)
}

// runPolicyCommand prints the effective permission policy and built-in deny
// rules.
func runPolicyCommand(args []string, o runOptions) int {
	opts, err := parseCLI(args, o.Err)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printPolicyUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern policy: %v\n", err)
		return 2
	}
	if opts.Help {
		printPolicyUsage(o.Out)
		return 0
	}
	if opts.PromptSet || opts.Print || opts.JSON || opts.RPC || opts.Continue || opts.Resume != "" {
		fmt.Fprintln(o.Err, "tern policy: only --permission, --plan, --local and --base-url are supported")
		return 2
	}

	workDir, err := commandWorkDir(o)
	if err != nil {
		fmt.Fprintf(o.Err, "tern policy: determine working directory: %v\n", err)
		return 1
	}
	cfg, err := loadConfig(opts, workDir)
	if err != nil {
		fmt.Fprintf(o.Err, "tern policy: %v\n", err)
		return 1
	}
	mode, err := permission.ParseMode(cfg.PermissionMode)
	if err != nil {
		fmt.Fprintf(o.Err, "tern policy: %v\n", err)
		return 1
	}
	web, err := permission.ParseWebPolicy(cfg.WebPermission)
	if err != nil {
		fmt.Fprintf(o.Err, "tern policy: %v\n", err)
		return 1
	}

	fmt.Fprintln(o.Out, "tern policy")
	fmt.Fprintln(o.Out)
	fmt.Fprintf(o.Out, "  Permission mode: %s\n", mode.String())
	fmt.Fprintf(o.Out, "  Web policy:      %s\n", web.String())
	fmt.Fprintf(o.Out, "  Workspace root:  %s\n", workDir)

	fmt.Fprintln(o.Out)
	fmt.Fprintln(o.Out, "Built-in deny paths (writes are always blocked):")
	for _, path := range permission.DefaultDenyWritePaths {
		fmt.Fprintf(o.Out, "  %s\n", path)
	}

	fmt.Fprintln(o.Out)
	fmt.Fprintln(o.Out, "Built-in deny bash patterns (commands are always blocked):")
	for _, pattern := range permission.DefaultDenyBashPatterns {
		fmt.Fprintf(o.Out, "  %s\n", pattern)
	}
	return 0
}
