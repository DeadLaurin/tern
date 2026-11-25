package subagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
	"github.com/tern-agent/tern/internal/tools"
)

// Default concurrency and depth limits.
const (
	DefaultMaxAgents = config.DefaultMaxAgents
	DefaultMaxDepth  = config.DefaultMaxAgentDepth
	// HardMaxDepth is the absolute recursion cap even if configuration asks
	// for more. Sub-agents are not allowed to build arbitrary agent trees.
	HardMaxDepth = 5
)

// Config configures a Manager.
type Config struct {
	MaxAgents int
	MaxDepth  int

	MaxCostUSD float64
	MaxSteps   int

	DefaultTimeout  time.Duration
	Model           string
	ReasoningEffort string

	WorkDir         string
	Shell           string
	GlobalConfigDir string

	Provider    provider.Provider
	Registry    *tools.Registry
	Permission  *permission.Evaluator
	Session     *session.Session
	Budget      *Budget
	Approvals   *ApprovalQueue
	Locker      *PathLocker
	Emit        func(Event)
	CostFunc    func(model string, u provider.Usage) float64
	Definitions []Definition

	SearchBackend  string
	SearchEndpoint string

	// PlanMode forces every sub-agent into read-only plan mode.
	PlanMode bool
}

// AgentStatus is a snapshot of one known sub-agent.
type AgentStatus struct {
	ID          string
	AgentType   string
	Description string
	Status      string
	Step        int
	CurrentTool string
	StartedAt   time.Time
	Usage       provider.Usage
	CostUSD     float64
	Error       string
	Summary     string
}

// Manager owns the concurrency pool and the active/completed agent registry.
type Manager struct {
	cfg    Config
	budget *Budget
	sem    chan struct{}
	locker *PathLocker
	queue  *ApprovalQueue

	mu        sync.Mutex
	agents    map[string]*agentRuntime
	order     []string
	completed map[string]AgentReport
	seq       int64
	stopped   bool
	defs      []Definition
	defsErr   error
	wg        sync.WaitGroup
}

type agentRuntime struct {
	id          string
	def         Definition
	task        Task
	ctx         context.Context
	cancel      context.CancelFunc
	status      string
	step        int
	currentTool string
	started     time.Time
	report      *AgentReport
	done        chan struct{}
}

// NewManager builds a manager from cfg, filling in safe defaults.
func NewManager(cfg Config) *Manager {
	if cfg.MaxAgents <= 0 {
		cfg.MaxAgents = DefaultMaxAgents
	}
	if cfg.MaxDepth < 0 {
		cfg.MaxDepth = DefaultMaxDepth
	}
	if cfg.MaxDepth == 0 {
		cfg.MaxDepth = DefaultMaxDepth
	}
	if cfg.MaxDepth > HardMaxDepth {
		cfg.MaxDepth = HardMaxDepth
	}
	if strings.TrimSpace(cfg.WorkDir) == "" {
		cfg.WorkDir, _ = os.Getwd()
	}
	budget := cfg.Budget
	if budget == nil {
		budget = NewBudget(cfg.MaxCostUSD)
		if cfg.Session != nil && cfg.MaxCostUSD > 0 {
			_, priorCost := cfg.Session.TotalUsage()
			budget.Add(priorCost)
		}
	}
	locker := cfg.Locker
	if locker == nil {
		locker = NewPathLocker()
	}
	queue := cfg.Approvals
	if queue == nil {
		queue = NewApprovalQueue()
	}
	m := &Manager{
		cfg:       cfg,
		budget:    budget,
		sem:       make(chan struct{}, cfg.MaxAgents),
		locker:    locker,
		queue:     queue,
		agents:    make(map[string]*agentRuntime),
		completed: make(map[string]AgentReport),
	}
	if len(cfg.Definitions) > 0 {
		m.defs = cloneDefinitions(cfg.Definitions)
	}
	return m
}

// Config returns the effective configuration.
func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// SetPermission updates the parent permission evaluator inherited by new
// sub-agents (for example when toggling YOLO or plan mode at runtime).
func (m *Manager) SetPermission(p *permission.Evaluator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Permission = p
	m.cfg.PlanMode = p != nil && p.Mode() == permission.ModePlan
}

// SetModel updates the default model used by new sub-agents.
func (m *Manager) SetModel(model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(model) != "" {
		m.cfg.Model = strings.TrimSpace(model)
	}
}

// SetReasoningEffort updates the default reasoning effort used by new
// sub-agents.
func (m *Manager) SetReasoningEffort(effort string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(effort) != "" {
		m.cfg.ReasoningEffort = strings.TrimSpace(effort)
	}
}

// Budget returns the shared cost budget.
func (m *Manager) Budget() *Budget { return m.budget }

// Locker returns the shared write-lock table.
func (m *Manager) Locker() *PathLocker { return m.locker }

// Approvals returns the serialized approval queue.
func (m *Manager) Approvals() *ApprovalQueue { return m.queue }

// SetDefinitions replaces the known agent definitions.
func (m *Manager) SetDefinitions(defs []Definition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defs = cloneDefinitions(defs)
	m.defsErr = nil
}

// Definitions returns the loaded definitions sorted by name.
func (m *Manager) Definitions() ([]Definition, error) {
	m.mu.Lock()
	if m.defs != nil {
		defs := cloneDefinitions(m.defs)
		err := m.defsErr
		m.mu.Unlock()
		return defs, err
	}
	m.mu.Unlock()

	loader := NewLoader(m.cfg.GlobalConfigDir, m.cfg.WorkDir)
	defs, err := loader.Load()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.defs == nil {
		m.defs = cloneDefinitions(defs)
		m.defsErr = err
	}
	return cloneDefinitions(m.defs), m.defsErr
}

// Run executes one task, waiting for a pool slot when necessary.
func (m *Manager) Run(ctx context.Context, task Task) AgentReport {
	if ctx == nil {
		ctx = context.Background()
	}
	defs, err := m.Definitions()
	if err != nil {
		return failedReport("", task, "load agent definitions: "+err.Error())
	}
	name := strings.TrimSpace(task.Agent)
	if name == "" {
		name = AgentGeneral
	}
	task.Agent = name
	def, ok := findDefinition(defs, name)
	if !ok {
		return failedReport("", task, fmt.Sprintf("unknown agent type %q", name))
	}
	def = applyTaskOverrides(def, task)

	if task.Depth < 0 {
		task.Depth = 0
	}
	if task.Depth >= m.cfg.MaxDepth {
		return failedReport("", task, fmt.Sprintf("maximum sub-agent depth of %d reached", m.cfg.MaxDepth))
	}

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return cancelledReport("", task, "manager has been cancelled")
	}
	m.seq++
	id := fmt.Sprintf("agent-%d", m.seq)
	childCtx, cancel := context.WithCancel(ctx)
	if task.Timeout <= 0 {
		task.Timeout = m.cfg.DefaultTimeout
	}
	if task.Timeout > 0 {
		childCtx, cancel = context.WithTimeout(ctx, task.Timeout)
	}
	rt := &agentRuntime{
		id:      id,
		def:     def,
		task:    task,
		ctx:     childCtx,
		cancel:  cancel,
		status:  StatusQueued,
		started: time.Now(),
		done:    make(chan struct{}),
	}
	m.agents[id] = rt
	m.order = append(m.order, id)
	m.wg.Add(1)
	m.mu.Unlock()

	finish := func(report AgentReport) AgentReport {
		report.AgentID = id
		report.AgentType = def.Name
		if report.Description == "" {
			report.Description = task.Description
		}
		cancel()
		m.mu.Lock()
		rt.status = report.Status
		rt.report = &report
		m.completed[id] = report
		m.mu.Unlock()
		m.recordLink(task.ParentToolCallID, report)
		m.emit(Event{
			Type: EventSubagentEnd, AgentID: id, ParentToolCallID: task.ParentToolCallID,
			AgentType: def.Name, Description: task.Description, Status: report.Status,
			Step: report.Steps, Usage: report.Usage, CostUSD: report.CostUSD,
			Elapsed: report.Duration, Summary: report.Summary, Detail: report.Error,
		})
		m.wg.Done()
		close(rt.done)
		return report
	}

	m.emit(Event{Type: EventSubagentStart, AgentID: id, ParentToolCallID: task.ParentToolCallID, AgentType: def.Name, Description: task.Description, Status: StatusQueued})
	select {
	case m.sem <- struct{}{}:
	case <-childCtx.Done():
		if errors.Is(childCtx.Err(), context.DeadlineExceeded) {
			return finish(limitReport(id, task, contextFailure(childCtx.Err())))
		}
		return finish(cancelledReport(id, task, contextFailure(childCtx.Err())))
	}
	defer func() { <-m.sem }()

	m.mu.Lock()
	if childCtx.Err() != nil {
		m.mu.Unlock()
		if errors.Is(childCtx.Err(), context.DeadlineExceeded) {
			return finish(limitReport(id, task, contextFailure(childCtx.Err())))
		}
		return finish(cancelledReport(id, task, contextFailure(childCtx.Err())))
	}
	rt.status = StatusRunning
	rt.started = time.Now()
	m.mu.Unlock()
	m.emit(Event{Type: EventSubagentStart, AgentID: id, ParentToolCallID: task.ParentToolCallID, AgentType: def.Name, Description: task.Description, Status: StatusRunning})

	if m.budget.Exceeded() {
		return finish(limitReport(id, task, "session cost budget reached"))
	}

	m.mu.Lock()
	cfgPerm := m.cfg.Permission
	cfgPlanMode := m.cfg.PlanMode
	cfgModel := m.cfg.Model
	cfgEffort := m.cfg.ReasoningEffort
	m.mu.Unlock()

	forceReadOnly := cfgPlanMode || def.ReadOnly
	if cfgPerm != nil && cfgPerm.Mode() == permission.ModePlan {
		forceReadOnly = true
	}
	override := strings.TrimSpace(def.Permission)
	if strings.TrimSpace(task.ApprovalMode) != "" {
		override = task.ApprovalMode
	}
	childPerm := permission.DeriveChild(cfgPerm, override, forceReadOnly)

	maxSteps := m.cfg.MaxSteps
	if task.MaxSteps > 0 {
		maxSteps = task.MaxSteps
	}
	model := strings.TrimSpace(task.Model)
	if model == "" {
		model = strings.TrimSpace(def.Model)
	}
	if model == "" {
		model = cfgModel
	}

	runner := &Runner{
		ID:               id,
		Definition:       def,
		Task:             task,
		Provider:         m.cfg.Provider,
		Registry:         m.cfg.Registry,
		Permission:       childPerm,
		Session:          m.cfg.Session,
		ParentToolCallID: task.ParentToolCallID,
		WorkDir:          m.cfg.WorkDir,
		Shell:            m.cfg.Shell,
		SearchBackend:    m.cfg.SearchBackend,
		SearchEndpoint:   m.cfg.SearchEndpoint,
		Approvals:        m.queue,
		Locker:           m.locker,
		Budget:           m.budget,
		MaxSteps:         maxSteps,
		ReasoningEffort:  cfgEffort,
		CostFunc:         m.cfg.CostFunc,
		Emit: func(ev Event) {
			ev.ParentToolCallID = task.ParentToolCallID
			m.updateRuntime(id, ev)
			m.emit(ev)
		},
		OnStatus: func(status, currentTool string) {
			m.mu.Lock()
			rt.status = status
			rt.currentTool = currentTool
			m.mu.Unlock()
		},
	}
	if runner.CostFunc == nil {
		pricing := config.Default()
		runner.CostFunc = func(model string, u provider.Usage) float64 {
			miss := u.PromptCacheMissTokens
			if miss == 0 && u.PromptCacheHitTokens == 0 {
				miss = u.PromptTokens
			}
			return pricing.EstimateCost(model, u.PromptCacheHitTokens, miss, u.CompletionTokens, time.Now())
		}
	}
	if model != "" {
		runner.Task.Model = model
	}

	report := runner.Run(childCtx)
	if report.Status == StatusRunning {
		report.Status = StatusDone
	}
	if childCtx.Err() != nil {
		switch {
		case errors.Is(childCtx.Err(), context.DeadlineExceeded):
			if report.Status == StatusDone {
				report.Status = StatusFailed
			}
			if report.Error == "" {
				report.Error = contextFailure(childCtx.Err())
			}
		case errors.Is(childCtx.Err(), context.Canceled) && report.Status == StatusRunning:
			report.Status = StatusCancelled
		}
	}
	return finish(report)
}

// List returns snapshots of every agent in creation order.
func (m *Manager) List() []AgentStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AgentStatus, 0, len(m.order))
	for _, id := range m.order {
		rt := m.agents[id]
		if rt == nil {
			continue
		}
		out = append(out, AgentStatus{
			ID:          rt.id,
			AgentType:   rt.def.Name,
			Description: rt.task.Description,
			Status:      rt.status,
			Step:        rt.step,
			CurrentTool: rt.currentTool,
			StartedAt:   rt.started,
			Error:       reportError(rt.report),
			Summary:     reportSummary(rt.report),
			Usage:       reportUsage(rt.report),
			CostUSD:     reportCost(rt.report),
		})
	}
	return out
}

// Get returns one agent snapshot.
func (m *Manager) Get(id string) (AgentStatus, bool) {
	for _, st := range m.List() {
		if st.ID == id {
			return st, true
		}
	}
	return AgentStatus{}, false
}

// Report returns the final report for id once it has completed.
func (m *Manager) Report(id string) (AgentReport, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	report, ok := m.completed[id]
	return report, ok
}

// Reports returns completed reports sorted by agent id.
func (m *Manager) Reports() []AgentReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.completed))
	for id := range m.completed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]AgentReport, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.completed[id])
	}
	return out
}

// CancelOne cancels only the named agent. The parent turn and all other agents
// continue unaffected.
func (m *Manager) CancelOne(id string) bool {
	m.mu.Lock()
	rt := m.agents[id]
	if rt == nil || rt.report != nil {
		m.mu.Unlock()
		return false
	}
	rt.cancel()
	m.mu.Unlock()
	return true
}

// CancelAll cancels every active agent and prevents new work from starting.
func (m *Manager) CancelAll() int {
	m.mu.Lock()
	m.stopped = true
	ids := make([]string, 0, len(m.agents))
	for id, rt := range m.agents {
		if rt.report == nil {
			rt.cancel()
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	return len(ids)
}

// Wait blocks until every currently registered agent has finished.
func (m *Manager) Wait() { m.wg.Wait() }

// WaitContext waits for all agents or until ctx is done.
func (m *Manager) WaitContext(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) updateRuntime(id string, ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.agents[id]
	if rt == nil {
		return
	}
	if ev.Status != "" {
		rt.status = ev.Status
	}
	if ev.Step > 0 {
		rt.step = ev.Step
	}
	if ev.CurrentTool != "" {
		rt.currentTool = ev.CurrentTool
	}
}

func (m *Manager) emit(ev Event) {
	if m.cfg.Emit != nil {
		m.cfg.Emit(ev)
	}
}

func (m *Manager) recordLink(parentToolCallID string, report AgentReport) {
	if m.cfg.Session == nil {
		return
	}
	_ = m.cfg.Session.AppendSubagentLink(session.SubagentLink{
		ParentToolCallID: parentToolCallID,
		AgentID:          report.AgentID,
		AgentType:        report.AgentType,
		Description:      report.Description,
		Status:           report.Status,
		Summary:          report.Summary,
		Usage:            report.Usage,
		CostUSD:          report.CostUSD,
		DurationMs:       report.Duration.Milliseconds(),
	})
}

func findDefinition(defs []Definition, name string) (Definition, bool) {
	for _, def := range defs {
		if def.Name == name {
			return def.Clone(), true
		}
	}
	return Definition{}, false
}

func applyTaskOverrides(def Definition, task Task) Definition {
	if strings.TrimSpace(task.Model) != "" {
		def.Model = strings.TrimSpace(task.Model)
	}
	if task.MaxSteps > 0 {
		def.MaxSteps = task.MaxSteps
	}
	if len(task.Tools) > 0 {
		allowed := make(map[string]struct{}, len(task.Tools))
		for _, t := range task.Tools {
			allowed[strings.TrimSpace(t)] = struct{}{}
		}
		filtered := make([]string, 0, len(def.Tools))
		for _, t := range def.Tools {
			if _, ok := allowed[t]; ok {
				filtered = append(filtered, t)
			}
		}
		def.Tools = filtered
	}
	sort.Strings(def.Tools)
	return def
}

func cloneDefinitions(defs []Definition) []Definition {
	out := make([]Definition, len(defs))
	for i, def := range defs {
		out[i] = def.Clone()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func failedReport(id string, task Task, msg string) AgentReport {
	return AgentReport{
		AgentID: id, AgentType: task.Agent, Description: task.Description,
		Status: StatusFailed, Summary: "Sub-agent failed: " + msg, Error: msg,
		FilesRead: []string{}, FilesChanged: []string{},
	}
}

func cancelledReport(id string, task Task, msg string) AgentReport {
	if strings.TrimSpace(msg) == "" {
		msg = "sub-agent cancelled"
	}
	return AgentReport{
		AgentID: id, AgentType: task.Agent, Description: task.Description,
		Status: StatusCancelled, Summary: "Sub-agent cancelled: " + msg, Error: msg,
		FilesRead: []string{}, FilesChanged: []string{},
	}
}

func limitReport(id string, task Task, msg string) AgentReport {
	return AgentReport{
		AgentID: id, AgentType: task.Agent, Description: task.Description,
		Status: StatusLimitReached, Summary: "Sub-agent stopped early: " + msg, Error: msg,
		FilesRead: []string{}, FilesChanged: []string{},
	}
}

func reportError(r *AgentReport) string {
	if r == nil {
		return ""
	}
	return r.Error
}
func reportSummary(r *AgentReport) string {
	if r == nil {
		return ""
	}
	return r.Summary
}
func reportUsage(r *AgentReport) provider.Usage {
	if r == nil {
		return provider.Usage{}
	}
	return r.Usage
}
func reportCost(r *AgentReport) float64 {
	if r == nil {
		return 0
	}
	return r.CostUSD
}
