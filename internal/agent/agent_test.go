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
	"testing"
	"time"

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

// ---------------------------------------------------------------------------
// Generation speed and runtime switching
// ---------------------------------------------------------------------------

func TestStreamMetricsReportedOnUsageAndTurnEnd(t *testing.T) {
	workDir := t.TempDir()
	p := provider.NewMockProvider()
	p.QueueSSE(sseFixture(t, "speed test", nil, &provider.Usage{
		PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20,
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

	res, err := a.RunTurn(context.Background(), "measure this", nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.DurationMs < 0 {
		t.Fatalf("turn duration_ms = %d, want non-negative", res.DurationMs)
	}
	if res.TokensPerSec < 0 {
		t.Fatalf("turn tokens_per_sec = %v, want non-negative", res.TokensPerSec)
	}
	if res.TokensPerSec == 0 {
		t.Fatalf("turn tokens_per_sec = 0, want positive for %d completion tokens", res.Usage.CompletionTokens)
	}

	var usageEv, endEv *Event
	for i := range events {
		switch events[i].Type {
		case EventUsage:
			usageEv = &events[i]
		case EventTurnEnd:
			endEv = &events[i]
		}
	}
	if usageEv == nil {
		t.Fatalf("missing usage event: %+v", events)
	}
	if endEv == nil {
		t.Fatalf("missing turn_end event: %+v", events)
	}
	for name, ev := range map[string]*Event{"usage": usageEv, "turn_end": endEv} {
		if ev.DurationMs < 0 {
			t.Fatalf("%s duration_ms = %d, want non-negative", name, ev.DurationMs)
		}
		if ev.TokensPerSec < 0 {
			t.Fatalf("%s tokens_per_sec = %v, want non-negative", name, ev.TokensPerSec)
		}
		if ev.TokensPerSec == 0 {
			t.Fatalf("%s tokens_per_sec = 0, want positive", name)
		}
	}

	// The JSON envelope must carry the new fields.
	raw, err := json.Marshal(*endEv)
	if err != nil {
		t.Fatalf("marshal turn_end: %v", err)
	}
	for _, key := range []string{`"duration_ms"`, `"tokens_per_sec"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("turn_end JSON missing %s: %s", key, raw)
		}
	}
}

func TestSetModelAndReasoningEffort(t *testing.T) {
	workDir := t.TempDir()
	cfg := testConfig()
	cfg.ReasoningEffort = ""
	a := New(Options{
		Config:      cfg,
		Provider:    provider.NewMockProvider(),
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		WorkDir:     workDir,
	})

	a.SetModel("deepseek-v4-pro")
	if a.Config.Model != "deepseek-v4-pro" {
		t.Fatalf("model = %q, want deepseek-v4-pro", a.Config.Model)
	}
	if got := a.ReasoningEffort(); got != "" {
		t.Fatalf("reasoning effort = %q, want empty default", got)
	}
	a.SetReasoningEffort("high")
	if got := a.ReasoningEffort(); got != "high" {
		t.Fatalf("reasoning effort = %q, want high", got)
	}
	req := a.buildRequest()
	if req.ReasoningEffort != "high" {
		t.Fatalf("request reasoning effort = %q, want high", req.ReasoningEffort)
	}
	a.SetReasoningEffort("")
	if got := a.ReasoningEffort(); got != "" {
		t.Fatalf("reasoning effort after clear = %q, want empty", got)
	}
}

func TestSwitchSessionPreservesHistoryUsageAndFirstTurnContext(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, workDir, "AGENTS.md", "Project convention: keep tests focused.")

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	populated, err := store.Create(session.SessionMeta{Name: "populated", Model: "deepseek-flash", WorkDir: workDir})
	if err != nil {
		t.Fatalf("create populated session: %v", err)
	}
	fresh, err := store.Create(session.SessionMeta{Name: "fresh", Model: "deepseek-flash", WorkDir: workDir})
	if err != nil {
		t.Fatalf("create fresh session: %v", err)
	}

	p := provider.NewMockProvider()
	p.QueueSSE(sseFixture(t, "first answer", nil, &provider.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}))
	p.QueueSSE(sseFixture(t, "fresh answer", nil, &provider.Usage{PromptTokens: 20, CompletionTokens: 3, TotalTokens: 23}))
	p.QueueSSE(sseFixture(t, "resumed answer", nil, &provider.Usage{PromptTokens: 30, CompletionTokens: 4, TotalTokens: 34}))

	a := New(Options{
		Config:      testConfig(),
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		Session:     populated,
		WorkDir:     workDir,
	})
	if _, err := a.RunTurn(context.Background(), "hello populated", nil); err != nil {
		t.Fatalf("populated turn: %v", err)
	}
	populatedHistory := a.History()
	populatedUsage, populatedCost := a.TotalUsage()
	if len(populatedHistory) != 2 {
		t.Fatalf("populated history = %d messages, want 2", len(populatedHistory))
	}
	if populatedUsage.CompletionTokens != 2 || populatedUsage.PromptTokens != 10 {
		t.Fatalf("populated usage = %+v", populatedUsage)
	}

	a.SwitchSession(fresh)
	if a.Session != fresh {
		t.Fatal("active session was not switched")
	}
	if got := a.History(); len(got) != 0 {
		t.Fatalf("fresh history = %d messages, want 0: %+v", len(got), got)
	}
	if usage, cost := a.TotalUsage(); usage != (provider.Usage{}) || cost != 0 {
		t.Fatalf("fresh usage = %+v cost %v, want zero", usage, cost)
	}
	if a.firstUserSent {
		t.Fatal("fresh session should not mark first user message as sent")
	}

	if _, err := a.RunTurn(context.Background(), "hello fresh", nil); err != nil {
		t.Fatalf("fresh turn: %v", err)
	}
	requests := p.RequestSnapshot()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requests))
	}
	freshUser := requests[1].Messages[len(requests[1].Messages)-1]
	if !strings.Contains(freshUser.Content, "<environment>") {
		t.Fatalf("fresh session first user message did not include dynamic context: %q", freshUser.Content)
	}
	if !strings.Contains(freshUser.Content, "keep tests focused") {
		t.Fatalf("fresh session first user message did not include project instructions: %q", freshUser.Content)
	}
	if got := a.History(); len(got) != 2 {
		t.Fatalf("fresh history after turn = %d messages, want 2", len(got))
	}
	if usage, _ := a.TotalUsage(); usage.CompletionTokens != 3 {
		t.Fatalf("fresh usage after turn = %+v, want only fresh session usage", usage)
	}

	// Switch back: the populated transcript and accounting return intact.
	a.SwitchSession(populated)
	if got := a.History(); !reflect.DeepEqual(got, populatedHistory) {
		t.Fatalf("restored history = %+v, want %+v", got, populatedHistory)
	}
	gotUsage, gotCost := a.TotalUsage()
	if gotUsage != populatedUsage || gotCost != populatedCost {
		t.Fatalf("restored usage = %+v cost %v, want %+v cost %v", gotUsage, gotCost, populatedUsage, populatedCost)
	}
	if !a.firstUserSent {
		t.Fatal("populated session should mark first user message as sent")
	}

	if _, err := a.RunTurn(context.Background(), "hello again", nil); err != nil {
		t.Fatalf("resumed turn: %v", err)
	}
	requests = p.RequestSnapshot()
	if len(requests) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(requests))
	}
	resumedUser := requests[2].Messages[len(requests[2].Messages)-1]
	if strings.Contains(resumedUser.Content, "<environment>") {
		t.Fatalf("resumed session re-prepended dynamic context: %q", resumedUser.Content)
	}
	if resumedUser.Content != "hello again" {
		t.Fatalf("resumed user message = %q, want plain input", resumedUser.Content)
	}
	if usage, _ := a.TotalUsage(); usage.CompletionTokens != 2+4 {
		t.Fatalf("resumed usage = %+v, want cumulative 6 completion tokens", usage)
	}
}

// ---------------------------------------------------------------------------
// Answer-text-only speed accounting
// ---------------------------------------------------------------------------

type pacedEvent struct {
	delay time.Duration
	ev    provider.StreamEvent
}

// pacedProvider emits scripted stream steps with configurable delays so tests
// can distinguish reasoning, tool, and answer-text phases.
type pacedProvider struct {
	turns [][]pacedEvent
	next  int
}

func (p *pacedProvider) ModelInfo(model string) provider.ModelCapabilities {
	return provider.DefaultModelCapabilities(model)
}

func (p *pacedProvider) Stream(ctx context.Context, _ provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	if p.next >= len(p.turns) {
		return nil, fmt.Errorf("paced provider: no script for turn %d", p.next)
	}
	script := p.turns[p.next]
	p.next++

	out := make(chan provider.StreamEvent)
	go func() {
		defer close(out)
		for _, pe := range script {
			if pe.delay > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(pe.delay):
				}
			}
			select {
			case <-ctx.Done():
				return
			case out <- pe.ev:
			}
		}
	}()
	return out, nil
}

// slowTool is a read-only tool whose execution can be delayed in tests.
type slowTool struct {
	delay time.Duration
}

func (t slowTool) Name() string { return "slow_tool" }
func (t slowTool) Description() string {
	return "A test tool that sleeps before returning."
}
func (t slowTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t slowTool) IsReadOnly() bool            { return true }
func (t slowTool) IsWeb() bool                 { return false }
func (t slowTool) IsParallelSafe() bool        { return true }
func (t slowTool) Execute(ctx context.Context, _ tools.ExecEnv, _ json.RawMessage) (tools.Result, error) {
	select {
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	case <-time.After(t.delay):
	}
	return tools.Result{Output: "slow tool done"}, nil
}

func TestTokensPerSecZeroForReasoningAndToolCallsOnly(t *testing.T) {
	workDir := t.TempDir()
	p := &pacedProvider{turns: [][]pacedEvent{
		{
			{delay: 0, ev: provider.StreamEvent{Type: provider.EventReasoningDelta, Text: "thinking about the tool call"}},
			{delay: 0, ev: provider.StreamEvent{
				Type:          provider.EventToolCallDelta,
				ToolCallIndex: 0,
				ToolCallID:    "call_slow",
				ToolName:      "slow_tool",
				ToolArgsDelta: `{}`,
			}},
			{delay: 0, ev: provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{
				PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, ReasoningTokens: 20,
			}}},
		},
		{
			{delay: 0, ev: provider.StreamEvent{Type: provider.EventReasoningDelta, Text: "more private reasoning, still no answer text"}},
			{delay: 0, ev: provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{
				PromptTokens: 12, CompletionTokens: 9, TotalTokens: 21, ReasoningTokens: 9,
			}}},
		},
	}}

	var events []Event
	a := New(Options{
		Config:      testConfig(),
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		WorkDir:     workDir,
		OnEvent:     func(ev Event) { events = append(events, ev) },
	})
	a.Registry.Register(slowTool{delay: 20 * time.Millisecond})

	res, err := a.RunTurn(context.Background(), "call the tool", nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.Usage.CompletionTokens == 0 {
		t.Fatalf("usage = %+v, want non-zero completion tokens to make the zero-speed check meaningful", res.Usage)
	}
	if res.TokensPerSec != 0 {
		t.Fatalf("turn tokens_per_sec = %v, want 0 when only reasoning and tool calls were emitted", res.TokensPerSec)
	}

	var endEv *Event
	for i := range events {
		if events[i].Type == EventTurnEnd {
			endEv = &events[i]
		}
	}
	if endEv == nil {
		t.Fatalf("missing turn_end event: %+v", events)
	}
	if endEv.TokensPerSec != 0 {
		t.Fatalf("turn_end tokens_per_sec = %v, want 0", endEv.TokensPerSec)
	}
}

func TestTokensPerSecUsesOnlyAnswerTextPhase(t *testing.T) {
	const textChunk = "abcdefghij" // 10 chars per delta
	workDir := t.TempDir()
	p := &pacedProvider{turns: [][]pacedEvent{
		{
			{delay: 60 * time.Millisecond, ev: provider.StreamEvent{Type: provider.EventReasoningDelta, Text: "slow reasoning before the tool call"}},
			{delay: 0, ev: provider.StreamEvent{
				Type:          provider.EventToolCallDelta,
				ToolCallIndex: 0,
				ToolCallID:    "call_slow",
				ToolName:      "slow_tool",
				ToolArgsDelta: `{}`,
			}},
			{delay: 0, ev: provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{
				PromptTokens: 20, CompletionTokens: 15, TotalTokens: 35, ReasoningTokens: 15,
			}}},
		},
		{
			{delay: 2 * time.Millisecond, ev: provider.StreamEvent{Type: provider.EventTextDelta, Text: textChunk}},
			{delay: 2 * time.Millisecond, ev: provider.StreamEvent{Type: provider.EventTextDelta, Text: textChunk}},
			{delay: 2 * time.Millisecond, ev: provider.StreamEvent{Type: provider.EventTextDelta, Text: textChunk}},
			{delay: 2 * time.Millisecond, ev: provider.StreamEvent{Type: provider.EventTextDelta, Text: textChunk}},
			{delay: 0, ev: provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{
				PromptTokens: 30, CompletionTokens: 10, TotalTokens: 40,
			}}},
		},
	}}

	var events []Event
	a := New(Options{
		Config:      testConfig(),
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		WorkDir:     workDir,
		OnEvent:     func(ev Event) { events = append(events, ev) },
	})
	a.Registry.Register(slowTool{delay: 60 * time.Millisecond})

	res, err := a.RunTurn(context.Background(), "answer after a slow tool", nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.Text != strings.Repeat(textChunk, 4) {
		t.Fatalf("text = %q, want the streamed answer", res.Text)
	}
	// Reasoning (~60ms) plus tool execution (~60ms) dominate wall-clock time.
	// If either leaked into the metric, speed would fall to roughly 10/0.12s.
	if res.DurationMs < 50 {
		t.Fatalf("turn duration_ms = %d, want the slow reasoning phase to be observed", res.DurationMs)
	}
	if res.TokensPerSec <= 500 {
		t.Fatalf("turn tokens_per_sec = %v, want answer-text-only speed (reasoning and tool time excluded)", res.TokensPerSec)
	}

	var endEv *Event
	for i := range events {
		if events[i].Type == EventTurnEnd {
			endEv = &events[i]
		}
	}
	if endEv == nil {
		t.Fatalf("missing turn_end event: %+v", events)
	}
	if endEv.TokensPerSec != res.TokensPerSec {
		t.Fatalf("turn_end tokens_per_sec = %v, want %v", endEv.TokensPerSec, res.TokensPerSec)
	}
}

// ---------------------------------------------------------------------------
// Mid-turn steering
// ---------------------------------------------------------------------------

// steerTool invokes a callback during execution so tests can queue a steering
// message while the tool batch is running.
type steerTool struct {
	steer func()
}

func (t steerTool) Name() string                { return "steer_tool" }
func (t steerTool) Description() string         { return "Queues a steering message during execution." }
func (t steerTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t steerTool) IsReadOnly() bool            { return true }
func (t steerTool) IsWeb() bool                 { return false }
func (t steerTool) IsParallelSafe() bool        { return false }
func (t steerTool) Execute(context.Context, tools.ExecEnv, json.RawMessage) (tools.Result, error) {
	if t.steer != nil {
		t.steer()
	}
	return tools.Result{Output: "steer tool done"}, nil
}

func TestSteerMidTurnAfterTool(t *testing.T) {
	workDir := t.TempDir()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	sess, err := store.Create(session.SessionMeta{Name: "steer-tool", Model: "deepseek-flash", WorkDir: workDir})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	const steerText = "focus only on main.go"
	p := provider.NewMockProvider()
	p.QueueSSE(sseFixture(t, "", []sseCall{
		{id: "call_steer", name: "steer_tool", args: `{}`},
	}, &provider.Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11}))
	p.QueueSSE(sseFixture(t, "Done after steering.", nil, &provider.Usage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22}))

	var events []Event
	a := New(Options{
		Config:      testConfig(),
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		Session:     sess,
		WorkDir:     workDir,
		OnEvent:     func(ev Event) { events = append(events, ev) },
	})
	a.Registry.Register(steerTool{steer: func() {
		if !a.Steer(steerText) {
			t.Error("Steer returned false for non-empty text")
		}
	}})

	res, err := a.RunTurn(context.Background(), "run the tool", nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.StopReason != StopComplete {
		t.Fatalf("stop reason = %q, want complete", res.StopReason)
	}
	if res.Steps != 2 {
		t.Fatalf("steps = %d, want 2", res.Steps)
	}

	requests := p.RequestSnapshot()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requests))
	}
	var steerMsg *provider.Message
	for i := range requests[1].Messages {
		msg := requests[1].Messages[i]
		if msg.Role == provider.RoleUser && msg.Content == steerText {
			steerMsg = &requests[1].Messages[i]
		}
	}
	if steerMsg == nil {
		t.Fatalf("next provider request does not include steering message: %+v", requests[1].Messages)
	}
	if requests[1].Messages[len(requests[1].Messages)-1].Content != steerText {
		t.Fatalf("steering message is not the last request message: %+v", requests[1].Messages)
	}

	var steerEv *Event
	for i := range events {
		if events[i].Type == EventSteer {
			steerEv = &events[i]
		}
	}
	if steerEv == nil {
		t.Fatalf("missing steer event: %+v", events)
	}
	if steerEv.Message != steerText || steerEv.Turn != res.Turn || steerEv.Step == 0 {
		t.Fatalf("steer event = %+v", steerEv)
	}

	replay := sess.ReplayMessages()
	found := false
	for _, msg := range replay {
		if msg.Role == provider.RoleUser && msg.Content == steerText {
			found = true
		}
	}
	if !found {
		t.Fatalf("steering message was not persisted to the session: %+v", replay)
	}
	if got := a.PendingSteers(); got != 0 {
		t.Fatalf("pending steers = %d, want 0 after drain", got)
	}
}

// steerStreamProvider queues a steering message from inside the first stream,
// after its answer text has been emitted but before the stream closes.
type steerStreamProvider struct {
	texts []string
	calls int
	steer func()
}

func (p *steerStreamProvider) ModelInfo(model string) provider.ModelCapabilities {
	return provider.DefaultModelCapabilities(model)
}

func (p *steerStreamProvider) Stream(ctx context.Context, _ provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	if p.calls >= len(p.texts) {
		return nil, fmt.Errorf("steer provider: no script for call %d", p.calls)
	}
	idx := p.calls
	p.calls++

	out := make(chan provider.StreamEvent, 4)
	go func() {
		defer close(out)
		select {
		case <-ctx.Done():
			return
		case out <- provider.StreamEvent{Type: provider.EventTextDelta, Text: p.texts[idx]}:
		}
		if idx == 0 && p.steer != nil {
			p.steer()
		}
		select {
		case <-ctx.Done():
		case out <- provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{
			PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11,
		}}:
		}
	}()
	return out, nil
}

func TestSteerDuringFinalStreamContinuesTurn(t *testing.T) {
	workDir := t.TempDir()
	const steerText = "also add a summary"
	p := &steerStreamProvider{texts: []string{"First answer.", "Summary added."}}

	var events []Event
	a := New(Options{
		Config:      testConfig(),
		Provider:    p,
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		WorkDir:     workDir,
		OnEvent:     func(ev Event) { events = append(events, ev) },
	})
	p.steer = func() {
		if !a.Steer(steerText) {
			t.Error("Steer returned false for non-empty text")
		}
	}

	res, err := a.RunTurn(context.Background(), "answer this", nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.StopReason != StopComplete {
		t.Fatalf("stop reason = %q, want complete", res.StopReason)
	}
	if res.Steps != 2 {
		t.Fatalf("steps = %d, want 2 (turn should continue for the steer)", res.Steps)
	}
	if !strings.Contains(res.Text, "Summary added.") {
		t.Fatalf("final text = %q, want the follow-up answer", res.Text)
	}
	if p.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", p.calls)
	}
	if countEvents(events, EventSteer) != 1 {
		t.Fatalf("steer events = %d, want 1: %+v", countEvents(events, EventSteer), events)
	}

	// The follow-up request must carry the steering user message after the
	// assistant's final answer.
	history := a.History()
	assistantIdx, steerIdx := -1, -1
	for i, msg := range history {
		if msg.Role == provider.RoleAssistant && strings.Contains(msg.Content, "First answer.") {
			assistantIdx = i
		}
		if msg.Role == provider.RoleUser && msg.Content == steerText {
			steerIdx = i
		}
	}
	if assistantIdx < 0 || steerIdx < 0 || steerIdx < assistantIdx {
		t.Fatalf("steer message not appended after the assistant answer: %+v", history)
	}
	if got := a.PendingSteers(); got != 0 {
		t.Fatalf("pending steers = %d, want 0 after drain", got)
	}
}

func TestSteerConcurrentCallsAreSafe(t *testing.T) {
	workDir := t.TempDir()
	a := New(Options{
		Config:      testConfig(),
		Provider:    provider.NewMockProvider(),
		Registry:    newRegistry(t),
		Permissions: yoloPermissions(workDir),
		WorkDir:     workDir,
	})

	if a.Steer("   ") {
		t.Fatal("Steer accepted whitespace-only text")
	}
	if a.Steer("") {
		t.Fatal("Steer accepted empty text")
	}

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if !a.Steer(fmt.Sprintf("steer %d", i)) {
				t.Errorf("Steer returned false for concurrent call %d", i)
			}
		}(i)
	}
	wg.Wait()

	if got := a.PendingSteers(); got != n {
		t.Fatalf("pending steers = %d, want %d", got, n)
	}
	a.ClearSteers()
	if got := a.PendingSteers(); got != 0 {
		t.Fatalf("pending steers after clear = %d, want 0", got)
	}
}
