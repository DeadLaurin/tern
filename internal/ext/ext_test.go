package ext

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	helperEnv         = "TERN_EXT_HELPER"
	helperModeHook    = "hook-echo"
	helperModeBlock   = "hook-block"
	helperModeSleep   = "hook-sleep"
	helperModeMCPSvr  = "mcp-stdio"
	helperModeExtTool = "ext-tool"
)

// TestMain doubles as the entry point for helper subprocesses: hook scripts and
// the stdio MCP server are the test binary re-executed with helperEnv set. This
// keeps the tests cross-platform without requiring python or node.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		runHelper()
		return
	}
	os.Exit(m.Run())
}

func runHelper() {
	mode := os.Getenv("GO_HELPER_MODE")
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case helperModeHook:
		helperHookEcho()
	case helperModeBlock:
		helperHookBlock()
	case helperModeSleep:
		helperHookSleep()
	case helperModeMCPSvr:
		helperMCPStdio()
	case helperModeExtTool:
		helperExtTool()
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(3)
	}
}

func helperHookEcho() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	var in HookInput
	if err := json.Unmarshal(data, &in); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(5)
	}
	out, err := json.Marshal(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(6)
	}
	fmt.Println(string(out))
	os.Exit(0)
}

func helperHookBlock() {
	data, _ := io.ReadAll(os.Stdin)
	var in HookInput
	_ = json.Unmarshal(data, &in)
	fmt.Printf("blocked by policy: %s\n", in.Tool)
	fmt.Fprintln(os.Stderr, "hook policy denied")
	os.Exit(2)
}

func helperHookSleep() {
	ms, _ := strconv.Atoi(os.Getenv("HOOK_SLEEP_MS"))
	if ms <= 0 {
		ms = 5000
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	os.Exit(0)
}

// helperMCPStdio is a minimal newline-delimited JSON-RPC MCP server. Tool
// names are deliberately unsorted so tests can verify deterministic ordering.
func helperMCPStdio() {
	type request struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	enc := json.NewEncoder(os.Stdout)
	writeResult := func(id json.RawMessage, result any) {
		_ = enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  result,
		})
	}
	writeError := func(id json.RawMessage, code int, msg string) {
		_ = enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": code, "message": msg},
		})
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			writeResult(req.ID, map[string]any{
				"protocolVersion": mcpProtocol,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "helper", "version": "1.0.0"},
			})
		case "notifications/initialized":
			// notification: no response
		case "tools/list":
			writeResult(req.ID, map[string]any{
				"tools": []map[string]any{
					{"name": "zeta", "description": "last"},
					{"name": "alpha", "description": "first"},
					{"name": "middle", "description": "in between"},
				},
			})
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.Name == "fail" {
				writeResult(req.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": "boom"}},
					"isError": true,
				})
				continue
			}
			writeResult(req.ID, map[string]any{
				"content": []map[string]any{{
					"type": "text",
					"text": fmt.Sprintf("stdio:%s:%s", params.Name, string(params.Arguments)),
				}},
			})
		default:
			writeError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
	os.Exit(0)
}

func helperRunner(t *testing.T, hooks ...HookConfig) *Runner {
	t.Helper()
	return &Runner{Hooks: hooks, Env: []string{helperEnv + "=1"}}
}

func helperHook(mode string, matcher string, timeout time.Duration) HookConfig {
	return HookConfig{
		Event:   HookBeforeTool,
		Command: os.Args[0],
		Args:    []string{mode},
		Matcher: matcher,
		Timeout: timeout,
	}
}

// ---------------------------------------------------------------------------
// Hook tests
// ---------------------------------------------------------------------------

func TestMatchHook(t *testing.T) {
	t.Parallel()
	cases := []struct {
		matcher string
		target  string
		want    bool
	}{
		{"", "shell", true},
		{"*", "shell", true},
		{"shell", "shell", true},
		{"shell", "shells", false},
		{"sh*", "shell", true},
		{"*_exec", "shell_exec", true},
		{"*_exec", "shell_read", false},
		{"?", "a", true},
		{"[", "[", true}, // invalid pattern falls back to exact match
		{"[", "b", false},
	}
	for _, tc := range cases {
		if got := MatchHook(tc.matcher, tc.target); got != tc.want {
			t.Errorf("MatchHook(%q, %q) = %v, want %v", tc.matcher, tc.target, got, tc.want)
		}
	}
}

func TestHookRunnerPassesJSONAndFiltersMatcher(t *testing.T) {
	t.Parallel()
	runner := helperRunner(t,
		helperHook(helperModeHook, "shell*", 5*time.Second),
		helperHook(helperModeHook, "other*", 5*time.Second),
		HookConfig{Event: HookAfterTool, Command: os.Args[0], Args: []string{helperModeHook}},
	)

	in := HookInput{
		Event:     HookBeforeTool,
		Tool:      "shell_exec",
		SessionID: "sess-1",
		CWD:       "/tmp",
		Args:      json.RawMessage(`{"cmd":"ls -la"}`),
		Extra:     map[string]any{"risk": "low"},
	}
	results, err := runner.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (only the matching hook)", len(results))
	}
	res := results[0]
	if !res.Allow || res.ExitCode != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	var got HookInput
	if err := json.Unmarshal([]byte(res.Stdout), &got); err != nil {
		t.Fatalf("hook stdout is not the JSON payload: %v (%q)", err, res.Stdout)
	}
	if got.Event != in.Event || got.Tool != in.Tool || got.SessionID != in.SessionID || got.CWD != in.CWD {
		t.Fatalf("hook received %+v, want %+v", got, in)
	}
	if string(got.Args) != string(in.Args) {
		t.Fatalf("hook args = %s, want %s", got.Args, in.Args)
	}
	if got.Extra["risk"] != "low" {
		t.Fatalf("hook extra = %v", got.Extra)
	}
}

func TestHookBlocksOnNonZeroExit(t *testing.T) {
	t.Parallel()
	runner := helperRunner(t,
		helperHook(helperModeBlock, "*", 5*time.Second),
		helperHook(helperModeHook, "*", 5*time.Second),
	)
	results, err := runner.Run(context.Background(), HookInput{
		Event: HookBeforeTool,
		Tool:  "shell_exec",
		Args:  json.RawMessage(`{"cmd":"rm -rf /"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v (non-zero exits are policy results, not errors)", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (must stop after a blocking hook)", len(results))
	}
	res := results[0]
	if res.Allow {
		t.Fatal("blocking hook reported Allow=true")
	}
	if res.ExitCode != 2 {
		t.Fatalf("ExitCode = %d, want 2", res.ExitCode)
	}
	if !strings.Contains(res.Message, "blocked by policy") {
		t.Fatalf("Message = %q, want reason from stdout", res.Message)
	}
	if !strings.Contains(res.Stderr, "hook policy denied") {
		t.Fatalf("Stderr = %q, want captured stderr", res.Stderr)
	}
	if !Blocked(results) {
		t.Fatal("Blocked(results) = false")
	}

	merged, err := runner.RunBeforeTool(context.Background(), "shell_exec", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("RunBeforeTool: %v", err)
	}
	if merged.Allow {
		t.Fatal("RunBeforeTool Allow = true, want false")
	}
	if !strings.Contains(merged.Message, "blocked by policy") {
		t.Fatalf("RunBeforeTool Message = %q", merged.Message)
	}
}

func TestHookAllowsOnZeroExit(t *testing.T) {
	t.Parallel()
	runner := helperRunner(t, helperHook(helperModeHook, "read_*", 5*time.Second))
	merged, err := runner.RunBeforeTool(context.Background(), "read_file", json.RawMessage(`{"path":"x"}`))
	if err != nil {
		t.Fatalf("RunBeforeTool: %v", err)
	}
	if !merged.Allow {
		t.Fatalf("Allow = false, want true: %+v", merged)
	}
}

func TestHookTimeout(t *testing.T) {
	t.Parallel()
	before := runtime.NumGoroutine()

	runner := helperRunner(t, HookConfig{
		Event:   HookBeforeTool,
		Command: os.Args[0],
		Args:    []string{helperModeSleep},
		Timeout: 200 * time.Millisecond,
		// Keep the child asleep far beyond the timeout.
	})
	runner.Env = append(runner.Env, "HOOK_SLEEP_MS=10000")

	start := time.Now()
	results, err := runner.Run(context.Background(), HookInput{Event: HookBeforeTool, Tool: "shell"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout took %s; hook was not killed promptly", elapsed)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if !results[0].TimedOut || results[0].Allow {
		t.Fatalf("unexpected timeout result: %+v", results[0])
	}

	// Give any reaper goroutines a moment and ensure running timed-out hooks
	// does not leak goroutines.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines leaked: before=%d after=%d", before, after)
	}
}

func TestHookStartFailure(t *testing.T) {
	t.Parallel()
	runner := &Runner{Hooks: []HookConfig{{
		Event:   HookBeforeTool,
		Command: "/nonexistent/tern-hook-binary",
		Timeout: time.Second,
	}}}
	results, err := runner.Run(context.Background(), HookInput{Event: HookBeforeTool, Tool: "shell"})
	if err == nil {
		t.Fatal("expected an error for a missing hook command")
	}
	if len(results) != 1 || results[0].Allow || results[0].ExitCode != -1 {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestHookConfigValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  HookConfig
		ok   bool
	}{
		{"valid", HookConfig{Event: HookBeforeTool, Command: "true"}, true},
		{"missing event", HookConfig{Command: "true"}, false},
		{"unknown event", HookConfig{Event: "nope", Command: "true"}, false},
		{"missing command", HookConfig{Event: HookSessionStart}, false},
		{"bad matcher", HookConfig{Event: HookBeforeTool, Command: "true", Matcher: "["}, false},
	}
	for _, tc := range cases {
		err := tc.cfg.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
	if d := (HookConfig{Event: HookBeforeTool, Command: "true"}).WithDefaults().Timeout; d != DefaultHookTimeout {
		t.Errorf("default timeout = %s, want %s", d, DefaultHookTimeout)
	}
}

func TestHookRunnerNoMatchingHooks(t *testing.T) {
	t.Parallel()
	runner := helperRunner(t, helperHook(helperModeHook, "never", time.Second))
	results, err := runner.Run(context.Background(), HookInput{Event: HookBeforeTool, Tool: "shell"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
	if len(runner.Matching(HookBeforeTool, "never")) != 1 {
		t.Fatal("Matching did not find the hook")
	}
}

// ---------------------------------------------------------------------------
// HTTP MCP tests
// ---------------------------------------------------------------------------

type httpRPCServer struct {
	t            *testing.T
	tools        []string
	notified     atomic.Int64
	lastAuth     atomic.Value // string
	sseToolCalls bool
}

func (h *httpRPCServer) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.t.Errorf("method = %s, want POST", r.Method)
	}
	if got := r.Header.Get("X-Api-Key"); got != "" {
		h.lastAuth.Store(got)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.t.Errorf("read body: %v", err)
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.JSONRPC != "2.0" {
		h.t.Errorf("jsonrpc = %q", req.JSONRPC)
	}
	writeJSON := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
	}
	switch req.Method {
	case "initialize":
		writeJSON(map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "httptest-mcp", "version": "9.9.9"},
		})
	case "notifications/initialized":
		h.notified.Add(1)
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		tools := make([]map[string]any, 0, len(h.tools))
		for _, name := range h.tools {
			tools = append(tools, map[string]any{
				"name":        name,
				"description": "tool " + name,
				"inputSchema": map[string]any{"type": "object"},
			})
		}
		writeJSON(map[string]any{"tools": tools})
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &params)
		result := map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": fmt.Sprintf("http:%s:%s", params.Name, string(params.Arguments)),
			}},
		}
		if params.Name == "fail" {
			result["isError"] = true
		}
		if !h.sseToolCalls {
			writeJSON(result)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// A notification event must be skipped in favor of the response.
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		payload, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
		if err != nil {
			h.t.Errorf("marshal sse: %v", err)
			return
		}
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error":   map[string]any{"code": -32601, "message": "method not found"},
		})
	}
}

func newHTTPTestServer(t *testing.T, sse bool, tools []string) (*httptest.Server, *httpRPCServer) {
	t.Helper()
	h := &httpRPCServer{t: t, tools: tools, sseToolCalls: sse}
	srv := httptest.NewServer(http.HandlerFunc(h.handler))
	t.Cleanup(srv.Close)
	return srv, h
}

func TestMCPHTTPClient(t *testing.T) {
	t.Parallel()
	srv, h := newHTTPTestServer(t, true, []string{"gamma", "alpha", "beta"})

	client, err := NewClient(ServerConfig{
		Name:      "remote",
		Transport: TransportHTTP,
		URL:       srv.URL,
		Headers:   map[string]string{"X-Api-Key": "secret"},
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	ctx := context.Background()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !client.Initialized() {
		t.Fatal("Initialized() = false after Initialize")
	}
	info, ok := client.ServerInfo()
	if !ok || info.Name != "httptest-mcp" || info.Version != "9.9.9" {
		t.Fatalf("ServerInfo = %+v, %v", info, ok)
	}
	if got := h.notified.Load(); got != 1 {
		t.Fatalf("initialized notifications = %d, want 1", got)
	}
	if got, _ := h.lastAuth.Load().(string); got != "secret" {
		t.Fatalf("X-Api-Key = %q, want secret", got)
	}
	// Initialize is idempotent.
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("second Initialize: %v", err)
	}
	if got := h.notified.Load(); got != 1 {
		t.Fatalf("initialized notifications after second call = %d, want 1", got)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	if got, want := strings.Join(names, ","), "alpha,beta,gamma"; got != want {
		t.Fatalf("tool order = %q, want %q", got, want)
	}
	if tools[0].Description != "tool alpha" {
		t.Fatalf("description = %q", tools[0].Description)
	}

	out, err := client.CallTool(ctx, "alpha", json.RawMessage(`{"n":1}`)) // SSE response
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if want := `http:alpha:{"n":1}`; out != want {
		t.Fatalf("CallTool output = %q, want %q", out, want)
	}

	if _, err := client.CallTool(ctx, "fail", nil); err == nil {
		t.Fatal("expected an error for isError=true tool result")
	}

	if _, err := client.CallTool(ctx, "bad", json.RawMessage(`not-json`)); err == nil {
		t.Fatal("expected an error for invalid JSON arguments")
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := client.ListTools(ctx); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("ListTools after Close error = %v, want ErrClientClosed", err)
	}
}

func TestMCPHTTPClientRPCError(t *testing.T) {
	t.Parallel()
	srv, _ := newHTTPTestServer(t, false, nil)
	client, err := NewClient(ServerConfig{Name: "remote", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	err = client.call(context.Background(), "no/such/method", map[string]any{}, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want *RPCError", err)
	}
	if rpcErr.Code != -32601 {
		t.Fatalf("code = %d, want -32601", rpcErr.Code)
	}
}

func TestMCPHTTPStatusError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	client, err := NewClient(ServerConfig{Name: "remote", Transport: "http", URL: srv.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()
	_, err = client.ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want HTTP 401", err)
	}
}

// ---------------------------------------------------------------------------
// stdio MCP tests
// ---------------------------------------------------------------------------

func stdioClient(t *testing.T, name string) *Client {
	t.Helper()
	client, err := NewClient(ServerConfig{
		Name:      name,
		Transport: TransportStdio,
		Command:   os.Args[0],
		Env:       []string{helperEnv + "=1", "GO_HELPER_MODE=" + helperModeMCPSvr},
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func TestMCPStdioClient(t *testing.T) {
	t.Parallel()
	client := stdioClient(t, "local")
	ctx := context.Background()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	info, ok := client.ServerInfo()
	if !ok || info.Name != "helper" {
		t.Fatalf("ServerInfo = %+v, %v", info, ok)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	if got, want := strings.Join(names, ","), "alpha,middle,zeta"; got != want {
		t.Fatalf("tool order = %q, want %q", got, want)
	}

	out, err := client.CallTool(ctx, "middle", json.RawMessage(`{"x":2}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if want := `stdio:middle:{"x":2}`; out != want {
		t.Fatalf("CallTool output = %q, want %q", out, want)
	}

	if _, err := client.CallTool(ctx, "fail", nil); err == nil {
		t.Fatal("expected an error for isError=true stdio result")
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestMCPStdioContextCancel(t *testing.T) {
	t.Parallel()
	client := stdioClient(t, "local")
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ListTools(ctx); err == nil {
		t.Fatal("expected an error for a canceled context")
	}
}

func TestMCPStdioStartFailure(t *testing.T) {
	t.Parallel()
	client, err := NewClient(ServerConfig{
		Name:      "broken",
		Transport: TransportStdio,
		Command:   "/nonexistent/tern-mcp-server",
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()
	if err := client.Initialize(context.Background()); err == nil {
		t.Fatal("expected an error starting a missing command")
	}
}

// ---------------------------------------------------------------------------
// Manager tests
// ---------------------------------------------------------------------------

func TestManagerConnectToolsStatus(t *testing.T) {
	t.Parallel()
	alphaSrv, _ := newHTTPTestServer(t, false, []string{"zulu", "alpha"})
	betaSrv, hb := newHTTPTestServer(t, true, []string{"beta_tool"})

	// Server names contain underscores to exercise the longest-prefix parse.
	manager := NewManager(
		ServerConfig{Name: "beta", Transport: TransportHTTP, URL: betaSrv.URL, Timeout: 5 * time.Second},
		ServerConfig{Name: "alpha_srv", Transport: TransportHTTP, URL: alphaSrv.URL, Timeout: 5 * time.Second},
	)
	if err := manager.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := hb.notified.Load(); got != 1 {
		t.Fatalf("beta initialized notifications = %d, want 1", got)
	}

	status := manager.Status()
	if len(status) != 2 {
		t.Fatalf("Status returned %d servers, want 2", len(status))
	}
	if status[0].Name != "alpha_srv" || status[1].Name != "beta" {
		t.Fatalf("Status order = %q, %q", status[0].Name, status[1].Name)
	}
	for _, st := range status {
		if !st.Connected || st.Error != "" {
			t.Fatalf("server %q status = %+v", st.Name, st)
		}
	}
	if status[0].ToolCount != 2 || status[1].ToolCount != 1 {
		t.Fatalf("tool counts = %d, %d, want 2, 1", status[0].ToolCount, status[1].ToolCount)
	}

	tools, err := manager.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
		if tool.Server == "" {
			t.Fatalf("tool %q has no server", tool.Name)
		}
	}
	want := "mcp_alpha_srv_alpha,mcp_alpha_srv_zulu,mcp_beta_beta_tool"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("namespaced tools = %q, want %q", got, want)
	}

	out, err := manager.CallTool(context.Background(), "mcp_alpha_srv_zulu", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if want := `http:zulu:{"q":"x"}`; out != want {
		t.Fatalf("CallTool output = %q, want %q", out, want)
	}

	out, err = manager.CallTool(context.Background(), "mcp_beta_beta_tool", nil)
	if err != nil {
		t.Fatalf("CallTool beta: %v", err)
	}
	if !strings.HasPrefix(out, "http:beta_tool:") {
		t.Fatalf("CallTool beta output = %q", out)
	}

	if _, err := manager.CallTool(context.Background(), "alpha", nil); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("un-namespaced tool error = %v, want ErrUnknownTool", err)
	}
	if _, err := manager.CallTool(context.Background(), "mcp_unknown_tool", nil); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("unknown server error = %v, want ErrUnknownTool", err)
	}

	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestManagerPartialFailure(t *testing.T) {
	t.Parallel()
	okSrv, _ := newHTTPTestServer(t, false, []string{"good"})
	manager := NewManager(
		ServerConfig{Name: "broken", Transport: TransportStdio, Command: "/nonexistent/tern-mcp-server", Timeout: time.Second},
		ServerConfig{Name: "ok", Transport: TransportHTTP, URL: okSrv.URL, Timeout: 5 * time.Second},
	)
	defer manager.Close()

	err := manager.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect should report the broken server")
	}
	status := manager.Status()
	if len(status) != 2 {
		t.Fatalf("Status returned %d servers, want 2", len(status))
	}
	var broken, healthy *MCPStatus
	for i := range status {
		switch status[i].Name {
		case "broken":
			broken = &status[i]
		case "ok":
			healthy = &status[i]
		}
	}
	if broken == nil || broken.Connected || broken.Error == "" {
		t.Fatalf("broken status = %+v", broken)
	}
	if healthy == nil || !healthy.Connected || healthy.ToolCount != 1 {
		t.Fatalf("healthy status = %+v", healthy)
	}

	tools, err := manager.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "mcp_ok_good" {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestManagerNoServers(t *testing.T) {
	t.Parallel()
	manager := NewManager()
	if err := manager.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	tools, err := manager.Tools(context.Background())
	if err != nil || len(tools) != 0 {
		t.Fatalf("Tools = %+v, %v", tools, err)
	}
	if status := manager.Status(); len(status) != 0 {
		t.Fatalf("Status = %+v", status)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNewClientValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  ServerConfig
	}{
		{"no name", ServerConfig{Transport: TransportStdio, Command: "x"}},
		{"stdio without command", ServerConfig{Name: "a", Transport: TransportStdio}},
		{"http without url", ServerConfig{Name: "a", Transport: TransportHTTP}},
		{"unknown transport", ServerConfig{Name: "a", Transport: "carrier-pigeon"}},
	}
	for _, tc := range cases {
		if _, err := NewClient(tc.cfg); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}

	// Transport is inferred when omitted.
	cfg, err := NewClient(ServerConfig{Name: "a", URL: "http://localhost:1"})
	if err != nil {
		t.Fatalf("infer http: %v", err)
	}
	if cfg.Config().Transport != TransportHTTP || cfg.Config().Timeout != defaultMCPTimeout {
		t.Fatalf("normalized config = %+v", cfg.Config())
	}
	_ = cfg.Close()
}

func TestNamespacedTool(t *testing.T) {
	t.Parallel()
	if got, want := NamespacedTool("srv", "tool"), "mcp_srv_tool"; got != want {
		t.Fatalf("NamespacedTool = %q, want %q", got, want)
	}
}

func TestParseRPCResponseBatch(t *testing.T) {
	t.Parallel()
	body := []byte(`[{"jsonrpc":"2.0","method":"notifications/message"},{"jsonrpc":"2.0","id":7,"result":{"ok":true}},{"jsonrpc":"2.0","id":8,"result":{"ok":false}}]`)
	resp, err := parseRPCResponse(body, "7")
	if err != nil {
		t.Fatalf("parseRPCResponse: %v", err)
	}
	if resp == nil || strings.TrimSpace(string(resp.ID)) != "7" {
		t.Fatalf("resp = %+v", resp)
	}
	if string(resp.Result) != `{"ok":true}` {
		t.Fatalf("result = %s", resp.Result)
	}
	if resp, err := parseRPCResponse(body, "9"); err != nil || resp != nil {
		t.Fatalf("unexpected match: %+v, %v", resp, err)
	}
}

func TestRequestIDString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   any
		want string
	}{
		{int64(3), "3"},
		{4, "4"},
		{json.Number("5"), "5"},
		{"abc", "abc"},
		{json.RawMessage(`6`), "6"},
		{nil, ""},
	}
	for _, tc := range cases {
		got, err := requestIDString(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("requestIDString(%v) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}
