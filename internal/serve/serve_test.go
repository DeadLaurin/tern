package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
	"github.com/tern-agent/tern/internal/tools"
)

type testEnv struct {
	server  *Server
	http    *httptest.Server
	agent   *agent.Agent
	mock    *provider.MockProvider
	session *session.Session
	workDir string
}

type testEnvOptions struct {
	password string
}

func newTestEnv(t *testing.T, opts testEnvOptions) *testEnv {
	t.Helper()

	workDir := t.TempDir()
	dataDir := t.TempDir()

	store, err := session.NewStore(dataDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	sess, err := store.Create(session.SessionMeta{Model: config.DefaultModel, WorkDir: workDir})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	cfg := config.Default()
	cfg.Model = "deepseek-flash"
	cfg.ReasoningEffort = "high"

	perms := permission.New(permission.Policy{
		Mode:    permission.ModeAsk,
		Web:     permission.WebPolicyAsk,
		WorkDir: workDir,
	})

	registry := tools.NewEmptyRegistry()
	registry.Register(tools.NewWriteTool())
	registry.Register(tools.NewReadTool())

	mock := provider.NewMockProvider()
	a := agent.New(agent.Options{
		Config:       cfg,
		Provider:     mock,
		Registry:     registry,
		Permissions:  perms,
		Session:      sess,
		WorkDir:      workDir,
		SystemPrompt: "test system prompt",
	})

	srv := New(Options{
		Agent:       a,
		Session:     sess,
		Store:       store,
		Config:      cfg,
		Permissions: perms,
		Version:     "test-version",
		WorkDir:     workDir,
		Password:    opts.password,
		AuthEnabled: opts.password != "",
	})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		hs.Close()
		srv.Close()
	})

	return &testEnv{server: srv, http: hs, agent: a, mock: mock, session: sess, workDir: workDir}
}

// doJSON performs a JSON request and returns the response along with the
// decoded body. The caller closes the body.
func doJSON(t *testing.T, client *http.Client, method, rawURL string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(data))
	}
	req, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	decoded := map[string]any{}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if len(data) > 0 {
		if err := json.Unmarshal(data, &decoded); err != nil {
			// Non-JSON (the HTML page) is fine; leave the map empty.
			decoded = map[string]any{}
		}
	}
	return resp, decoded
}

func newJarClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func hasCookie(jar http.CookieJar, rawURL, name string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	for _, c := range jar.Cookies(u) {
		if c.Name == name {
			return true
		}
	}
	return false
}

func TestAuthMiddlewareAndLoginFlow(t *testing.T) {
	env := newTestEnv(t, testEnvOptions{password: "correct-horse"})
	client := newJarClient(t)

	// The UI itself is reachable without a session so the login card can load.
	resp, _ := doJSON(t, client, http.MethodGet, env.http.URL+"/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}

	// Protected endpoints reject unauthenticated requests.
	resp, body := doJSON(t, client, http.MethodGet, env.http.URL+"/api/status", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/status = %d, want 401", resp.StatusCode)
	}
	if body["error"] == nil {
		t.Fatalf("expected JSON error, got %v", body)
	}

	// Auth status is public.
	resp, body = doJSON(t, client, http.MethodGet, env.http.URL+"/api/auth/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/auth/status = %d, want 200", resp.StatusCode)
	}
	if body["auth_enabled"] != true || body["authenticated"] != false {
		t.Fatalf("auth status = %v, want enabled and unauthenticated", body)
	}

	// Wrong password is rejected and sets no cookie.
	resp, _ = doJSON(t, client, http.MethodPost, env.http.URL+"/api/login", map[string]string{"password": "wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password login = %d, want 401", resp.StatusCode)
	}
	if hasCookie(client.Jar, env.http.URL, sessionCookieName) {
		t.Fatal("wrong password set a session cookie")
	}

	// Right password issues a cookie.
	resp, _ = doJSON(t, client, http.MethodPost, env.http.URL+"/api/login", map[string]string{"password": "correct-horse"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d, want 200", resp.StatusCode)
	}
	if !hasCookie(client.Jar, env.http.URL, sessionCookieName) {
		t.Fatal("login did not set a session cookie")
	}

	resp, body = doJSON(t, client, http.MethodGet, env.http.URL+"/api/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated /api/status = %d, want 200", resp.StatusCode)
	}
	if body["model"] != "deepseek-flash" {
		t.Fatalf("status model = %v, want deepseek-flash", body["model"])
	}

	// Logout revokes the session.
	resp, _ = doJSON(t, client, http.MethodPost, env.http.URL+"/api/logout", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout = %d, want 200", resp.StatusCode)
	}
	resp, _ = doJSON(t, client, http.MethodGet, env.http.URL+"/api/status", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status after logout = %d, want 401", resp.StatusCode)
	}
}

func TestStatusHistoryAndModel(t *testing.T) {
	env := newTestEnv(t, testEnvOptions{})
	client := newJarClient(t)

	resp, body := doJSON(t, client, http.MethodGet, env.http.URL+"/api/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/status = %d, want 200", resp.StatusCode)
	}
	if body["version"] != "test-version" {
		t.Fatalf("version = %v", body["version"])
	}
	if body["model"] != "deepseek-flash" {
		t.Fatalf("model = %v", body["model"])
	}
	if body["effort"] != "high" {
		t.Fatalf("effort = %v", body["effort"])
	}
	if body["permission"] != "ask" {
		t.Fatalf("permission = %v", body["permission"])
	}
	if body["session_id"] != env.session.ID() {
		t.Fatalf("session_id = %v, want %s", body["session_id"], env.session.ID())
	}
	if body["running"] != false {
		t.Fatalf("running = %v, want false", body["running"])
	}
	if body["auth_enabled"] != false {
		t.Fatalf("auth_enabled = %v, want false", body["auth_enabled"])
	}

	// Empty history is well-formed.
	resp, body = doJSON(t, client, http.MethodGet, env.http.URL+"/api/history", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/history = %d, want 200", resp.StatusCode)
	}
	msgs, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("history messages = %T, want array", body["messages"])
	}
	if len(msgs) != 0 {
		t.Fatalf("history messages = %d, want 0", len(msgs))
	}

	// Model and effort switch.
	resp, body = doJSON(t, client, http.MethodPost, env.http.URL+"/api/model", map[string]any{
		"model":  "deepseek-v4-pro",
		"effort": "low",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/model = %d, want 200", resp.StatusCode)
	}
	if body["model"] != "deepseek-v4-pro" || body["effort"] != "low" {
		t.Fatalf("model response = %v", body)
	}
	if got := env.agent.Config.Model; got != "deepseek-v4-pro" {
		t.Fatalf("agent model = %q, want deepseek-v4-pro", got)
	}
	if got := env.agent.ReasoningEffort(); got != "low" {
		t.Fatalf("agent effort = %q, want low", got)
	}

	// Invalid effort is rejected.
	resp, _ = doJSON(t, client, http.MethodPost, env.http.URL+"/api/model", map[string]any{"effort": "insane"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid effort = %d, want 400", resp.StatusCode)
	}
}

type sseEvent struct {
	Event string
	Data  string
}

// readSSE parses SSE frames from r into a channel. The channel closes when the
// stream ends.
func readSSE(r io.Reader) <-chan sseEvent {
	ch := make(chan sseEvent, 128)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 4096), 1<<20)
		var ev sseEvent
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if ev.Event != "" {
					ch <- ev
				}
				ev = sseEvent{}
			}
		}
	}()
	return ch
}

func waitForEvent(t *testing.T, ch <-chan sseEvent, name string) sseEvent {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("event stream closed while waiting for %q", name)
			}
			if ev.Event == name {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %q", name)
		}
	}
}

func openEventStream(t *testing.T, env *testEnv) (<-chan sseEvent, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, env.http.URL+"/api/events", nil)
	if err != nil {
		cancel()
		t.Fatalf("new SSE request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open SSE stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("SSE status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("SSE content type = %q", ct)
	}
	events := readSSE(resp.Body)
	go func() {
		<-ctx.Done()
		_ = resp.Body.Close()
	}()
	return events, cancel
}

func TestPromptStreamsEventsAndHistory(t *testing.T) {
	env := newTestEnv(t, testEnvOptions{})
	client := newJarClient(t)

	env.mock.QueueScript(
		provider.StreamEvent{Type: provider.EventReasoningDelta, Text: "thinking"},
		provider.StreamEvent{Type: provider.EventTextDelta, Text: "hello"},
		provider.StreamEvent{Type: provider.EventTextDelta, Text: " world"},
		provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{
			PromptTokens:         100,
			CompletionTokens:     10,
			TotalTokens:          110,
			PromptCacheHitTokens: 50,
		}},
		provider.StreamEvent{Type: provider.EventDone},
	)

	events, cancel := openEventStream(t, env)
	defer cancel()

	resp, body := doJSON(t, client, http.MethodPost, env.http.URL+"/api/prompt", map[string]string{"text": "say hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/prompt = %d, want 200", resp.StatusCode)
	}
	if body["started"] != true {
		t.Fatalf("prompt response = %v, want started", body)
	}

	waitForEvent(t, events, "turn_start")
	reasoning := waitForEvent(t, events, "reasoning_delta")
	if reasoning.Data == "" {
		t.Fatal("reasoning_delta had empty payload")
	}
	first := waitForEvent(t, events, "text_delta")
	if first.Data == "" {
		t.Fatal("text_delta had empty payload")
	}
	waitForEvent(t, events, "usage")
	end := waitForEvent(t, events, "turn_end")

	var turnEnd agent.Event
	if err := json.Unmarshal([]byte(end.Data), &turnEnd); err != nil {
		t.Fatalf("decode turn_end: %v", err)
	}
	if turnEnd.Reason != string(agent.StopComplete) {
		t.Fatalf("turn_end reason = %q, want complete", turnEnd.Reason)
	}
	if !strings.Contains(end.Data, "hello world") {
		t.Fatalf("turn_end payload missing assistant text: %s", end.Data)
	}

	// The turn must be marked as finished.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, status := doJSON(t, client, http.MethodGet, env.http.URL+"/api/status", nil)
		if status["running"] == false {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// History restores the conversation after a refresh.
	resp, body = doJSON(t, client, http.MethodGet, env.http.URL+"/api/history", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/history = %d, want 200", resp.StatusCode)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("history has %d messages, want at least 2", len(msgs))
	}
	firstMsg, _ := msgs[0].(map[string]any)
	if firstMsg["role"] != "user" || !strings.Contains(firstMsg["text"].(string), "say hi") {
		t.Fatalf("first history message = %v", firstMsg)
	}
	lastMsg, _ := msgs[len(msgs)-1].(map[string]any)
	if lastMsg["role"] != "assistant" || lastMsg["text"] != "hello world" {
		t.Fatalf("last history message = %v", lastMsg)
	}

	// Status reflects the accumulated usage.
	_, status := doJSON(t, client, http.MethodGet, env.http.URL+"/api/status", nil)
	usage, _ := status["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(100) {
		t.Fatalf("status usage = %v, want prompt_tokens 100", usage)
	}
	if status["cache_hit_rate"].(float64) == 0 {
		t.Fatalf("cache_hit_rate = %v, want non-zero", status["cache_hit_rate"])
	}
}

func toolCallScript(t *testing.T, path, content string) []provider.StreamEvent {
	t.Helper()
	args, err := json.Marshal(map[string]string{"path": path, "content": content})
	if err != nil {
		t.Fatalf("marshal tool args: %v", err)
	}
	return []provider.StreamEvent{
		{Type: provider.EventToolCallDelta, ToolCallIndex: 0, ToolCallID: "call_1", ToolName: "write", ToolArgsDelta: string(args)},
		{Type: provider.EventDone},
	}
}

// waitForApproval polls /api/status until a pending approval appears.
func waitForApproval(t *testing.T, client *http.Client, baseURL string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, status := doJSON(t, client, http.MethodGet, baseURL+"/api/status", nil)
		if pending, ok := status["pending_approval"].(map[string]any); ok && pending["id"] != nil {
			return pending
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for approval; last status = %v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForIdle(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, status := doJSON(t, client, http.MethodGet, baseURL+"/api/status", nil)
		if status["running"] == false {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for turn to finish; last status = %v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestApprovalDenyFlow(t *testing.T) {
	env := newTestEnv(t, testEnvOptions{})
	client := newJarClient(t)

	env.mock.QueueScript(toolCallScript(t, "denied.txt", "should not exist")...)
	env.mock.QueueScript(
		provider.StreamEvent{Type: provider.EventTextDelta, Text: "ok, skipped"},
		provider.StreamEvent{Type: provider.EventDone},
	)

	resp, _ := doJSON(t, client, http.MethodPost, env.http.URL+"/api/prompt", map[string]string{"text": "write a file"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/prompt = %d, want 200", resp.StatusCode)
	}

	pending := waitForApproval(t, client, env.http.URL)
	if pending["tool_name"] != "write" {
		t.Fatalf("pending tool = %v, want write", pending["tool_name"])
	}
	if pending["path"] != "denied.txt" {
		t.Fatalf("pending path = %v, want denied.txt", pending["path"])
	}
	id, _ := pending["id"].(string)
	if id == "" {
		t.Fatal("pending approval has no id")
	}

	resp, _ = doJSON(t, client, http.MethodPost, env.http.URL+"/api/approve", map[string]string{"id": id, "decision": "deny"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/approve = %d, want 200", resp.StatusCode)
	}

	waitForIdle(t, client, env.http.URL)
	if _, err := os.Stat(filepath.Join(env.workDir, "denied.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied write created a file (err = %v)", err)
	}

	// The denial is persisted as a tool result.
	_, body := doJSON(t, client, http.MethodGet, env.http.URL+"/api/history", nil)
	msgs, _ := body["messages"].([]any)
	found := false
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if msg["role"] == "tool" && strings.Contains(msg["text"].(string), "denied") {
			found = true
		}
	}
	if !found {
		t.Fatalf("history has no denial tool result: %v", msgs)
	}

	// Answering a stale id fails cleanly.
	resp, _ = doJSON(t, client, http.MethodPost, env.http.URL+"/api/approve", map[string]string{"id": id, "decision": "allow"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stale approve = %d, want 404", resp.StatusCode)
	}
}

func TestApprovalAllowFlow(t *testing.T) {
	env := newTestEnv(t, testEnvOptions{})
	client := newJarClient(t)

	env.mock.QueueScript(toolCallScript(t, "allowed.txt", "hello from tern")...)
	env.mock.QueueScript(
		provider.StreamEvent{Type: provider.EventTextDelta, Text: "written"},
		provider.StreamEvent{Type: provider.EventDone},
	)

	// Connect before prompting so the live approval_request frame is received.
	events, cancel := openEventStream(t, env)
	defer cancel()

	resp, _ := doJSON(t, client, http.MethodPost, env.http.URL+"/api/prompt", map[string]string{"text": "write a file"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/prompt = %d, want 200", resp.StatusCode)
	}

	approvalEvent := waitForEvent(t, events, "approval_request")
	if approvalEvent.Data == "" {
		t.Fatal("approval_request had empty payload")
	}
	var wire approvalWire
	if err := json.Unmarshal([]byte(approvalEvent.Data), &wire); err != nil {
		t.Fatalf("decode approval_request: %v", err)
	}
	if wire.ToolName != "write" || wire.ID == "" {
		t.Fatalf("approval_request payload = %+v", wire)
	}

	// Status exposes the same pending approval.
	pending := waitForApproval(t, client, env.http.URL)
	if pending["id"] != wire.ID {
		t.Fatalf("status pending id = %v, want %v", pending["id"], wire.ID)
	}

	resp, _ = doJSON(t, client, http.MethodPost, env.http.URL+"/api/approve", map[string]string{"id": wire.ID, "decision": "allow_session"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/approve = %d, want 200", resp.StatusCode)
	}
	waitForEvent(t, events, "approval_resolved")

	waitForIdle(t, client, env.http.URL)
	data, err := os.ReadFile(filepath.Join(env.workDir, "allowed.txt"))
	if err != nil {
		t.Fatalf("approved write missing: %v", err)
	}
	if string(data) != "hello from tern" {
		t.Fatalf("file contents = %q", string(data))
	}
}

func TestGeneratePasswordAndLANAddresses(t *testing.T) {
	pw, err := GeneratePassword()
	if err != nil {
		t.Fatalf("GeneratePassword: %v", err)
	}
	if len(pw) != 19 {
		t.Fatalf("password length = %d, want 19 (%q)", len(pw), pw)
	}
	for _, r := range pw {
		if r != '-' && !strings.ContainsRune(passwordAlphabet, r) {
			t.Fatalf("password contains unexpected rune %q", r)
		}
	}
	other, err := GeneratePassword()
	if err != nil {
		t.Fatalf("GeneratePassword: %v", err)
	}
	if pw == other {
		t.Fatal("two generated passwords are identical")
	}

	for _, ip := range LANAddresses() {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			t.Fatalf("LANAddresses returned invalid IP %q", ip)
		}
		if parsed.IsLoopback() || parsed.To4() == nil {
			t.Fatalf("LANAddresses returned a non-LAN address %q", ip)
		}
	}
}
