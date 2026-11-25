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
	"testing"

	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/ext"
	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
	"github.com/tern-agent/tern/internal/tools"
	"github.com/tern-agent/tern/internal/web"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type sseCall struct {
	id   string
	name string
	args string
}

func sseData(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal SSE chunk: %v", err)
	}
	return "data: " + string(b) + "\n\n"
}

// sseFixture renders a recorded SSE response with optional text, tool calls,
// and usage. Tool arguments are split across two deltas so accumulation is
// exercised.
func sseFixture(t *testing.T, text string, calls []sseCall, usage *provider.Usage) string {
	t.Helper()
	var b strings.Builder
	if text != "" {
		b.WriteString(sseData(t, map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": text},
			}},
		}))
	}
	for i, call := range calls {
		first, second := call.args, ""
		if len(call.args) > 3 {
			cut := len(call.args) / 2
			first, second = call.args[:cut], call.args[cut:]
		}
		b.WriteString(sseData(t, map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": i,
					"id":    call.id,
					"type":  "function",
					"function": map[string]any{
						"name":      call.name,
						"arguments": first,
					},
				}}},
			}},
		}))
		if second != "" {
			b.WriteString(sseData(t, map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index":    i,
						"function": map[string]any{"arguments": second},
					}}},
				}},
			}))
		}
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	b.WriteString(sseData(t, map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}},
	}))
	if usage != nil {
		b.WriteString(sseData(t, map[string]any{
			"choices": []any{},
			"usage":   usage,
		}))
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func testConfig() *config.Config {
	cfg := config.Default()
	cfg.PermissionMode = string(permission.ModeYolo)
	cfg.WebPermission = string(permission.WebAllow)
	cfg.MaxSteps = 20
	return cfg
}

func yoloPermissions(workDir string) *permission.Evaluator {
	return permission.New(permission.Policy{Mode: permission.ModeYolo, Web: permission.WebAllow, WorkDir: workDir})
}

func newRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	r := tools.NewRegistry()
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func findToolMessage(history []provider.Message, needle string) (provider.Message, bool) {
	for _, msg := range history {
		if msg.Role == provider.RoleTool && strings.Contains(msg.Content, needle) {
			return msg, true
		}
	}
	return provider.Message{}, false
}

func countEvents(events []Event, typ EventType) int {
	n := 0
	for _, ev := range events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Multi-step tool loop
// ---------------------------------------------------------------------------

func TestRunTurnMultiStepParallelReadsThenSequentialEdit(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, workDir, "a.txt", "hello a\n")
	writeFile(t, workDir, "b.txt", "hello b\n")

	p := provider.NewMockProvider()
	p.QueueSSE(sseFixture(t, "", []sseCall{
		{id: "call_a", name: tools.ToolRead, args: `{"path":"a.txt"}`},
		{id: "call_b", name: tools.ToolRead, args: `{"path":"b.txt"}`},
	}, &provider.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}))
	p.QueueSSE(sseFixture(t, "", []sseCall{
		{id: "call_edit", name: tools.ToolEdit, args: `{"path":"a.txt","old_string":"hello a","new_string":"hello edited"}`},
	}, &provider.Usage{PromptTokens: 200, CompletionTokens: 10, TotalTokens: 210}))
	p.QueueSSE(sseFixture(t, "All done.", nil, &provider.Usage{
		PromptTokens: 300, CompletionTokens: 5, TotalTokens: 305,
		PromptCacheHitTokens: 250, PromptCacheMissTokens: 50,
	}))

	var events []Event
	a := New(Options{
		Config:      testConfig(),
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		WorkDir:     workDir,
		OnEvent:     func(ev Event) { events = append(events, ev) },
	})

	res, err := a.RunTurn(context.Background(), "please inspect and edit", nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.StopReason != StopComplete {
		t.Fatalf("stop reason = %q, want complete", res.StopReason)
	}
	if res.Steps != 3 {
		t.Fatalf("steps = %d, want 3", res.Steps)
	}
	if res.ToolCalls != 3 {
		t.Fatalf("tool calls = %d, want 3", res.ToolCalls)
	}
	if !strings.Contains(res.Text, "All done.") {
		t.Fatalf("text = %q, want final answer", res.Text)
	}
	if got := readFile(t, filepath.Join(workDir, "a.txt")); got != "hello edited\n" {
		t.Fatalf("a.txt = %q", got)
	}
	if got := countEvents(events, EventToolStart); got != 3 {
		t.Fatalf("tool_start events = %d, want 3", got)
	}
	if got := countEvents(events, EventToolEnd); got != 3 {
		t.Fatalf("tool_end events = %d, want 3", got)
	}
	if countEvents(events, EventTurnStart) != 1 || countEvents(events, EventTurnEnd) != 1 {
		t.Fatalf("missing turn boundary events: %+v", events)
	}

	// Tool messages must appear in exact tool_calls order even though the two
	// reads ran concurrently.
	history := a.History()
	var toolIDs []string
	for _, msg := range history {
		if msg.Role == provider.RoleTool {
			toolIDs = append(toolIDs, msg.ToolCallID)
		}
	}
	wantIDs := []string{"call_a", "call_b", "call_edit"}
	if !reflect.DeepEqual(toolIDs, wantIDs) {
		t.Fatalf("tool result order = %v, want %v", toolIDs, wantIDs)
	}

	// The assistant tool-call messages carry the parsed calls.
	if len(history) < 6 {
		t.Fatalf("history has %d messages, want at least 6: %+v", len(history), history)
	}
	if len(history[1].ToolCalls) != 2 {
		t.Fatalf("first assistant message has %d tool calls, want 2", len(history[1].ToolCalls))
	}
}

// ---------------------------------------------------------------------------
// Prefix stability and session resume
// ---------------------------------------------------------------------------

func TestPrefixStabilityAcrossTurnsAndSessionResume(t *testing.T) {
	workDir := t.TempDir()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	sess, err := store.Create(session.SessionMeta{Name: "prefix", Model: "deepseek-flash", WorkDir: workDir})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	p := provider.NewMockProvider()
	p.QueueSSE(sseFixture(t, "first answer", nil, &provider.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}))
	p.QueueSSE(sseFixture(t, "second answer", nil, &provider.Usage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22}))
	p.QueueSSE(sseFixture(t, "third answer", nil, &provider.Usage{PromptTokens: 30, CompletionTokens: 2, TotalTokens: 32}))

	cfg := testConfig()
	cfg.DebugCache = true
	var events []Event
	a := New(Options{
		Config:      cfg,
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		Session:     sess,
		WorkDir:     workDir,
		OnEvent:     func(ev Event) { events = append(events, ev) },
	})
	if _, err := a.RunTurn(context.Background(), "one", nil); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, err := a.RunTurn(context.Background(), "two", nil); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if countEvents(events, EventCacheDebug) < 2 {
		t.Fatalf("cache_debug events = %d, want at least 2", countEvents(events, EventCacheDebug))
	}

	// Resume from disk exactly as a CLI --continue would.
	resumed, err := store.Open(sess.ID())
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	a2 := New(Options{
		Config:      cfg,
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		Session:     resumed,
		WorkDir:     workDir,
	})
	if _, err := a2.RunTurn(context.Background(), "three", nil); err != nil {
		t.Fatalf("turn 3 after resume: %v", err)
	}

	requests := p.RequestSnapshot()
	if len(requests) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(requests))
	}
	var wantHash string
	for i, req := range requests {
		hash, err := provider.SystemToolsHash(req)
		if err != nil {
			t.Fatalf("system/tools hash: %v", err)
		}
		if i == 0 {
			wantHash = hash
		} else if hash != wantHash {
			t.Fatalf("request %d system+tools hash = %s, want %s", i, hash, wantHash)
		}
	}

	// The final request before the resume must be a byte-identical prefix of
	// the resumed request once old reasoning is stripped the same way.
	prev := provider.PrepareMessagesForRequest(requests[1].Messages)
	next := requests[2].Messages
	if len(next) < len(prev) {
		t.Fatalf("resumed request has %d messages, shorter than prior prefix %d", len(next), len(prev))
	}
	for i := range prev {
		if !reflect.DeepEqual(prev[i], next[i]) {
			t.Fatalf("message %d diverged after resume:\nprev=%+v\nnext=%+v", i, prev[i], next[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Budget, max steps, cancellation
// ---------------------------------------------------------------------------

func TestBudgetStopsTurnAndSkipsTools(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, workDir, "a.txt", "hello\n")

	p := provider.NewMockProvider()
	p.QueueSSE(sseFixture(t, "", []sseCall{
		{id: "call_a", name: tools.ToolEdit, args: `{"path":"a.txt","old_string":"hello","new_string":"changed"}`},
	}, &provider.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}))
	// A second script must never be consumed.
	p.QueueSSE(sseFixture(t, "should not run", nil, nil))

	cfg := testConfig()
	pricing := cfg.Pricing["deepseek-flash"]
	pricing.OffPeakCacheMissPerM = 1_000_000
	pricing.PeakCacheMissPerM = 1_000_000
	pricing.OffPeakCacheHitPerM = 1_000_000
	pricing.PeakCacheHitPerM = 1_000_000
	pricing.OffPeakOutputPerM = 1_000_000
	pricing.PeakOutputPerM = 1_000_000
	cfg.Pricing["deepseek-flash"] = pricing
	cfg.MaxCostUSD = 0.000001

	a := New(Options{
		Config:      cfg,
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		WorkDir:     workDir,
	})
	res, err := a.RunTurn(context.Background(), "edit it", nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.StopReason != StopBudget {
		t.Fatalf("stop reason = %q, want budget", res.StopReason)
	}
	if len(p.RequestSnapshot()) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(p.RequestSnapshot()))
	}
	if got := readFile(t, filepath.Join(workDir, "a.txt")); got != "hello\n" {
		t.Fatalf("tool ran despite budget stop: %q", got)
	}
	if _, ok := findToolMessage(a.History(), "not executed: cost budget exceeded"); !ok {
		t.Fatalf("missing skipped tool result: %+v", a.History())
	}
}

func TestMaxStepsStopsTurn(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, workDir, "a.txt", "hello\n")

	p := provider.NewMockProvider()
	for i := 0; i < 3; i++ {
		p.QueueSSE(sseFixture(t, "", []sseCall{
			{id: fmt.Sprintf("call_%d", i), name: tools.ToolRead, args: `{"path":"a.txt"}`},
		}, &provider.Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11}))
	}
	cfg := testConfig()
	cfg.MaxSteps = 2
	a := New(Options{
		Config:      cfg,
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		WorkDir:     workDir,
	})
	res, err := a.RunTurn(context.Background(), "loop", nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.StopReason != StopMaxSteps {
		t.Fatalf("stop reason = %q, want max_steps", res.StopReason)
	}
	if res.Steps != 2 {
		t.Fatalf("steps = %d, want 2", res.Steps)
	}
	if len(p.RequestSnapshot()) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(p.RequestSnapshot()))
	}
}

func TestCancellationStopsCleanly(t *testing.T) {
	workDir := t.TempDir()
	p := provider.NewMockProvider()
	a := New(Options{
		Config:      testConfig(),
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		WorkDir:     workDir,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.RunTurn(ctx, "cancel me", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(p.RequestSnapshot()) != 0 {
		t.Fatal("provider was called after cancellation")
	}
}

// ---------------------------------------------------------------------------
// Permissions
// ---------------------------------------------------------------------------

func TestPermissionPlanModeDeniesMutation(t *testing.T) {
	workDir := t.TempDir()
	path := writeFile(t, workDir, "a.txt", "hello\n")

	p := provider.NewMockProvider()
	p.QueueSSE(sseFixture(t, "", []sseCall{
		{id: "call_edit", name: tools.ToolEdit, args: `{"path":"a.txt","old_string":"hello","new_string":"changed"}`},
	}, &provider.Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11}))
	p.QueueSSE(sseFixture(t, "still here", nil, nil))

	cfg := testConfig()
	cfg.PermissionMode = string(permission.ModePlan)
	var events []Event
	a := New(Options{
		Config:      cfg,
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: permission.New(permission.Policy{Mode: permission.ModePlan, WorkDir: workDir}),
		WorkDir:     workDir,
		OnEvent:     func(ev Event) { events = append(events, ev) },
	})
	res, err := a.RunTurn(context.Background(), "change it", nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.StopReason != StopComplete {
		t.Fatalf("stop reason = %q", res.StopReason)
	}
	if got := readFile(t, path); got != "hello\n" {
		t.Fatalf("plan mode mutated file: %q", got)
	}
	msg, ok := findToolMessage(a.History(), "permission denied")
	if !ok || !strings.Contains(msg.Content, "plan mode") {
		t.Fatalf("missing plan denial message: %+v", a.History())
	}
	for _, ev := range events {
		if ev.Type == EventToolEnd && ev.ToolName == tools.ToolEdit && !ev.IsError {
			t.Fatalf("denied edit reported success: %+v", ev)
		}
	}
}

func TestAskApprovalAllowWithDiffPreview(t *testing.T) {
	workDir := t.TempDir()
	path := writeFile(t, workDir, "a.txt", "hello\n")

	p := provider.NewMockProvider()
	p.QueueSSE(sseFixture(t, "", []sseCall{
		{id: "call_edit", name: tools.ToolEdit, args: `{"path":"a.txt","old_string":"hello","new_string":"changed"}`},
	}, &provider.Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11}))
	p.QueueSSE(sseFixture(t, "approved done", nil, nil))

	var approvals []ApprovalRequest
	a := New(Options{
		Config:      testConfig(),
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: permission.New(permission.Policy{Mode: permission.ModeAsk, WorkDir: workDir}),
		WorkDir:     workDir,
		OnApprove: func(_ context.Context, req ApprovalRequest) (bool, error) {
			approvals = append(approvals, req)
			return true, nil
		},
	})
	res, err := a.RunTurn(context.Background(), "edit", nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.StopReason != StopComplete {
		t.Fatalf("stop reason = %q", res.StopReason)
	}
	if len(approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(approvals))
	}
	if approvals[0].ToolName != tools.ToolEdit || approvals[0].Path != "a.txt" {
		t.Fatalf("approval request = %+v", approvals[0])
	}
	if !strings.Contains(approvals[0].Diff, "-hello") || !strings.Contains(approvals[0].Diff, "+changed") {
		t.Fatalf("diff preview missing change: %q", approvals[0].Diff)
	}
	if got := readFile(t, path); got != "changed\n" {
		t.Fatalf("file = %q, want changed", got)
	}
}

func TestAskApprovalDenyDoesNotMutate(t *testing.T) {
	workDir := t.TempDir()
	path := writeFile(t, workDir, "a.txt", "hello\n")

	p := provider.NewMockProvider()
	p.QueueSSE(sseFixture(t, "", []sseCall{
		{id: "call_edit", name: tools.ToolEdit, args: `{"path":"a.txt","old_string":"hello","new_string":"changed"}`},
	}, &provider.Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11}))
	p.QueueSSE(sseFixture(t, "denied done", nil, nil))

	a := New(Options{
		Config:      testConfig(),
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: permission.New(permission.Policy{Mode: permission.ModeAsk, WorkDir: workDir}),
		WorkDir:     workDir,
		OnApprove: func(_ context.Context, _ ApprovalRequest) (bool, error) {
			return false, nil
		},
	})
	if _, err := a.RunTurn(context.Background(), "edit", nil); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if got := readFile(t, path); got != "hello\n" {
		t.Fatalf("denied edit mutated file: %q", got)
	}
	msg, ok := findToolMessage(a.History(), "user denied")
	if !ok || !strings.Contains(strings.ToLower(msg.Content), "denied") {
		t.Fatalf("missing denial tool result: %+v", a.History())
	}
}

// ---------------------------------------------------------------------------
// Compaction
// ---------------------------------------------------------------------------

func TestCheapPruneReplacesOldLargeToolOutput(t *testing.T) {
	big := strings.Repeat("x", 200)
	history := []provider.Message{
		{Role: provider.RoleTool, ToolCallID: "a", Content: big},
		{Role: provider.RoleAssistant, Content: "ok"},
		{Role: provider.RoleTool, ToolCallID: "b", Content: big},
		{Role: provider.RoleUser, Content: "tail question"},
		{Role: provider.RoleAssistant, Content: "tail answer"},
	}
	pruned, n := CheapPrune(history, 2, 100)
	if n != 2 {
		t.Fatalf("pruned = %d, want 2", n)
	}
	if !strings.Contains(pruned[0].Content, "pruned tool output") {
		t.Fatalf("old tool output was not pruned: %q", pruned[0].Content)
	}
	if !strings.Contains(pruned[2].Content, "pruned tool output") {
		t.Fatalf("second old tool output was not pruned: %q", pruned[2].Content)
	}
	if pruned[3].Content != "tail question" || pruned[4].Content != "tail answer" {
		t.Fatalf("tail was modified: %+v", pruned[3:])
	}
	if history[0].Content != big {
		t.Fatal("CheapPrune mutated its input")
	}
	if _, n = CheapPrune(history, 10, 100); n != 0 {
		t.Fatalf("pruned = %d with large keepTail, want 0", n)
	}
}

func TestCompactSummarizesHistoryAndPersists(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, workDir, "AGENTS.md", "Always run focused tests.")

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	sess, err := store.Create(session.SessionMeta{Name: "compact", Model: "deepseek-flash", WorkDir: workDir})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	todos := tools.NewTodoStore()
	if _, err := todos.Replace([]tools.TodoItem{
		{ID: "1", Content: "finish compaction", Status: tools.TodoInProgress},
	}); err != nil {
		t.Fatalf("todos: %v", err)
	}

	p := provider.NewMockProvider()
	p.QueueSSE(sseFixture(t, "first answer", nil, &provider.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}))
	p.QueueSSE(sseFixture(t, "second answer", nil, &provider.Usage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22}))
	p.QueueSSE(sseFixture(t, "1. Goal: keep working\n2. Decisions: none\n3. Files: a.txt\n4. Findings: none\n5. Open TODOs: finish\n6. Next steps: continue", nil, &provider.Usage{PromptTokens: 30, CompletionTokens: 20, TotalTokens: 50}))

	a := New(Options{
		Config:      testConfig(),
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		Session:     sess,
		Todos:       todos,
		WorkDir:     workDir,
	})
	if _, err := a.RunTurn(context.Background(), "first question", nil); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, err := a.RunTurn(context.Background(), "second question", nil); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	before := len(a.History())
	if before != 4 {
		t.Fatalf("history before compact = %d, want 4", before)
	}

	res, err := a.Compact(context.Background(), "focus on open work")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.MessagesSummarized != 2 || res.KeptTail != 2 {
		t.Fatalf("compact counts = summarized %d tail %d, want 2/2", res.MessagesSummarized, res.KeptTail)
	}
	if !strings.Contains(res.Summary, "Goal: keep working") {
		t.Fatalf("summary = %q", res.Summary)
	}
	if !strings.Contains(res.SummaryMessage.Content, "Always run focused tests.") {
		t.Fatalf("AGENTS.md not re-injected: %q", res.SummaryMessage.Content)
	}
	if !strings.Contains(res.SummaryMessage.Content, "finish compaction") {
		t.Fatalf("todo list not preserved: %q", res.SummaryMessage.Content)
	}
	if !strings.Contains(res.SummaryMessage.Content, "permission mode:") {
		t.Fatalf("plan state not preserved: %q", res.SummaryMessage.Content)
	}
	history := a.History()
	if len(history) != 3 {
		t.Fatalf("history after compact = %d, want 3", len(history))
	}
	if !strings.Contains(history[0].Content, "conversation_summary") {
		t.Fatalf("compacted history does not start with summary: %+v", history[0])
	}
	if history[1].Content != "second question" || history[2].Content != "second answer" {
		t.Fatalf("recent tail not preserved: %+v", history[1:])
	}
	if len(sess.Compactions()) != 1 {
		t.Fatalf("session compactions = %d, want 1", len(sess.Compactions()))
	}
	replay := sess.ReplayMessages()
	if len(replay) != 3 || !strings.Contains(replay[0].Content, "conversation_summary") {
		t.Fatalf("session replay did not apply compaction: %+v", replay)
	}
}

func TestAutoCompactOnContextTooLongRetriesOnce(t *testing.T) {
	workDir := t.TempDir()
	p := provider.NewMockProvider()
	p.QueueScript(provider.StreamEvent{Type: provider.EventError, Err: provider.ErrContextTooLong})
	p.QueueSSE(sseFixture(t, "1. Goal: recover\n6. Next steps: retry", nil, nil))
	p.QueueSSE(sseFixture(t, "Recovered.", nil, &provider.Usage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22}))

	var events []Event
	a := New(Options{
		Config:      testConfig(),
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		WorkDir:     workDir,
		OnEvent:     func(ev Event) { events = append(events, ev) },
	})
	res, err := a.RunTurn(context.Background(), "a very long request", nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !res.Compacted {
		t.Fatal("result did not report compaction")
	}
	if res.StopReason != StopComplete || !strings.Contains(res.Text, "Recovered.") {
		t.Fatalf("result = %+v", res)
	}
	if len(p.RequestSnapshot()) != 3 {
		t.Fatalf("provider calls = %d, want 3 (error + summary + retry)", len(p.RequestSnapshot()))
	}
	if countEvents(events, EventCompact) != 1 {
		t.Fatalf("compact events = %d, want 1", countEvents(events, EventCompact))
	}
	history := a.History()
	if len(history) == 0 || !strings.Contains(history[0].Content, "conversation_summary") {
		t.Fatalf("history was not compacted: %+v", history)
	}
}

// ---------------------------------------------------------------------------
// Vision gating
// ---------------------------------------------------------------------------

func TestVisionGating(t *testing.T) {
	image := provider.ContentPart{Type: "image_url", ImageURL: &provider.ImageURL{URL: "data:image/png;base64,AAAA"}}

	t.Run("supported model", func(t *testing.T) {
		workDir := t.TempDir()
		p := provider.NewMockProvider()
		p.QueueSSE(sseFixture(t, "I see it.", nil, nil))
		cfg := testConfig()
		cfg.Model = "deepseek-flash"
		a := New(Options{
			Config:      cfg,
			Provider:    p,
			Registry:    newRegistry(t),
			Permissions: yoloPermissions(workDir),
			WorkDir:     workDir,
		})
		if _, err := a.RunTurn(context.Background(), "what is this?", []provider.ContentPart{image}); err != nil {
			t.Fatalf("vision-capable model rejected image: %v", err)
		}
		req := p.RequestSnapshot()[0]
		last := req.Messages[len(req.Messages)-1]
		if len(last.Parts) != 2 || last.Parts[1].ImageURL == nil {
			t.Fatalf("image part not forwarded: %+v", last)
		}
	})

	t.Run("unsupported model", func(t *testing.T) {
		workDir := t.TempDir()
		p := provider.NewMockProvider()
		cfg := testConfig()
		cfg.Model = "deepseek-v4-pro"
		a := New(Options{
			Config:      cfg,
			Provider:    p,
			Registry:    newRegistry(t),
			Permissions: yoloPermissions(workDir),
			WorkDir:     workDir,
		})
		_, err := a.RunTurn(context.Background(), "what is this?", []provider.ContentPart{image})
		if err == nil || !strings.Contains(err.Error(), "does not support image") {
			t.Fatalf("err = %v, want clear vision rejection", err)
		}
		if len(p.RequestSnapshot()) != 0 {
			t.Fatal("provider was called for a rejected image")
		}
	})
}

// ---------------------------------------------------------------------------
// Adapters
// ---------------------------------------------------------------------------

type fakeSearcher struct {
	results []web.SearchResult
}

func (f fakeSearcher) Search(_ context.Context, _ string, _ int) ([]web.SearchResult, error) {
	return f.results, nil
}

func TestWebAdapters(t *testing.T) {
	fetch := NewWebFetchTool(web.FetchOptions{})
	if fetch.Name() != ToolWebFetch || !fetch.IsReadOnly() || !fetch.IsWeb() || !fetch.IsParallelSafe() {
		t.Fatalf("web_fetch flags wrong: %+v", fetch)
	}
	if !json.Valid(fetch.Parameters()) {
		t.Fatal("web_fetch schema is not valid JSON")
	}

	search := NewWebSearchTool(fakeSearcher{results: []web.SearchResult{
		{Title: "Tern", URL: "https://example.com", Snippet: "agent"},
	}})
	if search.Name() != ToolWebSearch || !search.IsReadOnly() || !search.IsWeb() || !search.IsParallelSafe() {
		t.Fatalf("web_search flags wrong: %+v", search)
	}
	res, err := search.Execute(context.Background(), tools.ExecEnv{WorkDir: t.TempDir()}, json.RawMessage(`{"query":"tern"}`))
	if err != nil {
		t.Fatalf("web_search execute: %v", err)
	}
	if !res.External || !strings.Contains(res.Output, "Tern") {
		t.Fatalf("web_search result = %+v", res)
	}

	mcp := NewMCPTool(nil, ext.MCPTool{
		Name:        "mcp_demo_echo",
		Description: "Echo",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Server:      "demo",
	})
	if mcp.Name() != "mcp_demo_echo" || mcp.Description() != "Echo" || mcp.IsParallelSafe() {
		t.Fatalf("mcp adapter flags wrong: %+v", mcp)
	}
	if _, err := mcp.Execute(context.Background(), tools.ExecEnv{}, json.RawMessage(`{"text":"hi"}`)); err == nil {
		t.Fatal("mcp execute with nil manager should fail")
	}
}
