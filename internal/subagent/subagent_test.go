package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
	"github.com/tern-agent/tern/internal/tools"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func ptr[T any](v T) *T { return &v }

func usage(hit, miss, completion int) provider.Usage {
	return provider.Usage{
		PromptTokens:          hit + miss,
		CompletionTokens:      completion,
		TotalTokens:           hit + miss + completion,
		PromptCacheHitTokens:  hit,
		PromptCacheMissTokens: miss,
	}
}

func textScript(text string, u provider.Usage) []provider.StreamEvent {
	return []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: text},
		{Type: provider.EventUsage, Usage: ptr(u)},
		{Type: provider.EventDone, FinishReason: "stop", Usage: ptr(u)},
	}
}

type call struct {
	id   string
	name string
	args any
}

func toolScript(calls []call, text string, u provider.Usage) []provider.StreamEvent {
	events := make([]provider.StreamEvent, 0, len(calls)+3)
	for i, c := range calls {
		args, _ := json.Marshal(c.args)
		id := c.id
		if id == "" {
			id = fmt.Sprintf("call-%d", i)
		}
		events = append(events, provider.StreamEvent{
			Type:          provider.EventToolCallDelta,
			ToolCallIndex: i,
			ToolCallID:    id,
			ToolName:      c.name,
			ToolArgsDelta: string(args),
		})
	}
	events = append(events, provider.StreamEvent{Type: provider.EventUsage, Usage: ptr(u)})
	events = append(events, provider.StreamEvent{Type: provider.EventDone, FinishReason: "tool_calls", Usage: ptr(u)})
	if text != "" {
		events = append([]provider.StreamEvent{{Type: provider.EventTextDelta, Text: text}}, events...)
	}
	return events
}

func testRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	reg.Register(NewWebSearchTool(nil, "duckduckgo", ""))
	reg.Register(NewWebFetchTool())
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

func newManager(t *testing.T, dir string, p provider.Provider, scripts [][]provider.StreamEvent, mutate func(*Config)) (*Manager, *provider.MockProvider) {
	t.Helper()
	var mock *provider.MockProvider
	if p == nil {
		mock = provider.NewMockProvider()
		for _, script := range scripts {
			mock.QueueScript(script...)
		}
		p = mock
	} else if existing, ok := p.(*provider.MockProvider); ok {
		mock = existing
		for _, script := range scripts {
			mock.QueueScript(script...)
		}
	}
	cfg := Config{
		MaxAgents:  4,
		MaxDepth:   DefaultMaxDepth,
		WorkDir:    dir,
		Provider:   p,
		Registry:   testRegistry(t),
		Permission: permission.New(permission.Policy{Mode: permission.ModeYolo, WorkDir: dir}),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	m := NewManager(cfg)
	t.Cleanup(func() { m.CancelAll(); m.Wait() })
	return m, mock
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// controlledProvider blocks or delays each stream, which makes concurrency,
// cancellation and timeout behaviour observable.
type controlledProvider struct {
	mu       sync.Mutex
	calls    int
	requests []provider.ChatRequest
	started  chan struct{}
	release  chan struct{}
	block    bool
	delay    time.Duration
}

func newControlledProvider(block bool, delay time.Duration) *controlledProvider {
	return &controlledProvider{
		started: make(chan struct{}, 256),
		release: make(chan struct{}),
		block:   block,
		delay:   delay,
	}
}

func (p *controlledProvider) ModelInfo(model string) provider.ModelCapabilities {
	return provider.DefaultModelCapabilities(model)
}

func (p *controlledProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *controlledProvider) Stream(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	select {
	case p.started <- struct{}{}:
	default:
	}
	ch := make(chan provider.StreamEvent)
	go func() {
		defer close(ch)
		if p.block {
			select {
			case <-ctx.Done():
				return
			case <-p.release:
			}
		} else if p.delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(p.delay):
			}
		}
		u := usage(0, 10, 5)
		events := []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "finished"},
			{Type: provider.EventUsage, Usage: ptr(u)},
			{Type: provider.EventDone, FinishReason: "stop", Usage: ptr(u)},
		}
		for _, ev := range events {
			select {
			case <-ctx.Done():
				return
			case ch <- ev:
			}
		}
	}()
	return ch, nil
}

// ---------------------------------------------------------------------------
// definitions and loader
// ---------------------------------------------------------------------------

func TestBuiltinDefinitionsDeterministic(t *testing.T) {
	a := BuiltinDefinitions()
	b := BuiltinDefinitions()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("BuiltinDefinitions is not deterministic")
	}
	names := make([]string, len(a))
	for i, def := range a {
		names[i] = def.Name
		if !sort.StringsAreSorted(def.Tools) {
			t.Fatalf("%s tools not sorted: %v", def.Name, def.Tools)
		}
		if def.SystemPrompt == "" || def.Description == "" {
			t.Fatalf("%s has empty prompt or description", def.Name)
		}
	}
	if got, want := strings.Join(names, ","), "explore,general,research,review"; got != want {
		t.Fatalf("builtin order = %s, want %s", got, want)
	}
	if _, ok := BuiltinDefinition(AgentReview); !ok {
		t.Fatal("review builtin missing")
	}

	explore := a[0]
	for _, forbidden := range []string{"write", "edit", "multi_edit"} {
		for _, tool := range explore.Tools {
			if tool == forbidden {
				t.Fatalf("explore includes %s", forbidden)
			}
		}
	}
	general := a[1]
	for _, tool := range general.Tools {
		if tool == ToolTask || tool == tools.ToolAskUser {
			t.Fatalf("general includes %s", tool)
		}
	}
	research := a[2]
	if !contains(research.Tools, ToolWebSearch) || !contains(research.Tools, ToolWebFetch) {
		t.Fatalf("research tools = %v", research.Tools)
	}
	if !strings.Contains(research.SystemPrompt, "source") && !strings.Contains(research.SystemPrompt, "Sources") {
		t.Fatal("research prompt must require sources")
	}
}

func TestLoaderCustomAndProjectOverride(t *testing.T) {
	global := t.TempDir()
	work := t.TempDir()
	writeFile(t, filepath.Join(global, "agents", "docs.md"), `---
name: docs
description: Global docs agent
tools: [read, grep]
model: deepseek-flash
max_steps: 7
permission: plan
---
You are the global docs agent.
`)
	writeFile(t, filepath.Join(work, ".tern", "agents", "docs.md"), `---
name: docs
description: Project docs agent
tools: write, read
permission: ask
---
You are the project docs agent.
`)
	writeFile(t, filepath.Join(work, ".tern", "agents", "audit.md"), "Audit the code.\n")

	defs, err := LoadDefinitions(global, work)
	if err != nil {
		t.Fatal(err)
	}
	var docs, audit *Definition
	for i := range defs {
		switch defs[i].Name {
		case "docs":
			docs = &defs[i]
		case "audit":
			audit = &defs[i]
		}
	}
	if docs == nil || audit == nil {
		t.Fatalf("custom agents missing: %+v", defs)
	}
	if docs.Description != "Project docs agent" || docs.SystemPrompt != "You are the project docs agent." {
		t.Fatalf("project did not override global: %+v", docs)
	}
	if got, want := strings.Join(docs.Tools, ","), "read,write"; got != want {
		t.Fatalf("docs tools = %s, want %s", got, want)
	}
	if docs.Permission != "ask" || docs.Builtin {
		t.Fatalf("unexpected docs metadata: %+v", docs)
	}
	if audit.Name != "audit" || audit.Description != "Audit the code." {
		t.Fatalf("file stem fallback failed: %+v", audit)
	}
}

func TestLoaderErrorsAndMissingDirs(t *testing.T) {
	defs, err := LoadDefinitions(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing dirs should not error: %v", err)
	}
	if len(defs) != 4 {
		t.Fatalf("expected only builtins, got %d", len(defs))
	}
}

// ---------------------------------------------------------------------------
// locks and isolated workcopy
// ---------------------------------------------------------------------------

func TestPathLocker(t *testing.T) {
	locker := NewPathLocker()
	release, err := locker.Acquire(context.Background(), "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locker.TryAcquire("a.txt"); err == nil || !strings.Contains(err.Error(), "already being edited") {
		t.Fatalf("expected conflict, got %v", err)
	}
	if !locker.Locked("a.txt") {
		t.Fatal("locked path not reported")
	}
	acquired := make(chan struct{})
	go func() {
		r, err := locker.Acquire(context.Background(), "a.txt")
		if err != nil {
			t.Errorf("waiting acquire: %v", err)
			return
		}
		r()
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("second acquire succeeded while locked")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("waiting acquire did not resume")
	}
	if locker.Locked("a.txt") {
		t.Fatal("path still locked after release")
	}

	release2, _ := locker.Acquire(context.Background(), "b.txt")
	defer release2()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := locker.Acquire(ctx, "b.txt"); err == nil {
		t.Fatal("ctx-limited acquire succeeded")
	}
}

func TestWorkcopyPatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keep.txt"), "same\n")
	writeFile(t, filepath.Join(dir, "edit.txt"), "old\n")
	writeFile(t, filepath.Join(dir, "delete.txt"), "gone\n")
	writeFile(t, filepath.Join(dir, "node_modules", "skip.js"), "ignored\n")

	wc, err := PrepareWorkcopy(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer wc.Cleanup()
	if _, err := os.Stat(filepath.Join(wc.Dir, "node_modules")); !os.IsNotExist(err) {
		t.Fatal("node_modules should be skipped")
	}
	writeFile(t, filepath.Join(wc.Dir, "edit.txt"), "new\n")
	writeFile(t, filepath.Join(wc.Dir, "added.txt"), "added\n")
	if err := os.Remove(filepath.Join(wc.Dir, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	patch, changed, err := wc.Patch()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(changed, ","), "added.txt,delete.txt,edit.txt"; got != want {
		t.Fatalf("changed = %s, want %s", got, want)
	}
	for _, want := range []string{"--- a/added.txt", "+added", "--- a/delete.txt", "-gone", "--- a/edit.txt", "-old", "+new"} {
		if !strings.Contains(patch, want) {
			t.Fatalf("patch missing %q:\n%s", want, patch)
		}
	}
	if strings.Contains(patch, "keep.txt") {
		t.Fatalf("unchanged file diffed:\n%s", patch)
	}
}

// ---------------------------------------------------------------------------
// approvals
// ---------------------------------------------------------------------------

func TestApprovalQueueSerializes(t *testing.T) {
	var active, maxActive int32
	queue := NewApprovalQueue(func(ctx context.Context, req ApprovalRequest) (bool, error) {
		cur := atomic.AddInt32(&active, 1)
		for {
			prev := atomic.LoadInt32(&maxActive)
			if cur <= prev || atomic.CompareAndSwapInt32(&maxActive, prev, cur) {
				break
			}
		}
		if req.DiffPreview == "" {
			t.Errorf("request missing diff preview: %+v", req)
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return true, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := queue.Ask(context.Background(), ApprovalRequest{
				AgentID:         fmt.Sprintf("agent-%d", i),
				AgentType:       AgentGeneral,
				TaskDescription: "edit files",
				ToolName:        "write",
				Detail:          "a.txt",
				DiffPreview:     "--- a/a.txt\n+++ b/a.txt\n",
			})
			if err != nil || !ok {
				t.Errorf("Ask = %v, %v", ok, err)
			}
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&maxActive); got != 1 {
		t.Fatalf("max concurrent approvals = %d, want 1", got)
	}
}

func TestApprovalQueueContextCancelAndNoApprover(t *testing.T) {
	q := NewApprovalQueue()
	if ok, err := q.Ask(context.Background(), ApprovalRequest{}); ok || !errors.Is(err, ErrNoApprover) {
		t.Fatalf("no approver: %v, %v", ok, err)
	}
	block := make(chan struct{})
	q.SetApprover(func(ctx context.Context, _ ApprovalRequest) (bool, error) {
		close(block)
		<-ctx.Done()
		return false, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := q.Ask(ctx, ApprovalRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// manager and runner
// ---------------------------------------------------------------------------

func TestParallelRuns(t *testing.T) {
	dir := t.TempDir()
	p := newControlledProvider(false, 40*time.Millisecond)
	m, _ := newManager(t, dir, p, nil, func(cfg *Config) {
		cfg.MaxAgents = 2
	})

	var mu sync.Mutex
	running, maxRunning := 0, 0
	sawQueued := false
	cfg := m.Config()
	cfg.Emit = func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		switch ev.Type {
		case EventSubagentStart:
			switch ev.Status {
			case StatusQueued:
				sawQueued = true
			case StatusRunning:
				running++
				if running > maxRunning {
					maxRunning = running
				}
			}
		case EventSubagentEnd:
			if running > 0 {
				running--
			}
		}
	}
	// Rebuild with an Eq callback by replacing the config emit via a new manager.
	m = NewManager(cfg)

	var wg sync.WaitGroup
	reports := make([]AgentReport, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reports[i] = m.Run(context.Background(), Task{Description: fmt.Sprintf("task %d", i), Prompt: "do work", Agent: AgentExplore})
		}(i)
	}
	wg.Wait()
	for i, report := range reports {
		if report.Status != StatusDone {
			t.Fatalf("report %d = %+v", i, report)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maxRunning != 2 {
		t.Fatalf("max running = %d, want 2", maxRunning)
	}
	if !sawQueued {
		t.Fatal("never observed queued status")
	}
}

func TestQueueingAtConcurrencyLimitAndCancelOne(t *testing.T) {
	dir := t.TempDir()
	p := newControlledProvider(true, 0)
	m, _ := newManager(t, dir, p, nil, func(cfg *Config) {
		cfg.MaxAgents = 1
	})

	first := make(chan AgentReport, 1)
	go func() {
		first <- m.Run(context.Background(), Task{Description: "blocked", Prompt: "wait", Agent: AgentExplore})
	}()
	<-p.started
	// The second agent must be queued behind the first.
	second := make(chan AgentReport, 1)
	go func() {
		second <- m.Run(context.Background(), Task{Description: "queued", Prompt: "wait", Agent: AgentExplore})
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		list := m.List()
		if len(list) == 2 && list[1].Status == StatusQueued {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	list := m.List()
	if len(list) != 2 || list[1].Status != StatusQueued {
		t.Fatalf("second agent not queued: %+v", list)
	}

	if !m.CancelOne(list[0].ID) {
		t.Fatal("CancelOne returned false")
	}
	select {
	case r := <-first:
		if r.Status != StatusCancelled {
			t.Fatalf("first status = %s (%+v)", r.Status, r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled agent did not return")
	}
	// Closing the release lets the queued agent complete normally.
	close(p.release)
	select {
	case r := <-second:
		if r.Status != StatusDone {
			t.Fatalf("second status = %s (%+v)", r.Status, r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued agent did not finish")
	}
}

func TestCancelAll(t *testing.T) {
	dir := t.TempDir()
	p := newControlledProvider(true, 0)
	m, _ := newManager(t, dir, p, nil, func(cfg *Config) { cfg.MaxAgents = 2 })
	reports := make(chan AgentReport, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			reports <- m.Run(context.Background(), Task{Description: fmt.Sprintf("task %d", i), Prompt: "wait", Agent: AgentExplore})
		}(i)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-p.started:
		case <-time.After(time.Second):
			t.Fatal("agent did not start")
		}
	}
	if n := m.CancelAll(); n != 2 {
		t.Fatalf("CancelAll cancelled %d, want 2", n)
	}
	for i := 0; i < 2; i++ {
		select {
		case r := <-reports:
			if r.Status != StatusCancelled {
				t.Fatalf("status = %s (%+v)", r.Status, r)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled agent did not return")
		}
	}
	// After CancelAll, new work is refused cleanly.
	r := m.Run(context.Background(), Task{Description: "late", Prompt: "x", Agent: AgentExplore})
	if r.Status != StatusCancelled {
		t.Fatalf("late task status = %s", r.Status)
	}
}

func TestTimeoutReturnsLimitReport(t *testing.T) {
	dir := t.TempDir()
	p := newControlledProvider(true, 0)
	m, _ := newManager(t, dir, p, nil, nil)
	start := time.Now()
	r := m.Run(context.Background(), Task{
		Description: "slow", Prompt: "wait", Agent: AgentExplore, Timeout: 50 * time.Millisecond,
	})
	if r.Status != StatusLimitReached || !strings.Contains(r.Error, "timed out") {
		t.Fatalf("timeout report = %+v", r)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout took too long")
	}
}

func TestStepLimitPartialReport(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello\n")
	scripts := [][]provider.StreamEvent{
		toolScript([]call{{name: "read", args: map[string]any{"path": "a.txt"}}}, "starting work", usage(10, 5, 5)),
	}
	m, _ := newManager(t, dir, provider.NewMockProvider(), scripts, nil)
	r := m.Run(context.Background(), Task{
		Description: "limited", Prompt: "read", Agent: AgentExplore, MaxSteps: 1,
	})
	if r.Status != StatusLimitReached {
		t.Fatalf("status = %s (%+v)", r.Status, r)
	}
	if !strings.Contains(r.Summary, "starting work") || !strings.Contains(r.Error, "step limit") {
		t.Fatalf("partial report = %+v", r)
	}
	if r.Steps != 1 || len(r.FilesRead) != 1 {
		t.Fatalf("report missing partial details: %+v", r)
	}
}

func TestBudgetLimit(t *testing.T) {
	dir := t.TempDir()
	scripts := [][]provider.StreamEvent{textScript("expensive", usage(10, 10, 10))}
	m, _ := newManager(t, dir, provider.NewMockProvider(), scripts, func(cfg *Config) {
		cfg.MaxCostUSD = 0.000001
		cfg.CostFunc = func(string, provider.Usage) float64 { return 1 }
	})
	r := m.Run(context.Background(), Task{Description: "cost", Prompt: "go", Agent: AgentExplore})
	if r.Status != StatusLimitReached || !strings.Contains(r.Error, "budget") {
		t.Fatalf("budget report = %+v", r)
	}
	if r.CostUSD != 1 {
		t.Fatalf("cost = %v", r.CostUSD)
	}
}

func TestPanicRecovery(t *testing.T) {
	dir := t.TempDir()
	reg := testRegistry(t)
	reg.Register(&panicTool{})
	scripts := [][]provider.StreamEvent{
		toolScript([]call{{name: "read", args: map[string]any{"path": "x"}}}, "", usage(1, 1, 1)),
	}
	m, _ := newManager(t, dir, provider.NewMockProvider(), scripts, func(cfg *Config) {
		cfg.Registry = reg
	})
	r := m.Run(context.Background(), Task{
		Description: "panic", Prompt: "trigger", Agent: AgentExplore, Tools: []string{"read"},
	})
	if r.Status != StatusFailed || !strings.Contains(r.Error, "panic") {
		t.Fatalf("panic report = %+v", r)
	}
}

type panicTool struct{}

func (p *panicTool) Name() string        { return "read" }
func (p *panicTool) Description() string { return "panics" }
func (p *panicTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}
func (p *panicTool) IsReadOnly() bool     { return true }
func (p *panicTool) IsWeb() bool          { return false }
func (p *panicTool) IsParallelSafe() bool { return true }
func (p *panicTool) Execute(context.Context, tools.ExecEnv, json.RawMessage) (tools.Result, error) {
	panic("tool exploded")
}

func TestDepthCap(t *testing.T) {
	dir := t.TempDir()
	m, _ := newManager(t, dir, provider.NewMockProvider(), nil, func(cfg *Config) { cfg.MaxDepth = 1 })
	r := m.Run(context.Background(), Task{Description: "nested", Prompt: "x", Agent: AgentExplore, Depth: 1})
	if r.Status != StatusFailed || !strings.Contains(r.Error, "depth") {
		t.Fatalf("depth report = %+v", r)
	}
	m2, _ := newManager(t, dir, provider.NewMockProvider(), nil, func(cfg *Config) { cfg.MaxDepth = 99 })
	if m2.Config().MaxDepth != HardMaxDepth {
		t.Fatalf("hard cap not applied: %d", m2.Config().MaxDepth)
	}
}

func TestGoroutineLeak(t *testing.T) {
	dir := t.TempDir()
	scripts := [][]provider.StreamEvent{
		textScript("one", usage(1, 1, 1)),
		textScript("two", usage(1, 1, 1)),
	}
	run := func() {
		m, _ := newManager(t, dir, provider.NewMockProvider(), scripts, nil)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = m.Run(context.Background(), Task{Description: "leak", Prompt: "x", Agent: AgentExplore})
			}()
		}
		wg.Wait()
		m.CancelAll()
		m.Wait()
	}
	// Warm up lazy runtime goroutines, then measure.
	run()
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		run()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: before=%d after=%d", before, runtime.NumGoroutine())
}

// ---------------------------------------------------------------------------
// prefix stability and resume
// ---------------------------------------------------------------------------

func TestSameTypeAgentsSharePrefix(t *testing.T) {
	dir := t.TempDir()
	scripts := [][]provider.StreamEvent{
		textScript("result one", usage(5, 5, 5)),
		textScript("result two", usage(5, 5, 5)),
	}
	mock := provider.NewMockProvider()
	for _, s := range scripts {
		mock.QueueScript(s...)
	}
	m, _ := newManager(t, dir, mock, nil, nil)
	r1 := m.Run(context.Background(), Task{Description: "one", Prompt: "find auth", Agent: AgentExplore})
	r2 := m.Run(context.Background(), Task{Description: "two", Prompt: "find sessions", Agent: AgentExplore})
	if r1.Status != StatusDone || r2.Status != StatusDone {
		t.Fatalf("reports: %+v %+v", r1, r2)
	}
	reqs := mock.RequestSnapshot()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	h1, err := provider.SystemToolsHash(reqs[0])
	if err != nil {
		t.Fatal(err)
	}
	h2, err := provider.SystemToolsHash(reqs[1])
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("same-type prefixes differ: %s vs %s", h1, h2)
	}
	if reqs[0].Messages[0].Content != reqs[1].Messages[0].Content {
		t.Fatal("system prompt differs between same-type agents")
	}
	if reqs[0].Messages[1].Content == reqs[1].Messages[1].Content {
		t.Fatal("task-specific prompt should differ in the user message")
	}
	if strings.Contains(reqs[0].Messages[0].Content, "find auth") {
		t.Fatal("task prompt leaked into system prefix")
	}
}

func TestParentCanonicalRequestUnchanged(t *testing.T) {
	dir := t.TempDir()
	req := provider.ChatRequest{
		Model: "deepseek-flash",
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "parent system"},
			{Role: provider.RoleUser, Content: "parent task"},
		},
		Tools: testRegistry(t).ToolDefs(),
	}
	before, err := provider.MarshalCanonicalRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	scripts := [][]provider.StreamEvent{textScript("child", usage(1, 1, 1))}
	m, _ := newManager(t, dir, provider.NewMockProvider(), scripts, nil)
	_ = m.Run(context.Background(), Task{Description: "child", Prompt: "x", Agent: AgentExplore})
	after, err := provider.MarshalCanonicalRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("parent canonical request changed after a sub-agent run")
	}
}

func TestSessionResumeTranscripts(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Create(session.SessionMeta{ID: "parent", WorkDir: dir, Model: "deepseek-flash", Name: "p"})
	if err != nil {
		t.Fatal(err)
	}
	parentMsgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "parent system"},
		{Role: provider.RoleUser, Content: "parent user"},
		{Role: provider.RoleAssistant, Content: "parent answer"},
	}
	for _, msg := range parentMsgs {
		if err := parent.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	scripts := [][]provider.StreamEvent{textScript("child answer", usage(1, 2, 3))}
	m, _ := newManager(t, dir, provider.NewMockProvider(), scripts, func(cfg *Config) {
		cfg.Session = parent
	})
	r := m.Run(context.Background(), Task{
		Description: "child", Prompt: "child prompt", Agent: AgentExplore, ParentToolCallID: "call-1",
	})
	if r.Status != StatusDone {
		t.Fatalf("report = %+v", r)
	}

	children, err := parent.ListChildren()
	if err != nil || len(children) != 1 {
		t.Fatalf("children = %d, %v", len(children), err)
	}
	child := children[0]
	if child.Meta().ParentToolCallID != "call-1" || child.Meta().ParentSessionID != "parent" {
		t.Fatalf("child meta = %+v", child.Meta())
	}
	childBefore := child.ReplayMessages()
	parentBefore := parent.ReplayMessages()
	if !reflect.DeepEqual(childBefore, []provider.Message{
		{Role: provider.RoleSystem, Content: BuiltinDefinitions()[0].SystemPrompt},
		{Role: provider.RoleUser, Content: "child prompt"},
		{Role: provider.RoleAssistant, Content: "child answer"},
	}) {
		t.Fatalf("child transcript = %+v", childBefore)
	}
	usage, cost := parent.TotalUsage()
	if usage.TotalTokens != 6 || cost <= 0 {
		t.Fatalf("rolled-up usage = %+v cost=%v", usage, cost)
	}

	reopened, err := store.Open("parent")
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.ReplayMessages(); !reflect.DeepEqual(got, parentBefore) {
		t.Fatalf("parent replay differs:\n got %+v\nwant %+v", got, parentBefore)
	}
	reopenedChildren, err := reopened.ListChildren()
	if err != nil || len(reopenedChildren) != 1 {
		t.Fatalf("reopened children = %d, %v", len(reopenedChildren), err)
	}
	if got := reopenedChildren[0].ReplayMessages(); !reflect.DeepEqual(got, childBefore) {
		t.Fatalf("child replay differs:\n got %+v\nwant %+v", got, childBefore)
	}
	links := reopened.Subagents()
	if len(links) != 1 || links[0].Status != StatusDone || links[0].AgentID != r.AgentID {
		t.Fatalf("subagent link = %+v", links)
	}
}

// ---------------------------------------------------------------------------
// file locks, isolation and permissions
// ---------------------------------------------------------------------------

type slowWriteTool struct {
	delay time.Duration
}

func (s *slowWriteTool) Name() string { return tools.ToolWrite }
func (s *slowWriteTool) Description() string {
	return tools.NewWriteTool().Description()
}
func (s *slowWriteTool) Parameters() json.RawMessage { return tools.NewWriteTool().Parameters() }
func (s *slowWriteTool) IsReadOnly() bool            { return false }
func (s *slowWriteTool) IsWeb() bool                 { return false }
func (s *slowWriteTool) IsParallelSafe() bool        { return false }
func (s *slowWriteTool) Execute(ctx context.Context, env tools.ExecEnv, raw json.RawMessage) (tools.Result, error) {
	time.Sleep(s.delay)
	return tools.NewWriteTool().Execute(ctx, env, raw)
}

func TestConcurrentWriteAgentsFileLock(t *testing.T) {
	dir := t.TempDir()
	reg := testRegistry(t)
	reg.Register(&slowWriteTool{delay: 60 * time.Millisecond})
	scripts := [][]provider.StreamEvent{
		toolScript([]call{{id: "a", name: "write", args: map[string]any{"path": "same.txt", "content": "A\n"}}}, "", usage(1, 1, 1)),
		toolScript([]call{{id: "b", name: "write", args: map[string]any{"path": "same.txt", "content": "B\n"}}}, "", usage(1, 1, 1)),
		textScript("A done", usage(1, 1, 1)),
		textScript("B done", usage(1, 1, 1)),
	}
	mock := provider.NewMockProvider()
	for _, s := range scripts {
		mock.QueueScript(s...)
	}
	m, _ := newManager(t, dir, mock, nil, func(cfg *Config) {
		cfg.Registry = reg
		cfg.MaxAgents = 2
	})
	var wg sync.WaitGroup
	reports := make([]AgentReport, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reports[i] = m.Run(context.Background(), Task{
				Description: "write", Prompt: "write", Agent: AgentGeneral, Tools: []string{"write"},
			})
		}(i)
	}
	wg.Wait()
	for _, r := range reports {
		if r.Status != StatusDone {
			t.Fatalf("report = %+v", r)
		}
	}
	if content := readFile(t, filepath.Join(dir, "same.txt")); content != "A\n" && content != "B\n" {
		t.Fatalf("unexpected file content %q", content)
	}
	conflict := false
	for _, req := range mock.RequestSnapshot() {
		for _, msg := range req.Messages {
			if msg.Role == provider.RoleTool && strings.Contains(msg.Content, "already being edited") {
				conflict = true
			}
		}
	}
	if !conflict {
		t.Fatal("expected a concurrent write to be rejected by the path lock")
	}
	if m.Locker().Locked(filepath.Join(dir, "same.txt")) {
		t.Fatal("path still locked")
	}
}

func TestIsolatedAgentPatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "edit.txt"), "old\n")
	scripts := [][]provider.StreamEvent{
		toolScript([]call{
			{name: "write", args: map[string]any{"path": "added.txt", "content": "added\n"}},
			{name: "edit", args: map[string]any{"path": "edit.txt", "old_string": "old", "new_string": "new"}},
		}, "", usage(2, 2, 2)),
		textScript("done", usage(1, 1, 1)),
	}
	m, _ := newManager(t, dir, provider.NewMockProvider(), scripts, nil)
	r := m.Run(context.Background(), Task{
		Description: "isolated", Prompt: "edit", Agent: AgentGeneral, Isolated: true,
		Tools: []string{"write", "edit"},
	})
	if r.Status != StatusDone {
		t.Fatalf("report = %+v", r.Status)
	}
	if _, err := os.Stat(filepath.Join(dir, "added.txt")); !os.IsNotExist(err) {
		t.Fatal("isolated write leaked into the real workspace")
	}
	if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "old\n" {
		t.Fatalf("real file changed: %q", got)
	}
	for _, want := range []string{"--- a/added.txt", "+added", "--- a/edit.txt", "-old", "+new"} {
		if !strings.Contains(r.Patch, want) {
			t.Fatalf("patch missing %q:\n%s", want, r.Patch)
		}
	}
	if !contains(r.FilesChanged, "added.txt") || !contains(r.FilesChanged, "edit.txt") {
		t.Fatalf("files changed = %v", r.FilesChanged)
	}
}

func TestApprovalRoutingAndDiffPreview(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "old\n")
	var got ApprovalRequest
	approvals := NewApprovalQueue(func(ctx context.Context, req ApprovalRequest) (bool, error) {
		got = req
		return true, nil
	})
	scripts := [][]provider.StreamEvent{
		toolScript([]call{{name: "write", args: map[string]any{"path": "a.txt", "content": "new\n"}}}, "", usage(1, 1, 1)),
		textScript("written", usage(1, 1, 1)),
	}
	m, _ := newManager(t, dir, provider.NewMockProvider(), scripts, func(cfg *Config) {
		cfg.Approvals = approvals
		cfg.Permission = permission.New(permission.Policy{Mode: permission.ModeAsk, WorkDir: dir})
		cfg.Emit = func(ev Event) {
			if ev.Type == EventSubagentApproval {
				if ev.AgentID == "" || ev.AgentType != AgentGeneral || ev.CurrentTool != "write" {
					t.Errorf("approval event labels = %+v", ev)
				}
			}
		}
	})
	r := m.Run(context.Background(), Task{
		Description: "approval task", Prompt: "write", Agent: AgentGeneral, Tools: []string{"write"},
	})
	if r.Status != StatusDone {
		t.Fatalf("report = %+v", r)
	}
	if got.AgentID == "" || got.AgentType != AgentGeneral || got.TaskDescription != "approval task" || got.ToolName != "write" {
		t.Fatalf("approval labels = %+v", got)
	}
	if !strings.Contains(got.DiffPreview, "-old") || !strings.Contains(got.DiffPreview, "+new") {
		t.Fatalf("diff preview = %q", got.DiffPreview)
	}
	if readFile(t, filepath.Join(dir, "a.txt")) != "new\n" {
		t.Fatal("approved write did not happen")
	}
}

func TestPermissionNonEscalationAndPlanMode(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "old\n")

	t.Run("child cannot escalate", func(t *testing.T) {
		approvals := NewApprovalQueue(func(context.Context, ApprovalRequest) (bool, error) { return false, nil })
		scripts := [][]provider.StreamEvent{
			toolScript([]call{{name: "write", args: map[string]any{"path": "a.txt", "content": "escalated\n"}}}, "", usage(1, 1, 1)),
			textScript("denied", usage(1, 1, 1)),
		}
		m, _ := newManager(t, dir, provider.NewMockProvider(), scripts, func(cfg *Config) {
			cfg.Approvals = approvals
			cfg.Permission = permission.New(permission.Policy{Mode: permission.ModeAsk, WorkDir: dir})
		})
		r := m.Run(context.Background(), Task{
			Description: "no escalation", Prompt: "write", Agent: AgentGeneral,
			Tools: []string{"write"}, ApprovalMode: "yolo",
		})
		if r.Status != StatusDone {
			t.Fatalf("report = %+v", r)
		}
		if readFile(t, filepath.Join(dir, "a.txt")) != "old\n" {
			t.Fatal("child escalated to a write")
		}
	})

	t.Run("plan mode forces read-only", func(t *testing.T) {
		scripts := [][]provider.StreamEvent{
			toolScript([]call{{name: "write", args: map[string]any{"path": "a.txt", "content": "plan\n"}}}, "", usage(1, 1, 1)),
			textScript("blocked", usage(1, 1, 1)),
		}
		m, _ := newManager(t, dir, provider.NewMockProvider(), scripts, func(cfg *Config) {
			cfg.PlanMode = true
			cfg.Permission = permission.New(permission.Policy{Mode: permission.ModeYolo, WorkDir: dir})
		})
		r := m.Run(context.Background(), Task{
			Description: "plan", Prompt: "write", Agent: AgentGeneral, Tools: []string{"write"},
		})
		if r.Status != StatusDone {
			t.Fatalf("report = %+v", r)
		}
		if readFile(t, filepath.Join(dir, "a.txt")) != "old\n" {
			t.Fatal("plan mode allowed a write")
		}
	})
}

func TestReadOnlyBashAllowlist(t *testing.T) {
	dir := t.TempDir()
	scripts := [][]provider.StreamEvent{
		toolScript([]call{{name: "bash", args: map[string]any{"command": "touch should-not-exist"}}}, "", usage(1, 1, 1)),
		textScript("blocked", usage(1, 1, 1)),
	}
	m, _ := newManager(t, dir, provider.NewMockProvider(), scripts, nil)
	r := m.Run(context.Background(), Task{
		Description: "bash", Prompt: "touch", Agent: AgentExplore, Tools: []string{"bash"},
	})
	if r.Status != StatusDone {
		t.Fatalf("report = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatal("read-only bash executed a mutating command")
	}
}

func TestTaskTool(t *testing.T) {
	dir := t.TempDir()
	scripts := [][]provider.StreamEvent{textScript("delegated answer", usage(3, 3, 3))}
	m, _ := newManager(t, dir, provider.NewMockProvider(), scripts, nil)
	tool := NewTaskTool(m)
	if tool.Name() != "task" || !tool.IsParallelSafe() {
		t.Fatalf("task tool flags: %s %v", tool.Name(), tool.IsParallelSafe())
	}
	desc := tool.Description()
	for _, want := range []string{"explore", "general", "research", "review", "self-contained", "Do NOT delegate"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("description missing %q:\n%s", want, desc)
		}
	}
	raw, _ := json.Marshal(map[string]any{
		"description": "delegated task",
		"prompt":      "answer this",
		"agent":       AgentExplore,
	})
	res, err := tool.Execute(WithParentToolCallID(WithDepth(context.Background(), 0), "call-7"), tools.ExecEnv{WorkDir: dir}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.External {
		t.Fatal("task result must be external")
	}
	if !strings.Contains(res.Output, "delegated answer") || res.Metadata["status"] != StatusDone {
		t.Fatalf("task result = %+v", res)
	}
	if res.Metadata["agent_id"] == "" {
		t.Fatalf("task metadata = %+v", res.Metadata)
	}
	// Truncation happens exactly once at append time.
	env := tools.ExecEnv{WorkDir: dir, OutputCap: 40}
	res, err = tool.Execute(context.Background(), env, raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(res.Output, "truncated: showing") != 1 {
		t.Fatalf("expected exactly one truncation marker: %q", res.Output)
	}
}

// ---------------------------------------------------------------------------
// renderer goldens
// ---------------------------------------------------------------------------

func sampleViews() map[string]AgentView {
	base := AgentView{
		ID:          "agent-1",
		AgentType:   AgentExplore,
		Description: "Find where auth tokens are validated",
		Step:        3,
		Usage:       provider.Usage{PromptCacheHitTokens: 8200, PromptCacheMissTokens: 1800, CompletionTokens: 400, TotalTokens: 10400},
		CostUSD:     0.002,
		Elapsed:     12 * time.Second,
		Steps: []StepView{
			{Tool: "grep", Detail: `"validateToken"`, Status: StatusDone},
			{Tool: "read", Detail: "internal/auth/token.go", Status: StatusDone},
			{Tool: "read", Detail: "internal/auth/middleware.go", Status: StatusRunning},
		},
	}
	views := map[string]AgentView{}
	for _, status := range []string{StatusQueued, StatusRunning, StatusWaitingApproval, StatusDone, StatusFailed, StatusCancelled, StatusLimitReached} {
		v := base
		v.Status = status
		v.Summary = "Partial findings"
		v.Error = "boom"
		if status == StatusQueued {
			v.Step = 0
			v.Steps = nil
			v.Usage = provider.Usage{}
			v.CostUSD = 0
			v.Elapsed = 0
		}
		if status == StatusWaitingApproval {
			v.CurrentTool = "bash"
		}
		views[status] = v
	}
	return views
}

func TestRenderGoldens(t *testing.T) {
	if NoColor() != (os.Getenv("NO_COLOR") != "") {
		t.Fatal("NoColor mismatch")
	}
	views := sampleViews()
	for status, view := range views {
		name := "render_" + status + ".golden"
		t.Run(status, func(t *testing.T) {
			got := RenderAgent(view, true)
			checkGolden(t, name, got)
		})
	}
	t.Run("collapsed", func(t *testing.T) {
		got := RenderAgent(views[StatusDone], false)
		checkGolden(t, "render_collapsed.golden", got)
	})
	t.Run("group", func(t *testing.T) {
		got := RenderGroup([]AgentView{views[StatusRunning], views[StatusDone]}, false)
		checkGolden(t, "render_group.golden", got)
	})
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run UPDATE_GOLDEN=1 to create)", path, err)
	}
	want := strings.TrimSuffix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if got != want {
		t.Fatalf("golden mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

var _ = errors.Is
