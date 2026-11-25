package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/subagent"
	"github.com/tern-agent/tern/internal/tui"
)

// cliOptions is the parsed command line.
type cliOptions struct {
	Prompt     string
	PromptSet  bool
	Print      bool
	JSON       bool
	RPC        bool
	Continue   bool
	Resume     string
	Model      string
	ModelSet   bool
	Plan       bool
	Permission string
	PermSet    bool
	MaxCost    float64
	MaxCostSet bool
	MaxAgents  int
	AgentsSet  bool
	DebugCache bool
	CacheSet   bool
	Version    bool
	Help       bool
}

// runOptions makes main testable without a real terminal or process exit.
type runOptions struct {
	Args      []string
	In        io.Reader
	Out       io.Writer
	Err       io.Writer
	StdinTTY  bool
	StdoutTTY bool
	WorkDir   string
	Version   string
	Provider  provider.Provider
}

func defaultRunOptions(args []string) runOptions {
	return runOptions{
		Args: args,
		In:   os.Stdin,
		Out:  os.Stdout,
		Err:  os.Stderr,
	}
}

// runWithOptions executes one complete tern invocation and returns an exit
// code.
func runWithOptions(o runOptions) int {
	if o.In == nil {
		o.In = strings.NewReader("")
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	if o.Version == "" {
		o.Version = version
	}

	// Direct subcommands are checked before any interactive launch.
	if len(o.Args) > 0 {
		switch o.Args[0] {
		case "version":
			fmt.Fprintf(o.Out, "tern %s\n", o.Version)
			return 0
		case "config":
			return runConfigCommand(o.Args[1:], o)
		case "update":
			return runUpdateCommand(o.Out, o.Err)
		}
	}

	opts, err := parseCLI(o.Args, o.Err)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(o.Err, "tern: %v\n", err)
		return 2
	}
	if opts.Help {
		printUsage(o.Out)
		return 0
	}
	if opts.Version {
		fmt.Fprintf(o.Out, "tern %s\n", o.Version)
		return 0
	}

	workDir := strings.TrimSpace(o.WorkDir)
	if workDir == "" {
		workDir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(o.Err, "tern: determine working directory: %v\n", err)
			return 1
		}
	}

	// RPC owns stdin, so it must never be pre-read as a prompt.
	promptMode := !opts.RPC && (opts.Print || opts.PromptSet || !o.StdinTTY)
	var prompt string
	var piped string
	if promptMode {
		prompt = opts.Prompt
		if !o.StdinTTY && o.In != nil {
			data, readErr := io.ReadAll(o.In)
			if readErr != nil {
				fmt.Fprintf(o.Err, "tern: read stdin: %v\n", readErr)
				return 1
			}
			piped = string(data)
		}
		prompt = combinePrompt(prompt, piped)
		if strings.TrimSpace(prompt) == "" {
			fmt.Fprintln(o.Err, "tern: no prompt provided (pass -p <text>, --prompt <text>, or pipe stdin)")
			return 2
		}
	}
	if !promptMode && !opts.RPC && !o.StdinTTY {
		fmt.Fprintln(o.Err, "tern: interactive mode requires a terminal; pass -p <text> for non-interactive use")
		return 2
	}

	cfg, err := loadConfig(opts, workDir)
	if err != nil {
		fmt.Fprintf(o.Err, "tern: %v\n", err)
		return 1
	}

	// First-run authentication.
	if strings.TrimSpace(cfg.APIKey) == "" {
		key, authErr := ensureAPIKey(o.In, o.Out, o.Err, o.StdinTTY)
		if authErr != nil {
			fmt.Fprintf(o.Err, "tern: %v\n", authErr)
			return 1
		}
		cfg.APIKey = key
	}

	configDir, err := config.ConfigDir()
	if err != nil {
		fmt.Fprintf(o.Err, "tern: %v\n", err)
		return 1
	}
	dataDir, err := config.DataDir()
	if err != nil {
		fmt.Fprintf(o.Err, "tern: %v\n", err)
		return 1
	}

	prov := o.Provider
	if prov == nil {
		client := provider.NewDeepSeekClient(cfg.APIKey)
		client.BaseURL = cfg.BaseURL
		if cfg.DebugCache {
			client.DebugCacheLogger = func(msg string) {
				fmt.Fprintf(o.Err, "[cache] %s\n", msg)
			}
		}
		prov = client
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var sink *ndjsonWriter
	var printer *printStream
	rtOpts := RuntimeOptions{
		Context:   ctx,
		Config:    cfg,
		WorkDir:   workDir,
		Provider:  prov,
		ConfigDir: configDir,
		DataDir:   dataDir,
		Resume:    opts.Resume,
		Continue:  opts.Continue,
		Plan:      opts.Plan,
	}
	switch {
	case opts.RPC || opts.JSON:
		sink = newNDJSONWriter(o.Out)
		rtOpts.OnEvent = func(ev agent.Event) { _ = sink.WriteEvent(ev) }
		rtOpts.OnSubagentEvent = func(ev subagent.Event) { _ = sink.WriteSubagent(ev) }
	case promptMode:
		printer = newPrintStream(o.Out)
		rtOpts.OnEvent = printer.onEvent
		rtOpts.OnSubagentEvent = printModeSubagentEvent(o.Err)
	}

	rt, err := BuildRuntime(rtOpts)
	if err != nil {
		fmt.Fprintf(o.Err, "tern: %v\n", err)
		return 1
	}
	defer func() { _ = rt.Close() }()
	rt.printStream = printer
	for _, warning := range rt.Warnings {
		fmt.Fprintf(o.Err, "tern: warning: %s\n", warning)
	}

	switch {
	case opts.RPC:
		return runRPC(ctx, rt, o.In, sink, o.Err)
	case opts.JSON:
		return runJSON(ctx, rt, prompt, o.Out, o.Err)
	case promptMode:
		return runPrint(ctx, rt, prompt, o.Out, o.Err)
	default:
		err := tui.Run(ctx, tui.Options{
			Config:          cfg,
			Agent:           rt.Agent,
			Subagent:        rt.Subagents,
			SessionStore:    rt.SessionStore,
			Session:         rt.Session,
			Checkpoints:     rt.Checkpoints,
			Extensions:      rt.MCP,
			Todos:           rt.Todos,
			WorkDir:         workDir,
			GlobalConfigDir: configDir,
			Version:         o.Version,
			NoColor:         noColor(),
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(o.Err, "tern: %v\n", err)
			return 1
		}
		return 0
	}
}

func parseCLI(args []string, errOut io.Writer) (*cliOptions, error) {
	opts := &cliOptions{}
	fs := flag.NewFlagSet("tern", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&opts.Prompt, "p", "", "run one prompt non-interactively")
	fs.StringVar(&opts.Prompt, "prompt", "", "run one prompt non-interactively")
	fs.BoolVar(&opts.Print, "print", false, "run one prompt non-interactively")
	fs.BoolVar(&opts.JSON, "json", false, "emit newline-delimited JSON events")
	fs.BoolVar(&opts.RPC, "rpc", false, "serve JSON-RPC over stdin/stdout")
	fs.BoolVar(&opts.Continue, "continue", false, "continue the most recent session")
	fs.BoolVar(&opts.Continue, "c", false, "continue the most recent session")
	fs.StringVar(&opts.Resume, "resume", "", "resume a specific session id")
	fs.StringVar(&opts.Model, "model", "", "model name")
	fs.BoolVar(&opts.Plan, "plan", false, "start in read-only plan mode")
	fs.StringVar(&opts.Permission, "permission", "", "permission mode: ask, auto-edit, yolo, plan")
	fs.Float64Var(&opts.MaxCost, "max-cost", 0, "stop after this spend in USD")
	fs.IntVar(&opts.MaxAgents, "max-agents", 0, "maximum concurrent sub-agents")
	fs.BoolVar(&opts.DebugCache, "debug-cache", false, "log cache prefix hashes and divergence")
	fs.BoolVar(&opts.Version, "version", false, "print the version and exit")
	fs.BoolVar(&opts.Help, "help", false, "show help")
	fs.BoolVar(&opts.Help, "h", false, "show help")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return nil, fmt.Errorf("unexpected argument %q", rest[0])
	}
	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	opts.PromptSet = visited["p"] || visited["prompt"]
	opts.ModelSet = visited["model"]
	opts.PermSet = visited["permission"]
	opts.MaxCostSet = visited["max-cost"]
	opts.AgentsSet = visited["max-agents"]
	opts.CacheSet = visited["debug-cache"]
	return opts, nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `tern - a careful coding agent

Usage:
  tern [flags]
  tern config
  tern update
  tern version

Flags:
  -p, --prompt <text>       run one prompt non-interactively
      --print               run one prompt non-interactively
      --json                emit newline-delimited JSON events
      --rpc                 serve NDJSON JSON-RPC on stdin/stdout
  -c, --continue            continue the most recent session
      --resume <id>         resume a specific session
      --model <name>        model name
      --plan                start in read-only plan mode
      --permission <mode>   ask, auto-edit, yolo or plan
      --max-cost <usd>      stop after this spend
      --max-agents <n>      maximum concurrent sub-agents
      --debug-cache         log prefix hashes and cache divergence
      --version             print the version
  -h, --help                show this help`)
}

func loadConfig(opts *cliOptions, workDir string) (*config.Config, error) {
	flags := &config.FlagOverrides{}
	if opts.ModelSet {
		flags.Model = &opts.Model
	}
	if opts.Plan {
		mode := "plan"
		flags.PermissionMode = &mode
	} else if opts.PermSet {
		flags.PermissionMode = &opts.Permission
	}
	if opts.MaxCostSet {
		flags.MaxCostUSD = &opts.MaxCost
	}
	if opts.AgentsSet {
		flags.MaxAgents = &opts.MaxAgents
	}
	if opts.CacheSet {
		flags.DebugCache = &opts.DebugCache
	}
	return config.Load(config.LoadOptions{WorkDir: workDir, Flags: flags})
}

func runConfigCommand(args []string, o runOptions) int {
	opts, err := parseCLI(args, o.Err)
	if err != nil {
		fmt.Fprintf(o.Err, "tern config: %v\n", err)
		return 2
	}
	workDir := strings.TrimSpace(o.WorkDir)
	if workDir == "" {
		workDir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(o.Err, "tern config: %v\n", err)
			return 1
		}
	}
	cfg, err := loadConfig(opts, workDir)
	if err != nil {
		fmt.Fprintf(o.Err, "tern config: %v\n", err)
		return 1
	}
	_, _ = io.WriteString(o.Out, cfg.FormatEffective())
	return 0
}

func combinePrompt(prompt, piped string) string {
	prompt = strings.TrimSpace(prompt)
	piped = strings.TrimSpace(piped)
	switch {
	case prompt == "":
		return piped
	case piped == "":
		return prompt
	default:
		return prompt + "\n\n" + piped
	}
}

func noColor() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb")
}
