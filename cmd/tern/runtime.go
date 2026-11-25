package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/checkpoint"
	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/ext"
	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
	"github.com/tern-agent/tern/internal/subagent"
	"github.com/tern-agent/tern/internal/tools"
	"github.com/tern-agent/tern/internal/web"
)

// RuntimeOptions controls BuildRuntime. Provider is injectable so tests can
// drive the complete command-line runtime with a deterministic mock.
type RuntimeOptions struct {
	Context  context.Context
	Config   *config.Config
	WorkDir  string
	Provider provider.Provider

	// ConfigDir and DataDir default to config.ConfigDir and config.DataDir.
	ConfigDir string
	DataDir   string

	// Resume is a specific session id. Continue resumes the most recent
	// session for WorkDir.
	Resume   string
	Continue bool

	// Plan forces plan (read-only) mode regardless of the configured mode.
	Plan bool

	OnEvent         func(agent.Event)
	OnSubagentEvent func(subagent.Event)
	OnApprove       func(ctx context.Context, req agent.ApprovalRequest) (bool, error)
	OnAskUser       func(ctx context.Context, question string, opts []string) (string, error)
}

// Runtime is the fully wired command-line application. Every dependency is
// constructed once and shared by the agent loop, the sub-agent manager and the
// interactive UI.
type Runtime struct {
	Config    *config.Config
	ConfigDir string
	DataDir   string
	WorkDir   string
	Provider  provider.Provider

	Permissions  *permission.Evaluator
	SessionStore *session.Store
	Session      *session.Session
	Checkpoints  *checkpoint.Store
	Registry     *tools.Registry
	Todos        *tools.TodoStore
	Hooks        *ext.Runner
	MCP          *ext.Manager
	Subagents    *subagent.Manager
	Agent        *agent.Agent

	Warnings []string

	printStream *printStream

	closeOnce sync.Once
	closeErr  error
}

// BuildRuntime wires the complete agent runtime. It never creates a provider
// implicitly: the caller supplies one, which keeps tests hermetic.
func BuildRuntime(opts RuntimeOptions) (*Runtime, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := opts.Config
	if cfg == nil {
		return nil, errors.New("tern: runtime requires a config")
	}
	if opts.Provider == nil {
		return nil, errors.New("tern: runtime requires a provider")
	}

	workDir := strings.TrimSpace(opts.WorkDir)
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("tern: determine working directory: %w", err)
		}
		workDir = wd
	}
	absWork, err := filepath.Abs(workDir)
	if err == nil {
		workDir = absWork
	}

	configDir := strings.TrimSpace(opts.ConfigDir)
	if configDir == "" {
		configDir, err = config.ConfigDir()
		if err != nil {
			return nil, err
		}
	}
	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		dataDir, err = config.DataDir()
		if err != nil {
			return nil, err
		}
	}

	rt := &Runtime{
		Config:    cfg,
		ConfigDir: configDir,
		DataDir:   dataDir,
		WorkDir:   workDir,
		Provider:  opts.Provider,
	}

	// Permissions: earlier flag layers have already been folded into cfg.
	modeName := cfg.PermissionMode
	if opts.Plan {
		modeName = string(permission.ModePlan)
	}
	mode, err := permission.ParseMode(modeName)
	if err != nil {
		return nil, err
	}
	webPolicy, err := permission.ParseWebPolicy(cfg.WebPermission)
	if err != nil {
		return nil, err
	}
	rt.Permissions = permission.New(permission.Policy{
		Mode:    mode,
		Web:     webPolicy,
		WorkDir: workDir,
	})

	// Sessions.
	rt.SessionStore, err = session.NewStore(dataDir)
	if err != nil {
		return nil, err
	}
	rt.Session, err = openSession(rt.SessionStore, workDir, cfg.Model, opts.Resume, opts.Continue)
	if err != nil {
		return nil, err
	}

	// Checkpoints live below the data directory, one subdirectory per session.
	rt.Checkpoints, err = checkpoint.NewStore(filepath.Join(dataDir, "checkpoints"), rt.Session.ID())
	if err != nil {
		return nil, err
	}

	// Hooks and MCP servers are optional extensions. Broken extension files
	// are reported as warnings rather than preventing the session from
	// starting.
	hookConfigs, hookWarnings := loadHookConfigs(configDir, workDir)
	rt.Warnings = append(rt.Warnings, hookWarnings...)
	rt.Hooks = ext.NewRunner(hookConfigs...)

	mcpConfigs, mcpWarnings := loadMCPConfigs(configDir, workDir)
	rt.Warnings = append(rt.Warnings, mcpWarnings...)
	rt.MCP = ext.NewManager(mcpConfigs...)

	// Tool registry: built-ins plus web tools, MCP tools and the task tool.
	rt.Registry = tools.NewEmptyRegistry()
	rt.Todos = tools.NewTodoStore()
	rt.Registry.Register(tools.NewReadTool())
	rt.Registry.Register(tools.NewWriteTool())
	rt.Registry.Register(tools.NewEditTool())
	rt.Registry.Register(tools.NewMultiEditTool())
	rt.Registry.Register(tools.NewLSTool())
	rt.Registry.Register(tools.NewGlobTool())
	rt.Registry.Register(tools.NewGrepTool(false))
	rt.Registry.Register(tools.NewBashTool())
	rt.Registry.Register(tools.NewTodoToolWithStore(rt.Todos))
	rt.Registry.Register(tools.NewAskUserTool())

	rt.Registry.Register(agent.NewWebFetchTool(web.FetchOptions{}))

	searcher, searchErr := web.NewSearcher(cfg.SearchBackend, cfg.SearchEndpoint, "", nil)
	if searchErr != nil {
		rt.Warnings = append(rt.Warnings, "web search disabled: "+searchErr.Error())
	}
	rt.Registry.Register(agent.NewWebSearchTool(searcher))

	if len(mcpConfigs) > 0 {
		if cerr := rt.MCP.Connect(ctx); cerr != nil {
			rt.Warnings = append(rt.Warnings, "mcp: "+cerr.Error())
		}
		mcpTools, terr := rt.MCP.Tools(ctx)
		for _, tool := range mcpTools {
			rt.Registry.Register(agent.NewMCPTool(rt.MCP, tool))
		}
		if terr != nil {
			rt.Warnings = append(rt.Warnings, "mcp tools: "+terr.Error())
		}
	}

	// Sub-agent manager. The parent registry must not yet contain the task
	// tool when the manager is built, so sub-agents cannot recurse through it.
	costFunc := func(model string, u provider.Usage) float64 {
		miss := u.PromptCacheMissTokens
		if miss == 0 && u.PromptCacheHitTokens == 0 {
			miss = u.PromptTokens
		}
		return cfg.EstimateCost(model, u.PromptCacheHitTokens, miss, u.CompletionTokens, time.Now())
	}
	subEmit := func(ev subagent.Event) {
		if len(rt.Hooks.Hooks) > 0 {
			input := ext.HookInput{
				Agent:     ev.AgentType,
				SessionID: rt.Session.ID(),
				CWD:       workDir,
				Extra: map[string]any{
					"agent_id":    ev.AgentID,
					"description": ev.Description,
					"status":      ev.Status,
					"summary":     ev.Summary,
					"detail":      ev.Detail,
				},
			}
			switch ev.Type {
			case subagent.EventSubagentStart:
				if ev.Status == subagent.StatusRunning {
					input.Event = ext.HookSubagentStart
					_, _ = rt.Hooks.Run(context.Background(), input)
				}
			case subagent.EventSubagentEnd:
				input.Event = ext.HookSubagentEnd
				_, _ = rt.Hooks.Run(context.Background(), input)
			}
		}
		if opts.OnSubagentEvent != nil {
			opts.OnSubagentEvent(ev)
		}
	}
	rt.Subagents = subagent.NewManager(subagent.Config{
		MaxAgents:       cfg.MaxAgents,
		MaxDepth:        cfg.MaxAgentDepth,
		MaxCostUSD:      cfg.MaxCostUSD,
		MaxSteps:        cfg.MaxSteps,
		Model:           cfg.Model,
		ReasoningEffort: cfg.ReasoningEffort,
		WorkDir:         workDir,
		Shell:           cfg.Shell,
		GlobalConfigDir: configDir,
		Provider:        opts.Provider,
		Registry:        rt.Registry,
		Permission:      rt.Permissions,
		Session:         rt.Session,
		Emit:            subEmit,
		CostFunc:        costFunc,
		SearchBackend:   cfg.SearchBackend,
		SearchEndpoint:  cfg.SearchEndpoint,
		PlanMode:        mode == permission.ModePlan,
	})
	rt.Registry.Register(subagent.NewTaskTool(rt.Subagents))

	// The agent is created last so it sees the final registry (including the
	// task tool) and the session's restored history.
	rt.Agent = agent.New(agent.Options{
		Config:      cfg,
		Provider:    opts.Provider,
		Registry:    rt.Registry,
		Permissions: rt.Permissions,
		Checkpoints: rt.Checkpoints,
		Session:     rt.Session,
		Hooks:       rt.Hooks,
		Todos:       rt.Todos,
		WorkDir:     workDir,
		OnEvent:     opts.OnEvent,
		OnApprove:   opts.OnApprove,
		OnAskUser:   opts.OnAskUser,
	})

	return rt, nil
}

func openSession(store *session.Store, workDir, model, resume string, continueLatest bool) (*session.Session, error) {
	resume = strings.TrimSpace(resume)
	if resume != "" {
		sess, err := store.Open(resume)
		if err != nil {
			return nil, fmt.Errorf("tern: resume session %q: %w", resume, err)
		}
		return sess, nil
	}
	if continueLatest {
		sess, err := store.Latest(workDir)
		if err == nil {
			return sess, nil
		}
		if !errors.Is(err, session.ErrSessionNotFound) {
			return nil, fmt.Errorf("tern: continue session: %w", err)
		}
	}
	return store.Create(session.SessionMeta{
		Model:   model,
		WorkDir: workDir,
	})
}

// Close tears down background bash jobs, MCP clients and active sub-agents.
// It is safe to call more than once.
func (rt *Runtime) Close() error {
	if rt == nil {
		return nil
	}
	rt.closeOnce.Do(func() {
		var errs []error

		if rt.Subagents != nil {
			rt.Subagents.CancelAll()
			waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := rt.Subagents.WaitContext(waitCtx); err != nil {
				errs = append(errs, fmt.Errorf("sub-agents: %w", err))
			}
			cancel()
		}
		if rt.Registry != nil {
			if err := rt.Registry.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if rt.MCP != nil {
			if err := rt.MCP.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		rt.closeErr = errors.Join(errs...)
	})
	return rt.closeErr
}

// ---------------------------------------------------------------------------
// Optional extension configuration
// ---------------------------------------------------------------------------

type hooksFile struct {
	Hooks []struct {
		Event          string   `toml:"event"`
		Command        string   `toml:"command"`
		Args           []string `toml:"args"`
		Matcher        string   `toml:"matcher"`
		TimeoutSeconds int      `toml:"timeout_seconds"`
	} `toml:"hooks"`
}

// extensionFilePaths returns the global and project-local locations for one
// extension config file, in load order. Project-local entries are appended
// (hooks) or override by name (MCP servers).
func extensionFilePaths(configDir, workDir, name string) []string {
	paths := []string{filepath.Join(configDir, name)}
	if workDir = strings.TrimSpace(workDir); workDir != "" {
		projectPath := filepath.Join(workDir, ".tern", name)
		if projectPath != paths[0] {
			paths = append(paths, projectPath)
		}
	}
	return paths
}

func loadHookConfigs(configDir, workDir string) ([]ext.HookConfig, []string) {
	var out []ext.HookConfig
	var warnings []string
	for _, path := range extensionFilePaths(configDir, workDir, "hooks.toml") {
		cfgs, warns := loadHookFile(path)
		out = append(out, cfgs...)
		warnings = append(warnings, warns...)
	}
	return out, warnings
}

func loadHookFile(path string) ([]ext.HookConfig, []string) {
	var file hooksFile
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []string{"hooks: " + err.Error()}
	}
	if _, err := toml.DecodeFile(path, &file); err != nil {
		return nil, []string{fmt.Sprintf("hooks: decode %s: %v", path, err)}
	}
	var warnings []string
	out := make([]ext.HookConfig, 0, len(file.Hooks))
	for i, h := range file.Hooks {
		cfg := ext.HookConfig{
			Event:   ext.HookEvent(strings.TrimSpace(h.Event)),
			Command: h.Command,
			Args:    h.Args,
			Matcher: h.Matcher,
		}
		if h.TimeoutSeconds > 0 {
			cfg.Timeout = time.Duration(h.TimeoutSeconds) * time.Second
		}
		if err := cfg.Validate(); err != nil {
			warnings = append(warnings, fmt.Sprintf("hooks: entry %d: %v", i+1, err))
			continue
		}
		out = append(out, cfg)
	}
	return out, warnings
}

type mcpFile struct {
	Servers []struct {
		Name           string            `toml:"name"`
		Transport      string            `toml:"transport"`
		Command        string            `toml:"command"`
		Args           []string          `toml:"args"`
		Env            []string          `toml:"env"`
		URL            string            `toml:"url"`
		Headers        map[string]string `toml:"headers"`
		TimeoutSeconds int               `toml:"timeout_seconds"`
	} `toml:"servers"`
}

func loadMCPConfigs(configDir, workDir string) ([]ext.ServerConfig, []string) {
	var out []ext.ServerConfig
	var warnings []string
	// Project-local servers override global ones with the same name while
	// keeping the global ordering stable.
	index := map[string]int{}
	for _, path := range extensionFilePaths(configDir, workDir, "mcp.toml") {
		cfgs, warns := loadMCPFile(path)
		warnings = append(warnings, warns...)
		for _, cfg := range cfgs {
			if i, ok := index[cfg.Name]; ok {
				out[i] = cfg
				continue
			}
			index[cfg.Name] = len(out)
			out = append(out, cfg)
		}
	}
	return out, warnings
}

func loadMCPFile(path string) ([]ext.ServerConfig, []string) {
	var file mcpFile
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []string{"mcp: " + err.Error()}
	}
	if _, err := toml.DecodeFile(path, &file); err != nil {
		return nil, []string{fmt.Sprintf("mcp: decode %s: %v", path, err)}
	}
	var warnings []string
	out := make([]ext.ServerConfig, 0, len(file.Servers))
	for i, s := range file.Servers {
		cfg := ext.ServerConfig{
			Name:      strings.TrimSpace(s.Name),
			Transport: strings.TrimSpace(s.Transport),
			Command:   s.Command,
			Args:      s.Args,
			Env:       s.Env,
			URL:       s.URL,
			Headers:   s.Headers,
		}
		if s.TimeoutSeconds > 0 {
			cfg.Timeout = time.Duration(s.TimeoutSeconds) * time.Second
		}
		if cfg.Name == "" {
			warnings = append(warnings, fmt.Sprintf("mcp: %s: entry %d: server name is required", path, i+1))
			continue
		}
		out = append(out, cfg)
	}
	return out, warnings
}
