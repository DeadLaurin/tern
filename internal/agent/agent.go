// Package agent implements tern's model-agnostic agent loop.
//
// The loop is deliberately small and explicit: it builds a stable request
// prefix (system prompt + tool definitions + append-only history), streams a
// response, runs tool calls under the permission engine, checkpoints file
// mutations, and persists every message to the session transcript. Context
// compaction keeps long sessions inside the model window while remaining
// reversible on disk.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/tern-agent/tern/internal/checkpoint"
	"github.com/tern-agent/tern/internal/config"
	terncontext "github.com/tern-agent/tern/internal/context"
	"github.com/tern-agent/tern/internal/ext"
	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
	"github.com/tern-agent/tern/internal/tools"
)

// StopReason explains why a turn ended.
type StopReason string

// Turn stop reasons.
const (
	StopComplete  StopReason = "complete"
	StopMaxSteps  StopReason = "max_steps"
	StopBudget    StopReason = "budget"
	StopCancelled StopReason = "cancelled"
	StopError     StopReason = "error"
)

// ApprovalRequest describes a tool call that needs explicit user approval.
type ApprovalRequest struct {
	ToolName  string          `json:"tool_name"`
	Args      json.RawMessage `json:"args,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Path      string          `json:"path,omitempty"`
	Command   string          `json:"command,omitempty"`
	Diff      string          `json:"diff,omitempty"`
	IsWeb     bool            `json:"is_web,omitempty"`
	ReadOnly  bool            `json:"read_only,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Turn      int             `json:"turn,omitempty"`
	Step      int             `json:"step,omitempty"`
}

// TurnResult summarizes a completed RunTurn call.
type TurnResult struct {
	Turn         int            `json:"turn"`
	Text         string         `json:"text,omitempty"`
	Reasoning    string         `json:"reasoning,omitempty"`
	Steps        int            `json:"steps"`
	ToolCalls    int            `json:"tool_calls"`
	Usage        provider.Usage `json:"usage"`
	CostUSD      float64        `json:"cost_usd"`
	TotalUsage   provider.Usage `json:"total_usage"`
	TotalCostUSD float64        `json:"total_cost_usd"`
	CacheHitRate float64        `json:"cache_hit_rate"`
	Compacted    bool           `json:"compacted,omitempty"`
	StopReason   StopReason     `json:"stop_reason"`
}

// Options configures a new Agent. All fields are optional except Provider.
type Options struct {
	Config       *config.Config
	Provider     provider.Provider
	Registry     *tools.Registry
	Permissions  *permission.Evaluator
	Checkpoints  *checkpoint.Store
	Session      *session.Session
	Hooks        *ext.Runner
	Todos        *tools.TodoStore
	WorkDir      string
	SystemPrompt string
	OutputCap    int
	OnEvent      func(Event)
	OnApprove    func(ctx context.Context, req ApprovalRequest) (bool, error)
	OnAskUser    func(ctx context.Context, question string, opts []string) (string, error)
}

// Agent is the core agent loop. Configure it directly or with New.
type Agent struct {
	Config       *config.Config
	Provider     provider.Provider
	Registry     *tools.Registry
	Permissions  *permission.Evaluator
	Checkpoints  *checkpoint.Store
	Session      *session.Session
	Hooks        *ext.Runner
	Todos        *tools.TodoStore
	WorkDir      string
	SystemPrompt string
	OutputCap    int
	OnEvent      func(Event)
	OnApprove    func(ctx context.Context, req ApprovalRequest) (bool, error)
	OnAskUser    func(ctx context.Context, question string, opts []string) (string, error)

	runMu sync.Mutex

	initialized bool
	history     []provider.Message
	turn        int

	totalUsage provider.Usage
	totalCost  float64

	lastPrefixHash      string
	lastSystemToolsHash string
	lastReqMessages     []provider.Message
	lastPromptTokens    int
}

// New creates an agent and loads any existing session history.
func New(opts Options) *Agent {
	a := &Agent{
		Config:       opts.Config,
		Provider:     opts.Provider,
		Registry:     opts.Registry,
		Permissions:  opts.Permissions,
		Checkpoints:  opts.Checkpoints,
		Session:      opts.Session,
		Hooks:        opts.Hooks,
		Todos:        opts.Todos,
		WorkDir:      opts.WorkDir,
		SystemPrompt: opts.SystemPrompt,
		OutputCap:    opts.OutputCap,
		OnEvent:      opts.OnEvent,
		OnApprove:    opts.OnApprove,
		OnAskUser:    opts.OnAskUser,
	}
	a.ensureInit()
	return a
}

// ensureInit fills defaults and restores history from the session. It is
// idempotent and must be called with a.runMu held (or before the agent is
// shared).
func (a *Agent) ensureInit() {
	if a.initialized {
		return
	}
	a.initialized = true

	if a.Config == nil {
		a.Config = config.Default()
	}
	if a.Registry == nil {
		a.Registry = tools.NewEmptyRegistry()
	}
	if strings.TrimSpace(a.WorkDir) == "" {
		if wd, err := os.Getwd(); err == nil {
			a.WorkDir = wd
		}
	}
	if abs, err := filepath.Abs(a.WorkDir); err == nil {
		a.WorkDir = abs
	}
	if a.OutputCap <= 0 {
		a.OutputCap = tools.DefaultOutputCap
	}
	if a.SystemPrompt == "" {
		a.SystemPrompt = SystemPrompt
	}

	if a.Todos == nil {
		if t, ok := a.Registry.Get(tools.ToolTodo); ok {
			if tt, ok := t.(*tools.TodoTool); ok {
				a.Todos = tt.Store()
			}
		}
	}
	if a.Todos == nil {
		a.Todos = tools.NewTodoStore()
	}
	// Keep the todo tool and the agent's view of the todo list in sync.
	if t, ok := a.Registry.Get(tools.ToolTodo); ok {
		if tt, ok := t.(*tools.TodoTool); !ok || tt.Store() != a.Todos {
			a.Registry.Register(tools.NewTodoToolWithStore(a.Todos))
		}
	}

	if a.Permissions == nil {
		mode, err := permission.ParseMode(a.Config.PermissionMode)
		if err != nil {
			mode = permission.DefaultMode
		}
		webPolicy, err := permission.ParseWebPolicy(a.Config.WebPermission)
		if err != nil {
			webPolicy = permission.DefaultWebPolicy
		}
		a.Permissions = permission.New(permission.Policy{
			Mode:    mode,
			Web:     webPolicy,
			WorkDir: a.WorkDir,
		})
	}

	if a.Session != nil {
		a.history = a.Session.ReplayMessages()
		a.totalUsage, a.totalCost = a.Session.Usage()
	}
	a.turn = countUserMessages(a.history)
}

// TotalUsage returns cumulative token usage and estimated cost for the
// session, including compaction calls.
func (a *Agent) TotalUsage() (provider.Usage, float64) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.ensureInit()
	return a.totalUsage, a.totalCost
}

// History returns a copy of the active history.
func (a *Agent) History() []provider.Message {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.ensureInit()
	return append([]provider.Message(nil), a.history...)
}

// RunTurn runs one full user turn: append the user message, stream model
// responses, execute tools, and persist everything.
func (a *Agent) RunTurn(ctx context.Context, userInput string, attachments []provider.ContentPart) (*TurnResult, error) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.ensureInit()

	if ctx == nil {
		ctx = context.Background()
	}
	if a.Provider == nil {
		err := errors.New("agent: no provider configured")
		a.emit(Event{Type: EventError, Message: err.Error()})
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Validate image attachments against the model's static capabilities.
	if hasImageParts(attachments) {
		caps := a.Provider.ModelInfo(a.Config.Model)
		if !caps.SupportsVision {
			err := fmt.Errorf("agent: model %q does not support image attachments (vision is disabled for this model)", a.Config.Model)
			a.emit(Event{Type: EventError, Message: err.Error()})
			return nil, err
		}
	}

	firstTurn := countUserMessages(a.history) == 0
	a.turn++
	turn := a.turn
	startUsage := a.totalUsage
	startCost := a.totalCost

	text := composeUserMessage(a, userInput, attachments, firstTurn)
	userMsg := provider.Message{Role: provider.RoleUser, Content: text}
	if len(attachments) > 0 {
		parts := make([]provider.ContentPart, 0, len(attachments)+1)
		parts = append(parts, provider.ContentPart{Type: "text", Text: text})
		parts = append(parts, attachments...)
		userMsg.Parts = parts
		userMsg.Content = ""
	}
	if err := a.appendMessage(userMsg, false); err != nil {
		a.emit(Event{Type: EventError, Turn: turn, Message: err.Error()})
		return nil, err
	}

	res := &TurnResult{Turn: turn, StopReason: StopComplete}
	a.emit(Event{Type: EventTurnStart, Turn: turn, Message: "turn started"})

	maxSteps := a.Config.MaxSteps
	if maxSteps <= 0 {
		maxSteps = config.DefaultMaxSteps
	}
	steps := 0
	compacted := false

	for {
		if err := ctx.Err(); err != nil {
			res.Compacted = compacted
			return a.finishTurn(res, turn, startUsage, startCost, StopCancelled, err)
		}
		if steps >= maxSteps {
			res.Compacted = compacted
			return a.finishTurn(res, turn, startUsage, startCost, StopMaxSteps, nil)
		}
		steps++

		req := a.buildRequest()
		a.cacheDebug(req, steps)

		sr, err := a.streamWithRetry(ctx, req, turn, steps)
		if err != nil {
			res.Compacted = compacted || errors.Is(err, provider.ErrContextTooLong)
			return a.finishTurn(res, turn, startUsage, startCost, StopError, err)
		}
		if sr.compacted {
			compacted = true
		}
		if sr.Usage.TotalTokens > 0 || sr.Usage.PromptTokens > 0 || sr.Usage.CompletionTokens > 0 {
			a.lastPromptTokens = sr.Usage.PromptTokens
		}

		assistant := provider.Message{
			Role:             provider.RoleAssistant,
			Content:          sr.Text,
			ReasoningContent: sr.Reasoning,
			ToolCalls:        sr.ToolCalls,
		}
		if err := a.appendMessage(assistant, false); err != nil {
			res.Compacted = compacted
			return a.finishTurn(res, turn, startUsage, startCost, StopError, err)
		}
		res.Text += sr.Text
		res.Reasoning += sr.Reasoning
		res.Steps = steps
		res.ToolCalls += len(sr.ToolCalls)

		// Budget is checked before tools run. Any pending calls get explicit
		// "not executed" tool results so the transcript stays valid.
		if a.Config.MaxCostUSD > 0 && a.totalCost >= a.Config.MaxCostUSD {
			a.appendSkippedToolResults(turn, steps, sr.ToolCalls, "cost budget exceeded")
			res.Compacted = compacted
			return a.finishTurn(res, turn, startUsage, startCost, StopBudget, nil)
		}

		if len(sr.ToolCalls) == 0 {
			res.Compacted = compacted
			return a.finishTurn(res, turn, startUsage, startCost, StopComplete, nil)
		}

		a.runToolBatch(ctx, turn, steps, sr.ToolCalls)

		// Auto compaction is checked after the tool batch, before the next
		// model request.
		if a.shouldAutoCompact() {
			if _, cerr := a.compactInternal(ctx, "automatic compaction: context threshold reached", "auto"); cerr != nil {
				a.emit(Event{Type: EventError, Turn: turn, Step: steps, Message: "auto compaction failed: " + cerr.Error()})
			} else {
				compacted = true
			}
		}
	}
}

// finishTurn records the stop reason and emits turn_end (and an error event
// when err is non-nil).
func (a *Agent) finishTurn(res *TurnResult, turn int, startUsage provider.Usage, startCost float64, reason StopReason, err error) (*TurnResult, error) {
	res.StopReason = reason
	res.Usage = usageDiff(a.totalUsage, startUsage)
	res.CostUSD = a.totalCost - startCost
	res.TotalUsage = a.totalUsage
	res.TotalCostUSD = a.totalCost
	res.CacheHitRate = cacheHitRate(a.totalUsage)

	if err != nil {
		a.emit(Event{Type: EventError, Turn: turn, Message: err.Error(), Reason: string(reason)})
	}
	usage := res.Usage
	a.emit(Event{
		Type:         EventTurnEnd,
		Turn:         turn,
		Reason:       string(reason),
		Usage:        &usage,
		CostUSD:      res.CostUSD,
		TotalCostUSD: res.TotalCostUSD,
		CacheHitRate: res.CacheHitRate,
		Message:      res.Text,
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// streamResult is one assistant response accumulated from the stream.
type streamResult struct {
	Text      string
	Reasoning string
	ToolCalls []provider.ToolCall
	Usage     provider.Usage
	compacted bool
}

// streamWithRetry streams one response. On provider.ErrContextTooLong it
// compacts the context and retries exactly once.
func (a *Agent) streamWithRetry(ctx context.Context, req provider.ChatRequest, turn, step int) (*streamResult, error) {
	sr, err := a.streamOnce(ctx, req, turn, step)
	if err == nil {
		return sr, nil
	}
	if !errors.Is(err, provider.ErrContextTooLong) {
		return nil, err
	}

	a.emit(Event{
		Type:    EventError,
		Turn:    turn,
		Step:    step,
		Message: "context window exceeded; compacting and retrying once",
		Reason:  "context_too_long",
	})
	if _, cerr := a.compactInternal(ctx, "context window exceeded", "context_too_long"); cerr != nil {
		return nil, errors.Join(err, fmt.Errorf("agent: automatic compaction failed: %w", cerr))
	}

	req = a.buildRequest()
	a.cacheDebug(req, step)
	sr, err = a.streamOnce(ctx, req, turn, step)
	if err != nil {
		return nil, err
	}
	sr.compacted = true
	return sr, nil
}

// streamOnce performs one provider stream and emits live deltas and usage.
func (a *Agent) streamOnce(ctx context.Context, req provider.ChatRequest, turn, step int) (*streamResult, error) {
	ch, err := a.Provider.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	sr := &streamResult{}
	var calls []*provider.ToolCall
	for ev := range ch {
		switch ev.Type {
		case provider.EventReasoningDelta:
			sr.Reasoning += ev.Text
			a.emit(Event{Type: EventReasoningDelta, Turn: turn, Step: step, Text: ev.Text})
		case provider.EventTextDelta:
			sr.Text += ev.Text
			a.emit(Event{Type: EventTextDelta, Turn: turn, Step: step, Text: ev.Text})
		case provider.EventToolCallDelta:
			for len(calls) <= ev.ToolCallIndex {
				calls = append(calls, &provider.ToolCall{Type: "function"})
			}
			call := calls[ev.ToolCallIndex]
			if ev.ToolCallID != "" {
				call.ID = ev.ToolCallID
			}
			if ev.ToolName != "" {
				call.Function.Name = ev.ToolName
			}
			call.Function.Arguments += ev.ToolArgsDelta
		case provider.EventUsage:
			if ev.Usage != nil {
				sr.Usage = *ev.Usage
				a.recordUsage(req, *ev.Usage, turn, step)
			}
		case provider.EventError:
			if ev.Err != nil {
				return sr, ev.Err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return sr, err
	}
	for i, call := range calls {
		if call == nil {
			continue
		}
		if call.ID == "" {
			call.ID = fmt.Sprintf("call_%d_%d", turn, i)
		}
		if call.Type == "" {
			call.Type = "function"
		}
		sr.ToolCalls = append(sr.ToolCalls, *call)
	}
	return sr, nil
}

// buildRequest constructs the deterministic chat request.
func (a *Agent) buildRequest() provider.ChatRequest {
	messages := make([]provider.Message, 0, len(a.history)+1)
	messages = append(messages, provider.Message{Role: provider.RoleSystem, Content: a.systemPrompt()})
	messages = append(messages, provider.PrepareMessagesForRequest(a.history)...)
	return provider.ChatRequest{
		Model:           a.Config.Model,
		Messages:        messages,
		Tools:           a.Registry.ToolDefs(),
		ReasoningEffort: a.Config.ReasoningEffort,
		Stream:          true,
	}
}

func (a *Agent) systemPrompt() string {
	if strings.TrimSpace(a.SystemPrompt) != "" {
		return a.SystemPrompt
	}
	return SystemPrompt
}

// cacheDebug computes prefix hashes and warns about unexpected divergence.
func (a *Agent) cacheDebug(req provider.ChatRequest, step int) {
	if !a.Config.DebugCache {
		return
	}
	prefix, _ := provider.PrefixHash(req)
	sysTools, _ := provider.SystemToolsHash(req)

	warning := ""
	if a.lastSystemToolsHash != "" && sysTools != a.lastSystemToolsHash {
		warning = "system prompt or tool definitions changed unexpectedly; cache prefix was invalidated"
	}
	if step > 1 && a.lastReqMessages != nil && !messagesPrefix(a.lastReqMessages, req.Messages) {
		if warning == "" {
			warning = "request prefix diverged from the previous step"
		}
	}
	a.emit(Event{
		Type:            EventCacheDebug,
		Turn:            a.turn,
		Step:            step,
		PrefixHash:      prefix,
		SystemToolsHash: sysTools,
		CacheHitRate:    cacheHitRate(a.totalUsage),
		Message:         warning,
	})
	if warning != "" {
		a.emit(Event{
			Type:    EventError,
			Turn:    a.turn,
			Step:    step,
			Reason:  "cache_prefix_diverged",
			Message: "cache warning: " + warning,
		})
	}
	a.lastPrefixHash = prefix
	a.lastSystemToolsHash = sysTools
	a.lastReqMessages = append([]provider.Message(nil), req.Messages...)
}

// recordUsage accumulates accounting and emits a live usage event.
func (a *Agent) recordUsage(req provider.ChatRequest, u provider.Usage, turn, step int) {
	cost := a.costForUsage(u, time.Now())
	a.addUsage(u, cost)
	a.lastPromptTokens = u.PromptTokens

	prefix, _ := provider.PrefixHash(req)
	if a.Session != nil {
		_ = a.Session.AppendUsage(u, cost, prefix)
	}
	usage := u
	a.emit(Event{
		Type:         EventUsage,
		Turn:         turn,
		Step:         step,
		Usage:        &usage,
		CostUSD:      cost,
		TotalCostUSD: a.totalCost,
		CacheHitRate: cacheHitRate(a.totalUsage),
	})
}

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

type preparedCall struct {
	call    provider.ToolCall
	tool    tools.Tool
	ready   bool
	blocked tools.Result
}

type toolOutcome struct {
	result   tools.Result
	duration time.Duration
	executed bool
}

// runToolBatch evaluates permissions and hooks, executes tools in deterministic
// order, and appends every tool result to the transcript.
func (a *Agent) runToolBatch(ctx context.Context, turn, step int, calls []provider.ToolCall) {
	prepared := make([]preparedCall, len(calls))
	for i, call := range calls {
		prepared[i].call = call
		tool, ok := a.Registry.Get(call.Function.Name)
		if !ok {
			prepared[i].blocked = tools.Result{
				Output:  fmt.Sprintf("unknown tool %q", call.Function.Name),
				IsError: true,
			}
			continue
		}
		prepared[i].tool = tool

		args := json.RawMessage(call.Function.Arguments)
		path := extractPath(args)
		command := extractCommand(args)
		decision := a.Permissions.Check(permission.CheckRequest{
			ToolName:   call.Function.Name,
			IsReadOnly: tool.IsReadOnly(),
			IsWeb:      tool.IsWeb(),
			Path:       path,
			Command:    command,
			WorkDir:    a.WorkDir,
		})

		switch {
		case decision.IsDeny():
			prepared[i].blocked = tools.Result{
				Output:  "permission denied: " + decision.Reason,
				IsError: true,
			}
			continue
		case decision.IsAsk():
			if a.OnApprove == nil {
				prepared[i].blocked = tools.Result{
					Output:  "permission required but no interactive approver is available: " + decision.Reason,
					IsError: true,
				}
				continue
			}
			approval := ApprovalRequest{
				ToolName:  call.Function.Name,
				Args:      args,
				Reason:    decision.Reason,
				Path:      path,
				Command:   command,
				IsWeb:     tool.IsWeb(),
				ReadOnly:  tool.IsReadOnly(),
				SessionID: a.sessionID(),
				Turn:      turn,
				Step:      step,
			}
			if !tool.IsReadOnly() {
				approval.Diff = a.diffPreview(call.Function.Name, args)
			}
			allowed, err := a.OnApprove(ctx, approval)
			if err != nil {
				prepared[i].blocked = tools.Result{
					Output:  "approval failed: " + err.Error(),
					IsError: true,
				}
				continue
			}
			if !allowed {
				prepared[i].blocked = tools.Result{
					Output:  "user denied the tool call: " + decision.Reason,
					IsError: true,
				}
				continue
			}
		}

		if a.Hooks != nil {
			hr, err := a.Hooks.RunBeforeTool(ctx, call.Function.Name, args)
			if err != nil || !hr.Allow {
				msg := strings.TrimSpace(hr.Message)
				if msg == "" && err != nil {
					msg = err.Error()
				}
				if msg == "" {
					msg = "blocked by before_tool hook"
				}
				prepared[i].blocked = tools.Result{Output: msg, IsError: true}
				continue
			}
		}
		prepared[i].ready = true
	}

	anyMutating := false
	for _, p := range prepared {
		if p.ready && p.tool != nil && !p.tool.IsReadOnly() {
			anyMutating = true
			break
		}
	}

	var snap *checkpoint.TurnSnapshot
	if anyMutating && a.Checkpoints != nil {
		turnID := len(a.Checkpoints.List()) + 1
		snap = a.Checkpoints.BeginTurn(turnID, toolSummary(prepared))
	}

	env := tools.ExecEnv{
		WorkDir:      a.WorkDir,
		Shell:        a.Config.Shell,
		OutputCap:    a.OutputCap,
		AskUser:      a.OnAskUser,
		OnTodoUpdate: func(items []tools.TodoItem) {},
	}
	if snap != nil {
		env.BeforeEdit = snap.RecordBefore
	}

	outcomes := make([]toolOutcome, len(prepared))
	for i := 0; i < len(prepared); {
		if !prepared[i].ready {
			i++
			continue
		}
		if prepared[i].tool.IsReadOnly() && prepared[i].tool.IsParallelSafe() {
			j := i
			for j < len(prepared) && prepared[j].ready && prepared[j].tool.IsReadOnly() && prepared[j].tool.IsParallelSafe() {
				j++
			}
			var wg sync.WaitGroup
			for k := i; k < j; k++ {
				a.emitToolStart(turn, step, prepared[k].call)
				wg.Add(1)
				go func(k int) {
					defer wg.Done()
					outcomes[k] = a.executeTool(ctx, env, prepared[k])
				}(k)
			}
			wg.Wait()
			i = j
			continue
		}
		a.emitToolStart(turn, step, prepared[i].call)
		outcomes[i] = a.executeTool(ctx, env, prepared[i])
		i++
	}

	if snap != nil {
		if err := a.Checkpoints.CommitTurn(snap); err != nil {
			a.emit(Event{Type: EventError, Turn: turn, Step: step, Message: "checkpoint commit failed: " + err.Error()})
		}
	}

	// Results are appended in exact tool_calls order, preserving determinism
	// even when read-only tools ran concurrently.
	for i, p := range prepared {
		if !p.ready {
			a.finishToolCall(turn, step, p.call, p.tool, toolOutcome{result: p.blocked})
			continue
		}
		a.finishToolCall(turn, step, p.call, p.tool, outcomes[i])
	}
}

func (a *Agent) emitToolStart(turn, step int, call provider.ToolCall) {
	a.emit(Event{
		Type:       EventToolStart,
		Turn:       turn,
		Step:       step,
		ToolCallID: call.ID,
		ToolName:   call.Function.Name,
		Args:       json.RawMessage(call.Function.Arguments),
	})
}

func (a *Agent) executeTool(ctx context.Context, env tools.ExecEnv, p preparedCall) toolOutcome {
	start := time.Now()
	res, err := p.tool.Execute(ctx, env, json.RawMessage(p.call.Function.Arguments))
	duration := time.Since(start)
	if err != nil {
		res = tools.Result{
			Output:   "error: " + err.Error(),
			IsError:  true,
			External: p.tool.IsWeb(),
		}
	}
	if p.tool.IsWeb() {
		res.External = true
	}
	return toolOutcome{result: res, duration: duration, executed: true}
}

// finishToolCall runs after_tool hooks, emits tool_end, and appends the tool
// message (truncated once at append time).
func (a *Agent) finishToolCall(turn, step int, call provider.ToolCall, tool tools.Tool, outcome toolOutcome) {
	res := outcome.result
	output := truncateResult(res.Output, a.OutputCap)
	display := output
	if res.External {
		display = wrapExternal(display)
	}

	if outcome.executed && a.Hooks != nil {
		_, _ = a.Hooks.Run(context.Background(), ext.HookInput{
			Event:     ext.HookAfterTool,
			Tool:      call.Function.Name,
			SessionID: a.sessionID(),
			CWD:       a.WorkDir,
			Args:      json.RawMessage(call.Function.Arguments),
			Extra: map[string]any{
				"output":   display,
				"is_error": res.IsError,
				"external": res.External,
			},
		})
	}

	a.emit(Event{
		Type:       EventToolEnd,
		Turn:       turn,
		Step:       step,
		ToolCallID: call.ID,
		ToolName:   call.Function.Name,
		Output:     display,
		IsError:    res.IsError,
		External:   res.External,
		DurationMS: outcome.duration.Milliseconds(),
	})

	msg := provider.Message{
		Role:       provider.RoleTool,
		Name:       call.Function.Name,
		ToolCallID: call.ID,
		Content:    display,
	}
	if err := a.appendMessage(msg, res.External); err != nil {
		a.emit(Event{Type: EventError, Turn: turn, Step: step, Message: err.Error()})
	}
}

func (a *Agent) appendSkippedToolResults(turn, step int, calls []provider.ToolCall, reason string) {
	for _, call := range calls {
		tool, _ := a.Registry.Get(call.Function.Name)
		a.finishToolCall(turn, step, call, tool, toolOutcome{
			result: tools.Result{
				Output:  "not executed: " + reason,
				IsError: true,
			},
		})
	}
}

// appendMessage appends to the in-memory history and persists it.
func (a *Agent) appendMessage(msg provider.Message, external bool) error {
	a.history = append(a.history, msg)
	if a.Session != nil {
		if err := a.Session.AppendMessageExternal(msg, external); err != nil {
			return fmt.Errorf("agent: persist message: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Context and accounting helpers
// ---------------------------------------------------------------------------

func composeUserMessage(a *Agent, input string, attachments []provider.ContentPart, firstTurn bool) string {
	text := input
	if !firstTurn {
		return text
	}
	var blocks []string
	if env := terncontext.BuildInitialContext(a.WorkDir, time.Now()); env != "" {
		blocks = append(blocks, env)
	}
	if instructions := a.projectInstructions(); instructions != "" {
		blocks = append(blocks, instructions)
	}
	if text != "" {
		blocks = append(blocks, text)
	}
	return strings.Join(blocks, "\n\n")
}

func hasImageParts(parts []provider.ContentPart) bool {
	for _, part := range parts {
		if part.ImageURL != nil || part.Type == "image_url" || part.Type == "image" {
			return true
		}
	}
	return false
}

func countUserMessages(history []provider.Message) int {
	n := 0
	for _, msg := range history {
		if msg.Role == provider.RoleUser {
			n++
		}
	}
	return n
}

func (a *Agent) workDir() string {
	if strings.TrimSpace(a.WorkDir) != "" {
		return a.WorkDir
	}
	wd, _ := os.Getwd()
	return wd
}

func (a *Agent) sessionID() string {
	if a.Session == nil {
		return ""
	}
	return a.Session.ID()
}

func (a *Agent) emit(ev Event) {
	if a.OnEvent == nil {
		return
	}
	a.OnEvent(ev)
}

func (a *Agent) costForUsage(u provider.Usage, now time.Time) float64 {
	miss := u.PromptCacheMissTokens
	if miss == 0 {
		miss = u.PromptTokens - u.PromptCacheHitTokens
		if miss < 0 {
			miss = 0
		}
	}
	return a.Config.EstimateCost(a.Config.Model, u.PromptCacheHitTokens, miss, u.CompletionTokens, now)
}

func (a *Agent) addUsage(u provider.Usage, cost float64) {
	a.totalUsage = addUsage(a.totalUsage, u)
	a.totalCost += cost
}

func addUsage(a, b provider.Usage) provider.Usage {
	return provider.Usage{
		PromptTokens:          a.PromptTokens + b.PromptTokens,
		CompletionTokens:      a.CompletionTokens + b.CompletionTokens,
		TotalTokens:           a.TotalTokens + b.TotalTokens,
		PromptCacheHitTokens:  a.PromptCacheHitTokens + b.PromptCacheHitTokens,
		PromptCacheMissTokens: a.PromptCacheMissTokens + b.PromptCacheMissTokens,
		ReasoningTokens:       a.ReasoningTokens + b.ReasoningTokens,
	}
}

func usageDiff(total, start provider.Usage) provider.Usage {
	return provider.Usage{
		PromptTokens:          total.PromptTokens - start.PromptTokens,
		CompletionTokens:      total.CompletionTokens - start.CompletionTokens,
		TotalTokens:           total.TotalTokens - start.TotalTokens,
		PromptCacheHitTokens:  total.PromptCacheHitTokens - start.PromptCacheHitTokens,
		PromptCacheMissTokens: total.PromptCacheMissTokens - start.PromptCacheMissTokens,
		ReasoningTokens:       total.ReasoningTokens - start.ReasoningTokens,
	}
}

func cacheHitRate(u provider.Usage) float64 {
	total := u.PromptCacheHitTokens + u.PromptCacheMissTokens
	if total <= 0 {
		return 0
	}
	return float64(u.PromptCacheHitTokens) / float64(total)
}

func (a *Agent) shouldAutoCompact() bool {
	if a.Provider == nil {
		return false
	}
	threshold := a.Config.AutoCompactThreshold
	if threshold <= 0 {
		threshold = config.DefaultAutoCompactThreshold
	}
	caps := a.Provider.ModelInfo(a.Config.Model)
	window := caps.ContextWindow
	if window <= 0 {
		window = provider.DefaultContextWindow
	}
	tokens := a.lastPromptTokens
	if estimated := a.estimateTokens(); estimated > tokens {
		tokens = estimated
	}
	return tokens > 0 && float64(tokens) >= threshold*float64(window)
}

func (a *Agent) estimateTokens() int {
	chars := len(a.systemPrompt())
	for _, msg := range a.history {
		chars += len(msg.Content) + len(msg.ReasoningContent)
		for _, part := range msg.Parts {
			chars += len(part.Text)
		}
		for _, tc := range msg.ToolCalls {
			chars += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	for _, def := range a.Registry.ToolDefs() {
		chars += len(def.Name) + len(def.Description) + len(def.Parameters)
	}
	return chars / 4
}

func truncateResult(s string, maxChars int) string {
	// Tools may already have applied their own limit. Avoid stacking a second
	// truncation marker on top.
	if strings.Contains(s, "[... truncated:") || strings.Contains(s, "[pruned tool output:") {
		return s
	}
	return tools.TruncateOnce(s, maxChars)
}

func wrapExternal(s string) string {
	return "<untrusted_external_content>\n" + s + "\n</untrusted_external_content>"
}

func messagesPrefix(prev, next []provider.Message) bool {
	if len(prev) > len(next) {
		return false
	}
	for i := range prev {
		if !reflect.DeepEqual(prev[i], next[i]) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Tool argument helpers
// ---------------------------------------------------------------------------

type genericToolArgs struct {
	Path     string `json:"path"`
	FilePath string `json:"file_path"`
	Command  string `json:"command"`
}

func parseGenericArgs(raw json.RawMessage) genericToolArgs {
	var args genericToolArgs
	if len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &args)
	}
	return args
}

func extractPath(raw json.RawMessage) string {
	args := parseGenericArgs(raw)
	if args.Path != "" {
		return args.Path
	}
	return args.FilePath
}

func extractCommand(raw json.RawMessage) string {
	return parseGenericArgs(raw).Command
}

func (a *Agent) absPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(a.workDir(), path)
}

// diffPreview renders a best-effort unified diff for file-mutating tools. It is
// only used for approval prompts; the tool remains the authority on the edit.
func (a *Agent) diffPreview(toolName string, raw json.RawMessage) string {
	switch toolName {
	case tools.ToolWrite:
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(raw, &args); err != nil || args.Path == "" {
			return ""
		}
		old, _ := os.ReadFile(a.absPath(args.Path))
		return permission.UnifiedDiff(args.Path, string(old), args.Content)
	case tools.ToolEdit:
		var args struct {
			Path       string `json:"path"`
			OldString  string `json:"old_string"`
			NewString  string `json:"new_string"`
			ReplaceAll bool   `json:"replace_all"`
		}
		if err := json.Unmarshal(raw, &args); err != nil || args.Path == "" || args.OldString == "" {
			return ""
		}
		data, err := os.ReadFile(a.absPath(args.Path))
		if err != nil {
			return ""
		}
		content := string(data)
		if !strings.Contains(content, args.OldString) {
			return ""
		}
		n := 1
		if args.ReplaceAll {
			n = -1
		}
		updated := strings.Replace(content, args.OldString, args.NewString, n)
		return permission.UnifiedDiff(args.Path, content, updated)
	case tools.ToolMultiEdit:
		var args struct {
			Path  string `json:"path"`
			Edits []struct {
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			} `json:"edits"`
		}
		if err := json.Unmarshal(raw, &args); err != nil || args.Path == "" || len(args.Edits) == 0 {
			return ""
		}
		data, err := os.ReadFile(a.absPath(args.Path))
		if err != nil {
			return ""
		}
		content := string(data)
		for _, edit := range args.Edits {
			if edit.OldString == "" || !strings.Contains(content, edit.OldString) {
				return ""
			}
			n := 1
			if edit.ReplaceAll {
				n = -1
			}
			content = strings.Replace(content, edit.OldString, edit.NewString, n)
		}
		return permission.UnifiedDiff(args.Path, string(data), content)
	default:
		return ""
	}
}

func toolSummary(prepared []preparedCall) string {
	names := make([]string, 0, len(prepared))
	for _, p := range prepared {
		if p.ready && p.tool != nil && !p.tool.IsReadOnly() {
			names = append(names, p.call.Function.Name)
		}
	}
	if len(names) == 0 {
		return "tool batch"
	}
	return "tools: " + strings.Join(names, ", ")
}
