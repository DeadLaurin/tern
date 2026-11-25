package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
	"github.com/tern-agent/tern/internal/tools"
)

// Budget is a concurrency-safe shared cost budget. A non-positive maximum
// means unlimited.
type Budget struct {
	mu    sync.Mutex
	max   float64
	spent float64
}

// NewBudget returns a budget with the given USD cap.
func NewBudget(max float64) *Budget { return &Budget{max: max} }

// Add records cost and reports whether the budget is now exhausted.
func (b *Budget) Add(cost float64) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	b.spent += cost
	exceeded := b.max > 0 && b.spent >= b.max
	b.mu.Unlock()
	return exceeded
}

// Spent returns total cost recorded so far.
func (b *Budget) Spent() float64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// Max returns the configured cap.
func (b *Budget) Max() float64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max
}

// Exceeded reports whether the cap has been reached.
func (b *Budget) Exceeded() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max > 0 && b.spent >= b.max
}

// Runner executes one sub-agent definition.
type Runner struct {
	ID               string
	Definition       Definition
	Task             Task
	Provider         provider.Provider
	Registry         *tools.Registry
	Permission       *permission.Evaluator
	Session          *session.Session
	ParentToolCallID string
	WorkDir          string
	Shell            string
	SearchBackend    string
	SearchEndpoint   string
	Approvals        *ApprovalQueue
	Locker           *PathLocker
	Budget           *Budget
	MaxSteps         int
	MaxContextTokens int
	ReasoningEffort  string
	CostFunc         func(model string, u provider.Usage) float64
	Emit             func(Event)
	OnStatus         func(status, currentTool string)

	model  string
	steps  int
	usage  provider.Usage
	cost   float64
	closed bool
}

// Run executes the agent and always returns a structured report.
func (r *Runner) Run(ctx context.Context) (report AgentReport) {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	report = AgentReport{
		AgentID:      r.ID,
		AgentType:    r.Definition.Name,
		Description:  r.Task.Description,
		Status:       StatusRunning,
		FilesRead:    []string{},
		FilesChanged: []string{},
	}

	var finishOnce bool
	finish := func() {
		if finishOnce {
			return
		}
		finishOnce = true
		report.Steps = r.steps
		report.Usage = r.usage
		report.CostUSD = r.cost
		report.Duration = time.Since(start)
		if report.FilesRead == nil {
			report.FilesRead = []string{}
		}
		if report.FilesChanged == nil {
			report.FilesChanged = []string{}
		}
		sort.Strings(report.FilesRead)
		sort.Strings(report.FilesChanged)
		report.FilesRead = uniqueSorted(report.FilesRead)
		report.FilesChanged = uniqueSorted(report.FilesChanged)
	}

	defer func() {
		if p := recover(); p != nil {
			report.Status = StatusFailed
			report.Error = fmt.Sprintf("panic: %v", p)
			if report.Summary == "" {
				report.Summary = "Sub-agent crashed: " + report.Error
			}
			finish()
		}
	}()

	workDir := strings.TrimSpace(r.Task.WorkDir)
	if workDir == "" {
		workDir = r.WorkDir
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	var workcopy *Workcopy
	if r.Task.Isolated && !r.Definition.ReadOnly {
		wc, err := PrepareWorkcopy(workDir)
		if err != nil {
			report.Status = StatusFailed
			report.Error = err.Error()
			report.Summary = "Could not create an isolated workcopy: " + err.Error()
			finish()
			return report
		}
		workcopy = wc
		workDir = wc.Dir
		defer func() {
			if workcopy != nil {
				_ = workcopy.Cleanup()
			}
		}()
	}

	model := strings.TrimSpace(r.Task.Model)
	if model == "" {
		model = strings.TrimSpace(r.Definition.Model)
	}
	if model == "" {
		model = "deepseek-flash"
	}
	r.model = model

	registry := r.buildRegistry()
	if len(registry.Names()) == 0 {
		report.Status = StatusFailed
		report.Error = "sub-agent has no available tools"
		report.Summary = report.Error
		finish()
		return report
	}

	maxSteps := r.MaxSteps
	if maxSteps <= 0 {
		maxSteps = r.Definition.MaxSteps
	}
	if maxSteps <= 0 {
		maxSteps = 50
	}

	system := provider.Message{Role: provider.RoleSystem, Content: r.Definition.SystemPrompt}
	userText := r.Task.Prompt
	if strings.TrimSpace(userText) == "" {
		userText = r.Task.Description
	}
	user := provider.Message{Role: provider.RoleUser, Content: userText}
	messages := []provider.Message{system, user}

	child := r.openChild(system, user)

	emit := r.emit

	maxTokens := r.MaxContextTokens
	if maxTokens <= 0 && r.Provider != nil {
		maxTokens = r.Provider.ModelInfo(model).ContextWindow
	}
	if maxTokens <= 0 {
		maxTokens = provider.DefaultContextWindow
	}

	var lastText string
	limitReason := ""
	failure := ""
	status := StatusRunning

	for step := 1; step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			status = statusFromContext(err)
			failure = contextFailure(err)
			break
		}
		if r.Budget != nil && r.Budget.Exceeded() {
			status = StatusLimitReached
			limitReason = fmt.Sprintf("session cost budget of $%.4f reached", r.Budget.Max())
			break
		}
		if estimateTokens(messages) > maxTokens*95/100 {
			status = StatusLimitReached
			limitReason = fmt.Sprintf("context window of %d tokens reached", maxTokens)
			break
		}

		r.steps = step
		r.setStatus(StatusRunning, "")
		emit(Event{Type: EventSubagentStep, AgentID: r.ID, AgentType: r.Definition.Name, Description: r.Task.Description, Status: StatusRunning, Step: step, Elapsed: time.Since(start)})

		req := provider.ChatRequest{
			Model:           model,
			Messages:        provider.PrepareMessagesForRequest(messages),
			Tools:           registry.ToolDefs(),
			ReasoningEffort: r.ReasoningEffort,
		}
		stream, err := r.stream(ctx, req)
		if err != nil {
			status = statusFromContext(ctx.Err())
			if status == StatusRunning {
				status = StatusFailed
			}
			failure = err.Error()
			break
		}

		text, reasoning, calls, usage, streamErr := r.consume(ctx, stream, start)
		if usage != nil {
			r.usage = addUsage(r.usage, *usage)
			cost := r.estimateCost(model, *usage)
			r.cost += cost
			exceeded := false
			if r.Budget != nil {
				exceeded = r.Budget.Add(cost)
			}
			if child != nil {
				_ = child.AppendUsage(*usage, cost, "")
			}
			if exceeded {
				limitReason = fmt.Sprintf("session cost budget of $%.4f reached", r.Budget.Max())
			}
		}
		if text != "" {
			lastText = text
			report.Summary = text
		}

		assistant := provider.Message{Role: provider.RoleAssistant, Content: text, ReasoningContent: reasoning}
		if len(calls) > 0 {
			assistant.ToolCalls = calls
		}
		messages = append(messages, assistant)
		r.appendChild(child, assistant, false)

		if streamErr != nil {
			status = statusFromContext(streamErr)
			if status == StatusRunning {
				status = StatusFailed
			}
			if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
				failure = contextFailure(streamErr)
			} else {
				failure = streamErr.Error()
			}
			break
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			status = statusFromContext(ctxErr)
			failure = contextFailure(ctxErr)
			break
		}
		if limitReason != "" {
			status = StatusLimitReached
			break
		}
		if len(calls) == 0 {
			status = StatusDone
			break
		}

		for _, call := range calls {
			if err := ctx.Err(); err != nil {
				failure = contextFailure(err)
				status = statusFromContext(err)
				break
			}
			if call.Function.Name == "" {
				continue
			}
			result, toolErr := r.executeTool(ctx, registry, call, workDir, start)
			output := result.Output
			if toolErr != nil {
				if errors.Is(toolErr, context.Canceled) || errors.Is(toolErr, context.DeadlineExceeded) {
					status = statusFromContext(ctx.Err())
					failure = contextFailure(toolErr)
				} else {
					output = "Error: " + toolErr.Error()
				}
			}
			if output == "" && toolErr == nil {
				output = "(no output)"
			}
			toolMsg := provider.Message{
				Role:       provider.RoleTool,
				Content:    tools.TruncateOnce(output, tools.DefaultOutputCap),
				ToolCallID: call.ID,
				Name:       call.Function.Name,
			}
			messages = append(messages, toolMsg)
			r.appendChild(child, toolMsg, result.External)
			r.collectFiles(result.Metadata, &report)
			if status != StatusRunning {
				break
			}
		}
		if status != StatusRunning {
			break
		}
		if step == maxSteps {
			status = StatusLimitReached
			limitReason = fmt.Sprintf("step limit of %d reached", maxSteps)
		}
	}

	if status == StatusRunning {
		status = StatusLimitReached
		limitReason = "sub-agent stopped without a final answer"
	}

	report.Status = status
	if limitReason != "" {
		report.Error = limitReason
		note := "Sub-agent stopped early: " + limitReason + "."
		if strings.TrimSpace(lastText) == "" {
			report.Summary = note
		} else {
			report.Summary = strings.TrimSpace(lastText) + "\n\n" + note
		}
	}
	if failure != "" {
		report.Error = failure
		if strings.TrimSpace(report.Summary) == "" {
			report.Summary = "Sub-agent failed: " + failure
		}
	}
	if report.Summary == "" {
		report.Summary = "(sub-agent produced no final summary)"
	}
	if workcopy != nil {
		patch, changed, err := workcopy.Patch()
		if err != nil {
			if report.Error == "" {
				report.Error = err.Error()
			}
		} else {
			report.Patch = patch
			report.FilesChanged = uniqueSorted(append(report.FilesChanged, changed...))
		}
	}
	finish()
	return report
}

// EffectiveRegistry returns the tool registry this runner would use.
func (r *Runner) EffectiveRegistry() *tools.Registry { return r.buildRegistry() }

func (r *Runner) stream(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	if r.Provider == nil {
		return nil, errors.New("subagent: no model provider configured")
	}
	return r.Provider.Stream(ctx, req)
}

func (r *Runner) consume(ctx context.Context, stream <-chan provider.StreamEvent, start time.Time) (string, string, []provider.ToolCall, *provider.Usage, error) {
	var text, reasoning strings.Builder
	var usage *provider.Usage
	var streamErr error
	byIndex := make(map[int]*partialToolCall)
	for {
		select {
		case <-ctx.Done():
			return text.String(), reasoning.String(), nil, usage, ctx.Err()
		case ev, ok := <-stream:
			if !ok {
				if streamErr != nil {
					return text.String(), reasoning.String(), nil, usage, streamErr
				}
				return text.String(), reasoning.String(), toolCallsFrom(byIndex), usage, nil
			}
			switch ev.Type {
			case provider.EventTextDelta:
				text.WriteString(ev.Text)
				r.emit(Event{Type: EventSubagentDelta, AgentID: r.ID, AgentType: r.Definition.Name, Description: r.Task.Description, Status: StatusRunning, Step: r.steps, Detail: ev.Text, Elapsed: time.Since(start)})
			case provider.EventReasoningDelta:
				reasoning.WriteString(ev.Text)
			case provider.EventToolCallDelta:
				pc := byIndex[ev.ToolCallIndex]
				if pc == nil {
					pc = &partialToolCall{order: ev.ToolCallIndex}
					byIndex[ev.ToolCallIndex] = pc
				}
				if ev.ToolCallID != "" {
					pc.id = ev.ToolCallID
				}
				if ev.ToolName != "" {
					pc.name = ev.ToolName
				}
				pc.args.WriteString(ev.ToolArgsDelta)
			case provider.EventUsage:
				if ev.Usage != nil {
					u := *ev.Usage
					if u.PromptCacheHitTokens == 0 && u.PromptCacheMissTokens == 0 && u.PromptTokens > 0 {
						u.PromptCacheMissTokens = u.PromptTokens
					}
					usage = &u
				}
			case provider.EventDone:
				if ev.Usage != nil {
					u := *ev.Usage
					if u.PromptCacheHitTokens == 0 && u.PromptCacheMissTokens == 0 && u.PromptTokens > 0 {
						u.PromptCacheMissTokens = u.PromptTokens
					}
					usage = &u
				}
				if ev.Err != nil {
					streamErr = ev.Err
				}
			case provider.EventError:
				if ev.Err != nil {
					streamErr = ev.Err
				} else {
					streamErr = errors.New("subagent: model stream error")
				}
			}
		}
	}
}

type partialToolCall struct {
	id    string
	name  string
	args  strings.Builder
	order int
}

func toolCallsFrom(byIndex map[int]*partialToolCall) []provider.ToolCall {
	if len(byIndex) == 0 {
		return nil
	}
	indexes := make([]int, 0, len(byIndex))
	for index := range byIndex {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	out := make([]provider.ToolCall, 0, len(indexes))
	for _, index := range indexes {
		pc := byIndex[index]
		if pc == nil || pc.name == "" {
			continue
		}
		id := pc.id
		if id == "" {
			id = fmt.Sprintf("call_%d", index)
		}
		args := pc.args.String()
		if strings.TrimSpace(args) == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		out = append(out, provider.ToolCall{
			ID:       id,
			Type:     "function",
			Function: provider.ToolCallFunction{Name: pc.name, Arguments: args},
		})
	}
	return out
}

func (r *Runner) executeTool(ctx context.Context, registry *tools.Registry, call provider.ToolCall, workDir string, start time.Time) (tools.Result, error) {
	tool, ok := registry.Get(call.Function.Name)
	if !ok {
		return tools.Result{}, fmt.Errorf("unknown tool %q", call.Function.Name)
	}
	raw := json.RawMessage(call.Function.Arguments)
	if len(raw) == 0 || !json.Valid(raw) {
		raw = json.RawMessage("{}")
	}
	path, command, detail := toolRequestFields(call.Function.Name, raw)

	if r.Permission != nil {
		decision := r.Permission.Check(permission.CheckRequest{
			ToolName:   tool.Name(),
			IsReadOnly: tool.IsReadOnly(),
			IsWeb:      tool.IsWeb(),
			Path:       path,
			Command:    command,
			WorkDir:    workDir,
			AgentName:  r.Definition.Name,
		})
		switch {
		case decision.IsDeny():
			return tools.Result{Output: "Permission denied: " + decision.Reason, IsError: true}, nil
		case decision.IsAsk():
			diff := ""
			if path != "" && !tool.IsReadOnly() {
				diff = previewDiff(workDir, call.Function.Name, raw)
			}
			if r.Approvals == nil {
				return tools.Result{Output: "Permission denied: approval required but no approval queue is configured", IsError: true}, nil
			}
			r.setStatus(StatusWaitingApproval, tool.Name())
			r.emit(Event{Type: EventSubagentApproval, AgentID: r.ID, AgentType: r.Definition.Name, Description: r.Task.Description, Status: StatusWaitingApproval, Step: r.steps, CurrentTool: tool.Name(), Detail: detail, Elapsed: time.Since(start)})
			ok, err := r.Approvals.Ask(ctx, ApprovalRequest{
				AgentID:         r.ID,
				AgentType:       r.Definition.Name,
				TaskDescription: r.Task.Description,
				ToolName:        tool.Name(),
				Detail:          detail,
				DiffPreview:     diff,
				Reason:          decision.Reason,
			})
			if err != nil {
				return tools.Result{}, err
			}
			if !ok {
				return tools.Result{Output: "Permission denied by user for " + tool.Name(), IsError: true}, nil
			}
			r.setStatus(StatusRunning, tool.Name())
		}
	}

	r.emit(Event{Type: EventSubagentStep, AgentID: r.ID, AgentType: r.Definition.Name, Description: r.Task.Description, Status: StatusRunning, Step: r.steps, CurrentTool: tool.Name(), Elapsed: time.Since(start)})
	r.setStatus(StatusRunning, tool.Name())

	release := func() {}
	if r.Locker != nil && !tool.IsReadOnly() && path != "" {
		var err error
		release, err = r.Locker.TryAcquireAs(path, "sub-agent "+r.ID)
		if err != nil {
			return tools.Result{Output: err.Error(), IsError: true}, nil
		}
	}
	defer release()

	env := tools.ExecEnv{WorkDir: workDir, Shell: r.Shell}
	res, err := tool.Execute(ctx, env, raw)
	if err != nil {
		return res, err
	}
	return res, nil
}

func (r *Runner) buildRegistry() *tools.Registry {
	base := r.Registry
	if base == nil {
		base = tools.NewRegistry()
		base.Register(NewWebSearchTool(nil, r.SearchBackend, r.SearchEndpoint))
		base.Register(NewWebFetchTool())
	}
	out := tools.NewEmptyRegistry()
	names := append([]string(nil), r.Definition.Tools...)
	sort.Strings(names)
	for _, name := range names {
		switch name {
		case ToolWebSearch:
			out.Register(NewWebSearchTool(nil, r.SearchBackend, r.SearchEndpoint))
		case ToolWebFetch:
			out.Register(NewWebFetchTool())
		default:
			tool, ok := base.Get(name)
			if !ok {
				continue
			}
			if name == tools.ToolBash && r.Definition.ReadOnly {
				tool = &readOnlyBashTool{inner: tool}
			}
			out.Register(tool)
		}
	}
	return out
}

func (r *Runner) openChild(system, user provider.Message) *session.Session {
	if r.Session == nil {
		return nil
	}
	child, err := r.Session.CreateChild(r.ParentToolCallID, r.ID, r.Definition.Name, r.Task.Description, r.Definition.SystemPrompt, r.model)
	if err != nil {
		return nil
	}
	_ = child.AppendMessage(system)
	_ = child.AppendMessage(user)
	return child
}

func (r *Runner) appendChild(child *session.Session, msg provider.Message, external bool) {
	if child == nil {
		return
	}
	_ = child.AppendMessageExternal(msg, external)
}

func (r *Runner) setStatus(status, currentTool string) {
	if r.OnStatus != nil {
		r.OnStatus(status, currentTool)
	}
}

func (r *Runner) emit(ev Event) {
	if r.Emit != nil {
		r.Emit(ev)
	}
}

func (r *Runner) estimateCost(model string, u provider.Usage) float64 {
	if r.CostFunc != nil {
		return r.CostFunc(model, u)
	}
	return 0
}

func (r *Runner) collectFiles(meta map[string]any, report *AgentReport) {
	if meta == nil {
		return
	}
	report.FilesRead = append(report.FilesRead, stringsFromMeta(meta["files_read"])...)
	report.FilesChanged = append(report.FilesChanged, stringsFromMeta(meta["files_changed"])...)
}

func stringsFromMeta(v any) []string {
	switch list := v.(type) {
	case []string:
		return append([]string(nil), list...)
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if list == "" {
			return nil
		}
		return []string{list}
	default:
		return nil
	}
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

func estimateTokens(messages []provider.Message) int {
	chars := 0
	for _, msg := range messages {
		chars += len(msg.Content) + len(msg.ReasoningContent)
		for _, call := range msg.ToolCalls {
			chars += len(call.Function.Name) + len(call.Function.Arguments)
		}
	}
	return chars/4 + len(messages)*4
}

func statusFromContext(err error) string {
	switch {
	case err == nil:
		return StatusRunning
	case errors.Is(err, context.DeadlineExceeded):
		return StatusLimitReached
	case errors.Is(err, context.Canceled):
		return StatusCancelled
	default:
		return StatusFailed
	}
}

func contextFailure(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "sub-agent timed out: " + err.Error()
	case errors.Is(err, context.Canceled):
		return "sub-agent cancelled"
	default:
		return err.Error()
	}
}

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	sort.Strings(in)
	out := in[:0]
	var last string
	for i, s := range in {
		if i == 0 || s != last {
			out = append(out, s)
			last = s
		}
	}
	return out
}

// readOnlyBashTool enforces permission.IsReadOnlyBashCommand even if the
// child's permission evaluator would otherwise allow bash for another reason.
type readOnlyBashTool struct {
	inner tools.Tool
}

func (t *readOnlyBashTool) Name() string                { return t.inner.Name() }
func (t *readOnlyBashTool) Description() string         { return t.inner.Description() }
func (t *readOnlyBashTool) Parameters() json.RawMessage { return t.inner.Parameters() }
func (t *readOnlyBashTool) IsReadOnly() bool            { return true }
func (t *readOnlyBashTool) IsWeb() bool                 { return false }
func (t *readOnlyBashTool) IsParallelSafe() bool        { return false }

func (t *readOnlyBashTool) Execute(ctx context.Context, env tools.ExecEnv, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.Result{}, fmt.Errorf("bash: invalid arguments: %w", err)
	}
	if !permission.IsReadOnlyBashCommand(args.Command) {
		return tools.Result{Output: fmt.Sprintf("read-only agent: command %q is not on the read-only allowlist", args.Command), IsError: true}, nil
	}
	return t.inner.Execute(ctx, env, raw)
}

func toolRequestFields(toolName string, raw json.RawMessage) (path, command, detail string) {
	var args struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		URL     string `json:"url"`
		Query   string `json:"query"`
	}
	_ = json.Unmarshal(raw, &args)
	switch toolName {
	case tools.ToolBash:
		return "", args.Command, args.Command
	case ToolWebFetch:
		return "", "", args.URL
	case ToolWebSearch:
		return "", "", args.Query
	default:
		return args.Path, "", args.Path
	}
}

func previewDiff(workDir, toolName string, raw json.RawMessage) string {
	var args struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
		Edits      []struct {
			OldString  string `json:"old_string"`
			NewString  string `json:"new_string"`
			ReplaceAll bool   `json:"replace_all"`
		} `json:"edits"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || strings.TrimSpace(args.Path) == "" {
		return ""
	}
	path := args.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(workDir, path)
	}
	data, _ := os.ReadFile(path)
	oldContent := string(data)
	newContent := oldContent
	switch toolName {
	case tools.ToolWrite:
		newContent = args.Content
	case tools.ToolEdit:
		newContent = strings.Replace(oldContent, args.OldString, args.NewString, replaceCount(args.ReplaceAll))
	case tools.ToolMultiEdit:
		for _, edit := range args.Edits {
			newContent = strings.Replace(newContent, edit.OldString, edit.NewString, replaceCount(edit.ReplaceAll))
		}
	default:
		return ""
	}
	diff := permission.UnifiedDiff(args.Path, oldContent, newContent)
	if len(diff) > 6000 {
		diff = tools.TruncateOnce(diff, 6000)
	}
	return diff
}

func replaceCount(all bool) int {
	if all {
		return -1
	}
	return 1
}
