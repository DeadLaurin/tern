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

	"github.com/tern-agent/tern/internal/acp"
	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/provider"
)

// runACPCommand serves the Agent Client Protocol (ACP) over stdin/stdout.
// It is wired as `tern acp` by cli.go.
func runACPCommand(args []string, o runOptions) int {
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

	opts, err := parseCLI(args, o.Err)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(o.Err, "tern acp: %v\n", err)
		return 2
	}
	if opts.Help {
		printACPUsage(o.Out)
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
			fmt.Fprintf(o.Err, "tern acp: determine working directory: %v\n", err)
			return 1
		}
	}

	cfg, err := loadConfig(opts, workDir)
	if err != nil {
		fmt.Fprintf(o.Err, "tern acp: %v\n", err)
		return 1
	}

	prov := o.Provider
	if prov == nil {
		if strings.TrimSpace(cfg.APIKey) == "" {
			if isLoopbackBaseURL(cfg.BaseURL) {
				cfg.APIKey = "local"
			} else {
				key, authErr := ensureAPIKey(o.In, o.Out, o.Err, o.StdinTTY)
				if authErr != nil {
					fmt.Fprintf(o.Err, "tern acp: %v\n", authErr)
					return 1
				}
				cfg.APIKey = key
			}
		}
		client := provider.NewDeepSeekClient(cfg.APIKey)
		client.BaseURL = cfg.BaseURL
		prov = client
	}

	configDir, err := config.ConfigDir()
	if err != nil {
		fmt.Fprintf(o.Err, "tern acp: %v\n", err)
		return 1
	}
	dataDir, err := config.DataDir()
	if err != nil {
		fmt.Fprintf(o.Err, "tern acp: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := BuildRuntime(RuntimeOptions{
		Context:   ctx,
		Config:    cfg,
		WorkDir:   workDir,
		Provider:  prov,
		ConfigDir: configDir,
		DataDir:   dataDir,
	})
	if err != nil {
		fmt.Fprintf(o.Err, "tern acp: %v\n", err)
		return 1
	}
	defer func() { _ = rt.Close() }()
	for _, warning := range rt.Warnings {
		fmt.Fprintf(o.Err, "tern: warning: %s\n", warning)
	}

	server := acp.NewServer(acp.Options{
		Version:      o.Version,
		WorkDir:      workDir,
		Agent:        rt.Agent,
		Permissions:  rt.Permissions,
		Session:      rt.Session,
		SessionStore: rt.SessionStore,
	})
	if err := server.Serve(ctx, o.In, o.Out); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(o.Err, "tern acp: %v\n", err)
		return 1
	}
	return 0
}

func printACPUsage(w io.Writer) {
	fmt.Fprintln(w, `tern acp - serve the Agent Client Protocol over stdin/stdout

Usage:
  tern acp [flags]

Flags:
      --model <name>        model name
      --local               use Ollama at http://localhost:11434/v1 (no API key needed)
      --permission <mode>   ask, auto-edit, yolo or plan
      --max-cost <usd>      stop after this spend
      --max-agents <n>      maximum concurrent sub-agents
  -h, --help                show this help

ACP hosts start this command and speak JSON-RPC 2.0 as newline-delimited JSON.`)
}
