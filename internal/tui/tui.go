// Package tui implements tern's interactive terminal user interface.
//
// The interface is built on Bubble Tea, Bubbles and Lip Gloss. It renders a
// streaming Markdown transcript, the live sub-agent group, a multi-line input
// editor and a one-line status bar. Agent goroutines communicate with the UI
// through an unbounded event bus so a slow terminal can never block a turn.
package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/checkpoint"
	"github.com/tern-agent/tern/internal/config"
	terncontext "github.com/tern-agent/tern/internal/context"
	"github.com/tern-agent/tern/internal/ext"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
	"github.com/tern-agent/tern/internal/subagent"
	"github.com/tern-agent/tern/internal/tools"
)

// Options configures the interactive TUI.
type Options struct {
	Config   *config.Config
	Agent    *agent.Agent
	Subagent *subagent.Manager
	// SubagentManager is an alias for Subagent.
	SubagentManager *subagent.Manager
	SessionStore    *session.Store
	// Store is an alias for SessionStore.
	Store       *session.Store
	Session     *session.Session
	Checkpoints *checkpoint.Store
	// CheckpointStore is an alias for Checkpoints.
	CheckpointStore *checkpoint.Store
	Extensions      *ext.Manager
	// Ext and MCP are aliases for Extensions.
	Ext   *ext.Manager
	MCP   *ext.Manager
	Todos *tools.TodoStore
	// TodoStore is an alias for Todos.
	TodoStore       *tools.TodoStore
	WorkDir         string
	GlobalConfigDir string
	GitBranch       string
	Version         string
	NoColor         bool
}

// Run starts the interactive program and blocks until it exits.
func Run(ctx context.Context, opts Options) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m := New(opts)
	m.baseCtx = ctx

	// Install interactive approval handlers for sub-agents. The queue
	// serializes prompts from concurrent agents.
	if m.opts.Subagent != nil {
		m.opts.Subagent.Approvals().SetApprover(m.subagentApprover)
	}

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	m.program = p
	go m.bus.pump(p)

	// Honour caller cancellation without leaking a goroutine after exit.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			m.bus.Push(tea.QuitMsg{})
		case <-watchDone:
		}
	}()

	_, err := p.Run()
	close(watchDone)
	m.bus.Close()
	if m.cancel != nil {
		m.cancel()
	}
	if errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

// New constructs a model without starting a program. It is useful for tests
// and embeddings that drive the model directly.
func New(opts Options) *Model {
	// Capture the caller-provided config dir before normalization so an empty
	// value stays purely in-memory (no writes to the real user home).
	rawConfigDir := strings.TrimSpace(opts.GlobalConfigDir)
	opts = normalizeOptions(opts)
	m := &Model{
		opts:         opts,
		theme:        "dark",
		st:           newStyles(noColor(opts.NoColor)),
		ed:           newEditor(),
		bus:          newEventBus(),
		viewport:     viewport.New(0, 0),
		agents:       make(map[string]*agentBlock),
		agentLines:   make(map[string]*strings.Builder),
		statusMsg:    "Ready",
		followTail:   true,
		ctxWindow:    defaultContextWindow(opts),
		sessionStart: time.Now(),
		clipboardFn:  copyToClipboard,
	}
	if rawConfigDir != "" {
		m.historyPath = filepath.Join(rawConfigDir, "history.jsonl")
		m.loadHistory()
	}
	m.viewport.MouseWheelEnabled = true
	m.viewport.MouseWheelDelta = 3

	if opts.WorkDir != "" {
		// Commands are loaded eagerly so completion works offline.
		if cmds, err := terncontext.LoadCommands(opts.WorkDir, opts.GlobalConfigDir); err == nil {
			m.commands = cmds
		}
		// A cheap repository index keeps the welcome view informative without
		// delaying startup; failures simply leave the line out.
		if index, err := terncontext.IndexRepo(opts.WorkDir, 80); err == nil && index != nil {
			m.repoSummary = index.Summary()
		}
	}

	if m.opts.Agent != nil {
		m.opts.Agent.OnEvent = func(ev agent.Event) { m.bus.Push(agentEventMsg{event: ev}) }
		m.opts.Agent.OnApprove = m.approver
		m.opts.Agent.OnAskUser = m.askUser
		m.usage, m.totalCost = m.opts.Agent.TotalUsage()
		m.cache = cacheRate(m.usage)
		m.historyChars = historyChars(m.opts.Agent.History())
		m.ctxTokens = m.historyChars / 4
	}
	if m.opts.Subagent != nil {
		m.syncAgents()
	}
	return m
}

func normalizeOptions(opts Options) Options {
	if opts.Subagent == nil {
		opts.Subagent = opts.SubagentManager
	}
	if opts.SessionStore == nil {
		opts.SessionStore = opts.Store
	}
	if opts.Checkpoints == nil {
		opts.Checkpoints = opts.CheckpointStore
	}
	if opts.Extensions == nil {
		if opts.Ext != nil {
			opts.Extensions = opts.Ext
		} else {
			opts.Extensions = opts.MCP
		}
	}
	if opts.Todos == nil {
		opts.Todos = opts.TodoStore
	}
	if opts.Config == nil {
		if opts.Agent != nil && opts.Agent.Config != nil {
			opts.Config = opts.Agent.Config
		} else {
			opts.Config = config.Default()
		}
	}
	if opts.Agent != nil && opts.Agent.Config != nil {
		opts.Config = opts.Agent.Config
	}
	if strings.TrimSpace(opts.WorkDir) == "" {
		if wd, err := os.Getwd(); err == nil {
			opts.WorkDir = wd
		}
	}
	if abs, err := filepath.Abs(opts.WorkDir); err == nil {
		opts.WorkDir = abs
	}
	if strings.TrimSpace(opts.GlobalConfigDir) == "" {
		if dir, err := config.ConfigDir(); err == nil {
			opts.GlobalConfigDir = dir
		}
	}
	if opts.Agent == nil {
		opts.Agent = agent.New(agent.Options{
			Config:      opts.Config,
			Session:     opts.Session,
			Checkpoints: opts.Checkpoints,
			Todos:       opts.Todos,
			WorkDir:     opts.WorkDir,
		})
	}
	if opts.Agent.Config == nil {
		opts.Agent.Config = opts.Config
	}
	if opts.Todos == nil {
		opts.Todos = opts.Agent.Todos
	}
	return opts
}

func cacheRate(u provider.Usage) float64 {
	total := u.PromptCacheHitTokens + u.PromptCacheMissTokens
	if total <= 0 {
		return 0
	}
	return float64(u.PromptCacheHitTokens) / float64(total)
}

func defaultContextWindow(opts Options) int {
	if opts.Agent != nil && opts.Agent.Provider != nil {
		if caps := opts.Agent.Provider.ModelInfo(opts.Config.Model); caps.ContextWindow > 0 {
			return caps.ContextWindow
		}
	}
	if opts.Config != nil {
		if p, ok := opts.Config.Pricing[opts.Config.Model]; ok && p.ContextWindow > 0 {
			return p.ContextWindow
		}
	}
	return 1_000_000
}

func historyChars(msgs []provider.Message) int {
	total := 0
	for _, msg := range msgs {
		total += len(msg.Content) + len(msg.ReasoningContent)
		for _, part := range msg.Parts {
			total += len(part.Text)
		}
		for _, call := range msg.ToolCalls {
			total += len(call.Function.Name) + len(call.Function.Arguments)
		}
	}
	return total
}
