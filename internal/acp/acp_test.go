package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
	"github.com/tern-agent/tern/internal/tools"
)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

type testClient struct {
	t   *testing.T
	in  *io.PipeWriter
	out *bufio.Reader
	ch  chan []byte

	next int
	mu   sync.Mutex
	// notifications collects every session/update payload received so far.
	notifications []map[string]any
	// requests collects server-initiated requests (permission prompts).
	requests []map[string]any
}

func newTestClient(t *testing.T, srv *Server) (*testClient, context.CancelFunc) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = srv.Serve(ctx, inR, outW)
		_ = outW.Close()
	}()

	c := &testClient{
		t:   t,
		in:  inW,
		out: bufio.NewReaderSize(outR, 64*1024),
		ch:  make(chan []byte, 256),
	}
	go func() {
		for {
			line, err := c.out.ReadBytes('\n')
			if len(line) > 0 {
				cp := append([]byte(nil), line...)
				select {
				case c.ch <- cp:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
		_ = outR.Close()
	})
	return c, cancel
}

func (c *testClient) send(v any) {
	c.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("marshal request: %v", err)
	}
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		c.t.Fatalf("write request: %v", err)
	}
}

func (c *testClient) request(method string, params any) string {
	c.t.Helper()
	c.next++
	id := strconv.Itoa(c.next)
	c.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      c.next,
		"method":  method,
		"params":  params,
	})
	return id
}

func (c *testClient) notify(method string, params any) {
	c.t.Helper()
	c.send(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

// wait reads messages until the response for id arrives. Notifications are
// buffered and server-initiated permission requests are auto-approved.
func (c *testClient) wait(id string) map[string]any {
	c.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case line, ok := <-c.ch:
			if !ok {
				c.t.Fatalf("stream closed while waiting for response %s", id)
			}
			var msg rpcIncoming
			if err := json.Unmarshal(line, &msg); err != nil {
				c.t.Fatalf("decode server message %q: %v", string(line), err)
			}
			if msg.Method != "" {
				var payload map[string]any
				_ = json.Unmarshal(line, &payload)
				if len(msg.ID) > 0 {
					c.mu.Lock()
					c.requests = append(c.requests, payload)
					c.mu.Unlock()
					c.send(map[string]any{
						"jsonrpc": "2.0",
						"id":      json.RawMessage(msg.ID),
						"result": map[string]any{
							"outcome": map[string]any{"outcome": "selected", "optionId": "allow_once"},
						},
					})
					continue
				}
				params, _ := payload["params"].(map[string]any)
				c.mu.Lock()
				c.notifications = append(c.notifications, params)
				c.mu.Unlock()
				continue
			}
			if string(msg.ID) == id {
				var payload map[string]any
				if err := json.Unmarshal(line, &payload); err != nil {
					c.t.Fatalf("decode response: %v", err)
				}
				return payload
			}
		case <-deadline:
			c.t.Fatalf("timed out waiting for response %s", id)
		}
	}
}

func (c *testClient) takeNotifications() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.notifications
	c.notifications = nil
	return out
}

type testRuntime struct {
	srv     *Server
	agent   *agent.Agent
	store   *session.Store
	session *session.Session
	workDir string
}

func newTestRuntime(t *testing.T, prov provider.Provider) *testRuntime {
	t.Helper()
	workDir := t.TempDir()
	store, err := session.NewStore(filepath.Join(workDir, "sessions"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess, err := store.Create(session.SessionMeta{Model: "deepseek-flash", WorkDir: workDir})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	cfg := config.Default()
	cfg.Model = "deepseek-flash"
	cfg.PermissionMode = string(permission.ModeYolo)
	cfg.WebPermission = string(permission.WebAllow)
	cfg.MaxSteps = 20

	perms := permission.New(permission.Policy{
		Mode:    permission.ModeYolo,
		Web:     permission.WebAllow,
		WorkDir: workDir,
	})
	registry := tools.NewEmptyRegistry()
	registry.Register(tools.NewReadTool())

	a := agent.New(agent.Options{
		Config:      cfg,
		Provider:    prov,
		Registry:    registry,
		Permissions: perms,
		Session:     sess,
		WorkDir:     workDir,
	})
	srv := NewServer(Options{
		Version:      "test-version",
		WorkDir:      workDir,
		Agent:        a,
		Permissions:  perms,
		Session:      sess,
		SessionStore: store,
	})
	return &testRuntime{srv: srv, agent: a, store: store, session: sess, workDir: workDir}
}

func findNotification(notifications []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, n := range notifications {
		if n["sessionUpdate"] == kind {
			out = append(out, n)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

func TestInitializeHandshake(t *testing.T) {
	rt := newTestRuntime(t, provider.NewMockProvider())
	c, _ := newTestClient(t, rt.srv)

	resp := c.wait(c.request("initialize", map[string]any{
		"protocolVersion": 1,
		"clientInfo":      map[string]any{"name": "test-host"},
	}))
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize returned no result: %+v", resp)
	}
	if got := int(result["protocolVersion"].(float64)); got != ProtocolVersion {
		t.Fatalf("protocolVersion = %d, want %d", got, ProtocolVersion)
	}
	info := result["agentInfo"].(map[string]any)
	if info["name"] != "tern" || info["title"] != "tern" || info["version"] != "test-version" {
		t.Fatalf("agentInfo = %+v", info)
	}
	caps := result["agentCapabilities"].(map[string]any)
	if caps["loadSession"] != true {
		t.Fatalf("loadSession capability missing: %+v", caps)
	}
	promptCaps := caps["promptCapabilities"].(map[string]any)
	if promptCaps["image"] != true || promptCaps["embeddedContext"] != true {
		t.Fatalf("promptCapabilities = %+v", promptCaps)
	}
}

// ---------------------------------------------------------------------------
// session/new, set_model, set_mode
// ---------------------------------------------------------------------------

func TestSessionModelAndMode(t *testing.T) {
	rt := newTestRuntime(t, provider.NewMockProvider())
	c, _ := newTestClient(t, rt.srv)
	c.wait(c.request("initialize", map[string]any{"protocolVersion": 1}))

	resp := c.wait(c.request("session/new", map[string]any{"cwd": rt.workDir}))
	result := resp["result"].(map[string]any)
	sessionID, _ := result["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("session/new returned no sessionId: %+v", resp)
	}
	models := result["models"].(map[string]any)
	if models["currentModelId"] != "deepseek-flash" {
		t.Fatalf("currentModelId = %v", models["currentModelId"])
	}
	available := models["availableModels"].([]any)
	if len(available) != 2 {
		t.Fatalf("availableModels = %+v", available)
	}
	modes := result["modes"].(map[string]any)
	if modes["currentModeId"] != "yolo" {
		t.Fatalf("currentModeId = %v, want yolo", modes["currentModeId"])
	}

	resp = c.wait(c.request("session/set_model", map[string]any{
		"sessionId": sessionID,
		"modelId":   "deepseek-v4-pro",
	}))
	if got := rt.agent.Config.Model; got != "deepseek-v4-pro" {
		t.Fatalf("agent model = %q, want deepseek-v4-pro", got)
	}
	models = resp["result"].(map[string]any)["models"].(map[string]any)
	if models["currentModelId"] != "deepseek-v4-pro" {
		t.Fatalf("set_model currentModelId = %v", models["currentModelId"])
	}

	// T3 Code sends its own mode ids. They must map onto tern's native modes.
	cases := map[string]permission.Mode{
		"full-access":       permission.ModeYolo,
		"auto":              permission.ModeYolo,
		"approval-required": permission.ModeAsk,
		"auto-accept-edits": permission.ModeAutoEdit,
		"plan":              permission.ModePlan,
		"yolo":              permission.ModeYolo,
	}
	for input, want := range cases {
		resp = c.wait(c.request("session/set_mode", map[string]any{
			"sessionId": sessionID,
			"modeId":    input,
		}))
		got := resp["result"].(map[string]any)["modes"].(map[string]any)["currentModeId"]
		if got != string(want) {
			t.Fatalf("mode %q mapped to %v, want %s", input, got, want)
		}
		if rt.agent.Permissions.Mode() != want {
			t.Fatalf("agent permissions mode = %s, want %s", rt.agent.Permissions.Mode(), want)
		}
	}
}

// ---------------------------------------------------------------------------
// session/prompt end to end
// ---------------------------------------------------------------------------

func TestPromptStreamsUpdatesAndUsage(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "hello.txt")
	if err := os.WriteFile(path, []byte("hello from tern\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	p := provider.NewMockProvider()
	p.QueueScript(
		provider.StreamEvent{Type: provider.EventReasoningDelta, Text: "thinking about the file"},
		provider.StreamEvent{Type: provider.EventTextDelta, Text: "Let me read that."},
		provider.StreamEvent{
			Type:          provider.EventToolCallDelta,
			ToolCallIndex: 0,
			ToolCallID:    "call_read_1",
			ToolName:      tools.ToolRead,
			ToolArgsDelta: `{"path":"hello.txt"}`,
		},
		provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{
			PromptTokens:          100,
			CompletionTokens:      20,
			TotalTokens:           120,
			PromptCacheHitTokens:  40,
			PromptCacheMissTokens: 60,
			ReasoningTokens:       5,
		}},
	)
	p.QueueScript(
		provider.StreamEvent{Type: provider.EventTextDelta, Text: "The file says hello."},
		provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{
			PromptTokens:          150,
			CompletionTokens:      10,
			TotalTokens:           160,
			PromptCacheHitTokens:  100,
			PromptCacheMissTokens: 50,
		}},
	)

	rt := newTestRuntime(t, p)
	// The runtime creates its own temp dir; put the file where the agent works.
	if err := os.WriteFile(filepath.Join(rt.workDir, "hello.txt"), []byte("hello from tern\n"), 0o644); err != nil {
		t.Fatalf("write file in workdir: %v", err)
	}
	c, _ := newTestClient(t, rt.srv)
	c.wait(c.request("initialize", map[string]any{"protocolVersion": 1}))
	newResp := c.wait(c.request("session/new", map[string]any{"cwd": rt.workDir}))
	sessionID := newResp["result"].(map[string]any)["sessionId"].(string)

	c.takeNotifications()
	resp := c.wait(c.request("session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt": []map[string]any{
			{"type": "text", "text": "read hello.txt"},
			{"type": "resource", "resource": map[string]any{"uri": "file:///note.md", "text": "extra context"}},
		},
	}))
	if errObj, ok := resp["error"]; ok {
		t.Fatalf("session/prompt failed: %+v", errObj)
	}

	notifications := c.takeNotifications()

	thoughts := findNotification(notifications, "agent_thought_chunk")
	if len(thoughts) == 0 {
		t.Fatal("missing agent_thought_chunk")
	}
	if got := thoughts[0]["content"].(map[string]any)["text"].(string); !strings.Contains(got, "thinking") {
		t.Fatalf("thought chunk = %v", got)
	}

	messages := findNotification(notifications, "agent_message_chunk")
	if len(messages) < 2 {
		t.Fatalf("agent_message_chunk count = %d, want >= 2", len(messages))
	}
	var messageText strings.Builder
	for _, m := range messages {
		messageText.WriteString(m["content"].(map[string]any)["text"].(string))
	}
	if !strings.Contains(messageText.String(), "Let me read") || !strings.Contains(messageText.String(), "file says hello") {
		t.Fatalf("message text = %q", messageText.String())
	}

	toolCalls := findNotification(notifications, "tool_call")
	if len(toolCalls) != 1 {
		t.Fatalf("tool_call count = %d, want 1", len(toolCalls))
	}
	tc := toolCalls[0]
	if tc["toolCallId"] != "call_read_1" || tc["status"] != "in_progress" {
		t.Fatalf("tool_call = %+v", tc)
	}
	if tc["kind"] != "read" {
		t.Fatalf("tool_call kind = %v, want read", tc["kind"])
	}
	rawInput := tc["rawInput"].(map[string]any)
	if rawInput["path"] != "hello.txt" {
		t.Fatalf("tool_call rawInput = %+v", rawInput)
	}

	updates := findNotification(notifications, "tool_call_update")
	if len(updates) != 1 {
		t.Fatalf("tool_call_update count = %d, want 1", len(updates))
	}
	update := updates[0]
	if update["toolCallId"] != "call_read_1" || update["status"] != "completed" {
		t.Fatalf("tool_call_update = %+v", update)
	}
	content := update["content"].([]any)[0].(map[string]any)
	inner := content["content"].(map[string]any)
	if !strings.Contains(inner["text"].(string), "hello from tern") {
		t.Fatalf("tool output = %v", inner["text"])
	}

	usageUpdates := findNotification(notifications, "usage_update")
	if len(usageUpdates) != 2 {
		t.Fatalf("usage_update count = %d, want 2", len(usageUpdates))
	}
	lastUsage := usageUpdates[len(usageUpdates)-1]
	if lastUsage["used"].(float64) <= 0 {
		t.Fatalf("usage used = %v", lastUsage["used"])
	}
	if lastUsage["size"].(float64) != float64(provider.DefaultContextWindow) {
		t.Fatalf("usage size = %v, want %d", lastUsage["size"], provider.DefaultContextWindow)
	}
	cost := lastUsage["cost"].(map[string]any)
	if cost["currency"] != "USD" || cost["amount"].(float64) < 0 {
		t.Fatalf("usage cost = %+v", cost)
	}
	meta := lastUsage["_meta"].(map[string]any)
	for _, key := range []string{"inputTokens", "outputTokens", "totalTokens", "cachedReadTokens", "cacheMissTokens", "reasoningTokens", "cacheHitRate", "tokensPerSec", "turnCostUsd", "totalCostUsd"} {
		if _, ok := meta[key]; !ok {
			t.Fatalf("usage _meta missing %q: %+v", key, meta)
		}
	}
	if meta["inputTokens"].(float64) != 150 || meta["outputTokens"].(float64) != 10 {
		t.Fatalf("last usage meta = %+v", meta)
	}

	result := resp["result"].(map[string]any)
	if result["stopReason"] != "end_turn" {
		t.Fatalf("stopReason = %v", result["stopReason"])
	}
	usage := result["usage"].(map[string]any)
	if usage["inputTokens"].(float64) != 250 ||
		usage["outputTokens"].(float64) != 30 ||
		usage["totalTokens"].(float64) != 280 ||
		usage["cachedReadTokens"].(float64) != 140 ||
		usage["thoughtTokens"].(float64) != 5 {
		t.Fatalf("prompt usage = %+v", usage)
	}
	promptMeta := result["_meta"].(map[string]any)
	if promptMeta["steps"].(float64) != 2 || promptMeta["toolCalls"].(float64) != 1 {
		t.Fatalf("prompt _meta = %+v", promptMeta)
	}
	if promptMeta["contextWindow"].(float64) != float64(provider.DefaultContextWindow) {
		t.Fatalf("prompt _meta contextWindow = %v", promptMeta["contextWindow"])
	}
	if promptMeta["totalCostUsd"].(float64) <= 0 {
		t.Fatalf("prompt _meta totalCostUsd = %v", promptMeta["totalCostUsd"])
	}
}

// ---------------------------------------------------------------------------
// session/stats
// ---------------------------------------------------------------------------

func TestSessionStats(t *testing.T) {
	p := provider.NewMockProvider()
	p.QueueScript(
		provider.StreamEvent{Type: provider.EventTextDelta, Text: "hello"},
		provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{
			PromptTokens:          80,
			CompletionTokens:      12,
			TotalTokens:           92,
			PromptCacheHitTokens:  50,
			PromptCacheMissTokens: 30,
		}},
	)
	rt := newTestRuntime(t, p)
	c, _ := newTestClient(t, rt.srv)
	c.wait(c.request("initialize", map[string]any{"protocolVersion": 1}))
	newResp := c.wait(c.request("session/new", map[string]any{"cwd": rt.workDir}))
	sessionID := newResp["result"].(map[string]any)["sessionId"].(string)
	c.wait(c.request("session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "hi"}},
	}))

	resp := c.wait(c.request("session/stats", map[string]any{"sessionId": sessionID}))
	result := resp["result"].(map[string]any)
	if result["sessionId"] != sessionID {
		t.Fatalf("stats sessionId = %v, want %s", result["sessionId"], sessionID)
	}
	usage := result["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 80 || usage["completion_tokens"].(float64) != 12 {
		t.Fatalf("stats usage = %+v", usage)
	}
	if result["totalCostUsd"].(float64) <= 0 {
		t.Fatalf("stats totalCostUsd = %v", result["totalCostUsd"])
	}
	if result["cacheHitRate"].(float64) <= 0 {
		t.Fatalf("stats cacheHitRate = %v", result["cacheHitRate"])
	}
	if result["contextTokens"].(float64) <= 0 {
		t.Fatalf("stats contextTokens = %v", result["contextTokens"])
	}
	if result["contextWindow"].(float64) != float64(provider.DefaultContextWindow) {
		t.Fatalf("stats contextWindow = %v", result["contextWindow"])
	}
	store, ok := result["store"].(map[string]any)
	if !ok {
		t.Fatalf("stats store missing: %+v", result)
	}
	if store["SessionCount"].(float64) < 1 {
		t.Fatalf("store SessionCount = %v", store["SessionCount"])
	}
	if store["TurnCount"].(float64) < 1 {
		t.Fatalf("store TurnCount = %v", store["TurnCount"])
	}
}

// ---------------------------------------------------------------------------
// session/cancel
// ---------------------------------------------------------------------------

// blockingProvider emits nothing until its context is cancelled.
type blockingProvider struct {
	started chan struct{}
	once    sync.Once
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{started: make(chan struct{})}
}

func (p *blockingProvider) Stream(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.once.Do(func() { close(p.started) })
	ch := make(chan provider.StreamEvent)
	go func() {
		defer close(ch)
		<-ctx.Done()
	}()
	return ch, nil
}

func (p *blockingProvider) ModelInfo(model string) provider.ModelCapabilities {
	return provider.DefaultModelCapabilities(model)
}

func TestCancelInFlightPrompt(t *testing.T) {
	bp := newBlockingProvider()
	rt := newTestRuntime(t, bp)
	c, _ := newTestClient(t, rt.srv)
	c.wait(c.request("initialize", map[string]any{"protocolVersion": 1}))
	newResp := c.wait(c.request("session/new", map[string]any{"cwd": rt.workDir}))
	sessionID := newResp["result"].(map[string]any)["sessionId"].(string)

	promptID := c.request("session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "wait forever"}},
	})

	select {
	case <-bp.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider was never called")
	}

	c.notify("session/cancel", map[string]any{"sessionId": sessionID})
	resp := c.wait(promptID)
	if errObj, ok := resp["error"]; ok {
		t.Fatalf("cancelled prompt returned error: %+v", errObj)
	}
	result := resp["result"].(map[string]any)
	if result["stopReason"] != "cancelled" {
		t.Fatalf("stopReason = %v, want cancelled", result["stopReason"])
	}
	if rt.agent.PendingSteers() != 0 {
		t.Fatalf("pending steers after cancel = %d", rt.agent.PendingSteers())
	}
	fmt.Fprintln(io.Discard, "cancel complete")
}
