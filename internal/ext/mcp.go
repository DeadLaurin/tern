package ext

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// JSON-RPC 2.0 and MCP protocol constants.
const (
	jsonRPCVersion    = "2.0"
	mcpProtocol       = "2024-11-05"
	mcpClientName     = "tern"
	mcpClientVersion  = "1.0.0"
	defaultMCPTimeout = 30 * time.Second
)

// Transport names accepted by ServerConfig.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// ToolNamePrefix is the namespace prefix used by Manager for MCP tools.
const ToolNamePrefix = "mcp_"

// Errors returned by the MCP client.
var (
	// ErrUnknownTool is returned when a namespaced tool cannot be resolved.
	ErrUnknownTool = errors.New("ext: unknown mcp tool")
	// ErrClientClosed is returned when a client is used after Close.
	ErrClientClosed = errors.New("ext: mcp client closed")
	// ErrTransport is returned for unsupported or misconfigured transports.
	ErrTransport = errors.New("ext: mcp transport")
)

// ServerConfig describes one MCP server.
type ServerConfig struct {
	// Name identifies the server; it is used for tool namespacing.
	Name string
	// Transport is "stdio" or "http". Empty is inferred from Command/URL.
	Transport string
	// Command, Args and Env configure a stdio server subprocess.
	Command string
	Args    []string
	Env     []string
	// URL and Headers configure an HTTP server.
	URL     string
	Headers map[string]string
	// Timeout bounds each request. Zero means 30s.
	Timeout time.Duration
}

// normalized returns a copy with defaults applied and the transport resolved.
func (c ServerConfig) normalized() ServerConfig {
	if c.Transport == "" {
		if c.URL != "" {
			c.Transport = TransportHTTP
		} else {
			c.Transport = TransportStdio
		}
	}
	c.Transport = strings.ToLower(strings.TrimSpace(c.Transport))
	if c.Timeout <= 0 {
		c.Timeout = defaultMCPTimeout
	}
	return c
}

func (c ServerConfig) validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("%w: server name is required", ErrTransport)
	}
	switch c.Transport {
	case TransportStdio:
		if strings.TrimSpace(c.Command) == "" {
			return fmt.Errorf("%w: stdio server %q requires a command", ErrTransport, c.Name)
		}
	case TransportHTTP:
		if strings.TrimSpace(c.URL) == "" {
			return fmt.Errorf("%w: http server %q requires a URL", ErrTransport, c.Name)
		}
	default:
		return fmt.Errorf("%w: server %q has unsupported transport %q", ErrTransport, c.Name, c.Transport)
	}
	return nil
}

// MCPTool is a tool advertised by an MCP server.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	// Server names the owning server. Manager fills it in; direct Client
	// calls leave it empty.
	Server string `json:"-"`
}

// ServerInfo identifies an MCP server implementation.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult is the MCP initialize handshake result.
type InitializeResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities,omitempty"`
	ServerInfo      ServerInfo      `json:"serverInfo"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("mcp: rpc error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("mcp: rpc error %d: %s", e.Code, e.Message)
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// transport moves JSON-RPC messages to and from a server.
type transport interface {
	Call(ctx context.Context, req *rpcRequest) (*rpcResponse, error)
	Notify(ctx context.Context, req *rpcRequest) error
	Close() error
}

// Client is a JSON-RPC 2.0 MCP client bound to one server.
type Client struct {
	cfg ServerConfig

	mu       sync.Mutex
	tr       transport
	nextID   int64
	closed   bool
	info     *InitializeResult
	initDone bool
	initMu   sync.Mutex
}

// NewClient validates cfg and returns a client. The underlying transport is
// created lazily on first use, so no subprocess is spawned here.
func NewClient(cfg ServerConfig) (*Client, error) {
	cfg = cfg.normalized()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Client{cfg: cfg}, nil
}

// Config returns the client's normalized server configuration.
func (c *Client) Config() ServerConfig { return c.cfg }

// Name returns the configured server name.
func (c *Client) Name() string { return c.cfg.Name }

// ServerInfo returns the server identity learned during Initialize, if any.
func (c *Client) ServerInfo() (ServerInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info == nil {
		return ServerInfo{}, false
	}
	return c.info.ServerInfo, true
}

// Initialized reports whether the handshake has completed.
func (c *Client) Initialized() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initDone
}

// Initialize performs the MCP handshake: the initialize request followed by
// the notifications/initialized notification. It is safe to call repeatedly.
func (c *Client) Initialize(ctx context.Context) error {
	c.initMu.Lock()
	defer c.initMu.Unlock()
	if c.Initialized() {
		return nil
	}
	params := map[string]any{
		"protocolVersion": mcpProtocol,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name":    mcpClientName,
			"version": mcpClientVersion,
		},
	}
	var res InitializeResult
	if err := c.call(ctx, "initialize", params, &res); err != nil {
		return fmt.Errorf("ext: mcp %q initialize: %w", c.cfg.Name, err)
	}
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("ext: mcp %q initialized notification: %w", c.cfg.Name, err)
	}
	c.mu.Lock()
	c.info = &res
	c.initDone = true
	c.mu.Unlock()
	return nil
}

// ListTools fetches the server's tools, sorted deterministically by name so
// upstream prompt prefixes and caches stay stable.
func (c *Client) ListTools(ctx context.Context) ([]MCPTool, error) {
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}
	var res struct {
		Tools      []MCPTool `json:"tools"`
		NextCursor string    `json:"nextCursor,omitempty"`
	}
	if err := c.call(ctx, "tools/list", map[string]any{}, &res); err != nil {
		return nil, fmt.Errorf("ext: mcp %q tools/list: %w", c.cfg.Name, err)
	}
	tools := append([]MCPTool(nil), res.Tools...)
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

// CallTool invokes a tool and returns its textual output.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%w: empty tool name", ErrUnknownTool)
	}
	if err := c.Initialize(ctx); err != nil {
		return "", err
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		return "", fmt.Errorf("ext: mcp %q tools/call: arguments are not valid JSON", c.cfg.Name)
	}
	params := map[string]any{
		"name":      name,
		"arguments": args,
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
		IsError           bool            `json:"isError,omitempty"`
	}
	if err := c.call(ctx, "tools/call", params, &res); err != nil {
		return "", fmt.Errorf("ext: mcp %q tools/call %q: %w", c.cfg.Name, name, err)
	}
	var parts []string
	for _, part := range res.Content {
		if part.Type == "text" || part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	out := strings.Join(parts, "\n")
	if out == "" && len(res.StructuredContent) > 0 {
		out = string(res.StructuredContent)
	}
	if res.IsError {
		if out == "" {
			out = "tool reported an error"
		}
		return out, fmt.Errorf("ext: mcp %q tool %q failed: %s", c.cfg.Name, name, out)
	}
	return out, nil
}

// Close releases the transport (and terminates a stdio subprocess). It is
// idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	tr := c.tr
	c.tr = nil
	c.mu.Unlock()
	if tr == nil {
		return nil
	}
	return tr.Close()
}

func (c *Client) ensureTransport() (transport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClientClosed
	}
	if c.tr != nil {
		return c.tr, nil
	}
	var (
		tr  transport
		err error
	)
	switch c.cfg.Transport {
	case TransportStdio:
		tr, err = newStdioTransport(c.cfg)
	case TransportHTTP:
		tr, err = newHTTPTransport(c.cfg)
	default:
		err = fmt.Errorf("%w: unsupported transport %q", ErrTransport, c.cfg.Transport)
	}
	if err != nil {
		return nil, err
	}
	c.tr = tr
	return tr, nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	tr, err := c.ensureTransport()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	req := &rpcRequest{JSONRPC: jsonRPCVersion, ID: id, Method: method, Params: params}
	callCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	resp, err := tr.Call(callCtx, req)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("ext: mcp %q: empty response to %s", c.cfg.Name, method)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out == nil {
		return nil
	}
	if len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("ext: mcp %q: decode %s result: %w", c.cfg.Name, method, err)
	}
	return nil
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	tr, err := c.ensureTransport()
	if err != nil {
		return err
	}
	req := &rpcRequest{JSONRPC: jsonRPCVersion, Method: method, Params: params}
	notifyCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	return tr.Notify(notifyCtx, req)
}

// ---------------------------------------------------------------------------
// stdio transport
// ---------------------------------------------------------------------------

type stdioTransport struct {
	cfg    ServerConfig
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *lockedBuffer

	writeMu   sync.Mutex
	callMu    sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

func newStdioTransport(cfg ServerConfig) (transport, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = append(os.Environ(), cfg.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ext: mcp %q: stdin pipe: %w", cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ext: mcp %q: stdout pipe: %w", cfg.Name, err)
	}
	stderr := &lockedBuffer{max: maxHookOutput}
	cmd.Stderr = stderr
	// Bound Wait if the child cannot be reaped promptly.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ext: mcp %q: start %q: %w", cfg.Name, cfg.Command, err)
	}
	return &stdioTransport{
		cfg:    cfg,
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 256<<10),
		stderr: stderr,
	}, nil
}

func (t *stdioTransport) Call(ctx context.Context, req *rpcRequest) (*rpcResponse, error) {
	t.callMu.Lock()
	defer t.callMu.Unlock()

	id, err := requestIDString(req.ID)
	if err != nil {
		return nil, err
	}
	if err := t.write(req); err != nil {
		return nil, err
	}

	type readResult struct {
		resp *rpcResponse
		err  error
	}
	done := make(chan readResult, 1)
	go func() {
		resp, err := t.readResponse(id)
		done <- readResult{resp: resp, err: err}
	}()

	select {
	case r := <-done:
		return r.resp, r.err
	case <-ctx.Done():
		// Unblock the reader by terminating the subprocess; the goroutine
		// then sees EOF and exits, so nothing leaks.
		t.kill()
		<-done
		return nil, ctx.Err()
	}
}

func (t *stdioTransport) Notify(_ context.Context, req *rpcRequest) error {
	return t.write(req)
}

func (t *stdioTransport) write(req *rpcRequest) error {
	if req == nil {
		return errors.New("ext: mcp: nil request")
	}
	line, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("ext: mcp %q: encode request: %w", t.cfg.Name, err)
	}
	line = append(line, '\n')
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.stdin.Write(line); err != nil {
		return fmt.Errorf("ext: mcp %q: write request: %w%s", t.cfg.Name, err, t.stderrSuffix())
	}
	return nil
}

func (t *stdioTransport) readResponse(id string) (*rpcResponse, error) {
	for {
		line, err := t.stdout.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			resp, perr := t.parseAndHandle(bytes.TrimSpace(line), id)
			if perr != nil {
				return nil, perr
			}
			if resp != nil {
				return resp, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("ext: mcp %q: server closed the connection%s", t.cfg.Name, t.stderrSuffix())
			}
			return nil, fmt.Errorf("ext: mcp %q: read response: %w%s", t.cfg.Name, err, t.stderrSuffix())
		}
	}
}

// parseRPCResponse decodes one line of output. For server-initiated requests
// it writes a method-not-found reply and returns (nil, nil) so the caller keeps
// reading. Responses to other ids and notifications are skipped.
func (t *stdioTransport) parseAndHandle(line []byte, wantID string) (*rpcResponse, error) {
	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		// Ignore unparsable log lines some servers emit on stdout.
		return nil, nil
	}
	if resp.Method != "" && len(resp.ID) > 0 {
		reply := map[string]any{
			"jsonrpc": jsonRPCVersion,
			"id":      json.RawMessage(resp.ID),
			"error": map[string]any{
				"code":    -32601,
				"message": "method not found",
			},
		}
		if data, err := json.Marshal(reply); err == nil {
			t.writeMu.Lock()
			_, _ = t.stdin.Write(append(data, '\n'))
			t.writeMu.Unlock()
		}
		return nil, nil
	}
	if got, err := responseIDString(resp.ID); err != nil || got != wantID {
		return nil, nil
	}
	return &resp, nil
}

func (t *stdioTransport) kill() {
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
}

func (t *stdioTransport) Close() error {
	t.closeOnce.Do(func() {
		_ = t.stdin.Close()
		done := make(chan error, 1)
		go func() { done <- t.cmd.Wait() }()
		select {
		case t.closeErr = <-done:
		case <-time.After(2 * time.Second):
			t.kill()
			t.closeErr = <-done
		}
		// A killed process surfaces as an ExitError; that is expected.
		var exitErr *exec.ExitError
		if errors.As(t.closeErr, &exitErr) {
			t.closeErr = nil
		}
	})
	return t.closeErr
}

func (t *stdioTransport) stderrSuffix() string {
	if s := strings.TrimSpace(t.stderr.String()); s != "" {
		return ": " + s
	}
	return ""
}

// ---------------------------------------------------------------------------
// HTTP transport
// ---------------------------------------------------------------------------

type httpTransport struct {
	cfg    ServerConfig
	client *http.Client
}

func newHTTPTransport(cfg ServerConfig) (transport, error) {
	return &httpTransport{
		cfg: cfg,
		client: &http.Client{
			// Request deadlines are enforced via context.
			Timeout: 0,
		},
	}, nil
}

func (t *httpTransport) Call(ctx context.Context, req *rpcRequest) (*rpcResponse, error) {
	id, err := requestIDString(req.ID)
	if err != nil {
		return nil, err
	}
	body, err := t.post(ctx, req)
	if err != nil {
		return nil, err
	}
	resp, err := parseRPCResponse(body, id)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("ext: mcp %q: no JSON-RPC response for id %s", t.cfg.Name, id)
	}
	return resp, nil
}

func (t *httpTransport) Notify(ctx context.Context, req *rpcRequest) error {
	_, err := t.post(ctx, req)
	return err
}

func (t *httpTransport) Close() error {
	t.client.CloseIdleConnections()
	return nil
}

// post sends req and returns the raw JSON body of the first matching
// response. SSE responses are parsed event by event.
func (t *httpTransport) post(ctx context.Context, req *rpcRequest) ([]byte, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("ext: mcp %q: encode request: %w", t.cfg.Name, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("ext: mcp %q: build request: %w", t.cfg.Name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.cfg.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ext: mcp %q: post %s: %w", t.cfg.Name, t.cfg.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("ext: mcp %q: HTTP %d from %s: %s",
			t.cfg.Name, resp.StatusCode, t.cfg.URL, strings.TrimSpace(string(snippet)))
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		return t.readSSE(resp.Body)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("ext: mcp %q: read response: %w", t.cfg.Name, err)
	}
	return body, nil
}

// readSSE returns the data payload of the first "message" event that contains
// a JSON-RPC response. Notifications are skipped until the stream ends.
func (t *httpTransport) readSSE(body io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var data []string
	flush := func() []byte {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		if !json.Valid([]byte(payload)) {
			return nil
		}
		// Prefer responses over notifications.
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal([]byte(payload), &probe); err != nil || len(probe.ID) == 0 {
			return nil
		}
		return []byte(payload)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			if payload := flush(); payload != nil {
				return payload, nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// event:, id:, retry:, comments: ignored.
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("ext: mcp %q: read SSE stream: %w", t.cfg.Name, err)
	}
	if payload := flush(); payload != nil {
		return payload, nil
	}
	return nil, fmt.Errorf("ext: mcp %q: SSE stream ended without a JSON-RPC response", t.cfg.Name)
}

// ---------------------------------------------------------------------------
// JSON-RPC helpers
// ---------------------------------------------------------------------------

func requestIDString(id any) (string, error) {
	switch v := id.(type) {
	case nil:
		return "", nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case json.Number:
		return v.String(), nil
	case string:
		return v, nil
	case json.RawMessage:
		return responseIDString(v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("ext: mcp: encode request id: %w", err)
		}
		return responseIDString(data)
	}
}

func responseIDString(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	return trimmed, nil
}

// parseRPCResponse decodes a JSON-RPC response body (single object or batch)
// and returns the matching response. Notifications and other ids are skipped.
func parseRPCResponse(body []byte, wantID string) (*rpcResponse, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var candidates []json.RawMessage
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(trimmed, &candidates); err != nil {
			return nil, fmt.Errorf("ext: mcp: decode batch response: %w", err)
		}
	default:
		candidates = []json.RawMessage{trimmed}
	}
	for _, raw := range candidates {
		var resp rpcResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("ext: mcp: decode response: %w", err)
		}
		if resp.Method != "" && len(resp.ID) == 0 {
			continue // server notification
		}
		got, err := responseIDString(resp.ID)
		if err != nil {
			return nil, err
		}
		if wantID != "" && got != wantID {
			continue
		}
		if wantID == "" && got == "" {
			continue
		}
		respCopy := resp
		return &respCopy, nil
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

// MCPStatus describes the state of one configured MCP server for /mcp.
type MCPStatus struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Connected bool     `json:"connected"`
	ToolCount int      `json:"tool_count"`
	Tools     []string `json:"tools,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// Manager owns multiple MCP servers and exposes their tools under stable,
// deterministic names.
type Manager struct {
	mu      sync.RWMutex
	order   []string
	cfgs    map[string]ServerConfig
	clients map[string]*Client
	status  map[string]*MCPStatus
}

// NewManager returns a Manager for the given server configs. Configs are
// retained in the order provided; tools are always exported sorted.
func NewManager(cfgs ...ServerConfig) *Manager {
	m := &Manager{
		cfgs:    make(map[string]ServerConfig, len(cfgs)),
		clients: make(map[string]*Client, len(cfgs)),
		status:  make(map[string]*MCPStatus, len(cfgs)),
	}
	for _, cfg := range cfgs {
		m.Add(cfg)
	}
	return m
}

// Add registers (or replaces) a server config. Replacing a connected server
// closes the previous client.
func (m *Manager) Add(cfg ServerConfig) {
	cfg = cfg.normalized()
	name := cfg.Name
	m.mu.Lock()
	old, existed := m.clients[name]
	if existed {
		delete(m.clients, name)
	} else {
		m.order = append(m.order, name)
	}
	m.cfgs[name] = cfg
	m.status[name] = &MCPStatus{Name: name, Transport: cfg.Transport}
	m.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// NamespacedTool returns the stable public name for a tool on a server.
func NamespacedTool(server, tool string) string {
	return ToolNamePrefix + server + "_" + tool
}

// Client returns the client for a server that has been connected.
func (m *Manager) Client(server string) (*Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clients[server]
	return c, ok
}

// Connect initializes every configured server and caches its tool list.
// Failures are recorded in Status and joined into the returned error so that
// one broken server does not prevent the others from working.
func (m *Manager) Connect(ctx context.Context) error {
	m.mu.RLock()
	order := append([]string(nil), m.order...)
	cfgs := make([]ServerConfig, 0, len(order))
	for _, name := range order {
		cfgs = append(cfgs, m.cfgs[name])
	}
	m.mu.RUnlock()

	var errs []error
	for _, cfg := range cfgs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		name := cfg.Name

		m.mu.Lock()
		client := m.clients[name]
		m.mu.Unlock()
		if client == nil {
			var err error
			client, err = NewClient(cfg)
			if err != nil {
				m.setStatus(name, MCPStatus{Name: name, Transport: cfg.Transport, Error: err.Error()})
				errs = append(errs, err)
				continue
			}
			m.mu.Lock()
			m.clients[name] = client
			m.mu.Unlock()
		}

		if err := client.Initialize(ctx); err != nil {
			m.setStatus(name, MCPStatus{Name: name, Transport: cfg.Transport, Error: err.Error()})
			errs = append(errs, err)
			continue
		}
		tools, err := client.ListTools(ctx)
		status := MCPStatus{Name: name, Transport: cfg.Transport, Connected: true}
		if err != nil {
			status.Error = err.Error()
			errs = append(errs, err)
		}
		status.ToolCount = len(tools)
		status.Tools = toolNames(tools)
		m.setStatus(name, status)
	}
	return errors.Join(errs...)
}

// Tools returns all tools from all servers, namespaced as
// mcp_<server>_<tool> and sorted by name.
func (m *Manager) Tools(ctx context.Context) ([]MCPTool, error) {
	m.mu.RLock()
	names := append([]string(nil), m.order...)
	sort.Strings(names)
	clients := make(map[string]*Client, len(names))
	skip := make(map[string]bool, len(names))
	for _, name := range names {
		clients[name] = m.clients[name]
		// Servers that failed to connect are reported via Status; do not
		// retry them (and re-spawn subprocesses) on every Tools call.
		if st := m.status[name]; st != nil && !st.Connected && st.Error != "" {
			skip[name] = true
		}
	}
	m.mu.RUnlock()

	var (
		out  []MCPTool
		errs []error
	)
	for _, name := range names {
		client := clients[name]
		if client == nil || skip[name] {
			continue
		}
		tools, err := client.ListTools(ctx)
		if err != nil {
			errs = append(errs, err)
			m.setStatusError(name, err)
			continue
		}
		m.setStatusTools(name, tools)
		for _, tool := range tools {
			tool.Server = name
			tool.Name = NamespacedTool(name, tool.Name)
			out = append(out, tool)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Server < out[j].Server
	})
	return out, errors.Join(errs...)
}

// CallTool invokes a namespaced tool. The longest matching server name wins,
// so server names may contain underscores.
func (m *Manager) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if !strings.HasPrefix(name, ToolNamePrefix) {
		return "", fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	m.mu.RLock()
	names := append([]string(nil), m.order...)
	clients := make(map[string]*Client, len(names))
	for _, n := range names {
		clients[n] = m.clients[n]
	}
	m.mu.RUnlock()

	best := ""
	for _, server := range names {
		prefix := ToolNamePrefix + server + "_"
		if strings.HasPrefix(name, prefix) && len(server) > len(best) {
			best = server
		}
	}
	if best == "" || clients[best] == nil {
		return "", fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	tool := strings.TrimPrefix(name, ToolNamePrefix+best+"_")
	if tool == "" {
		return "", fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	return clients[best].CallTool(ctx, tool, args)
}

// Status returns a snapshot of every configured server, sorted by name.
func (m *Manager) Status() []MCPStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]MCPStatus, 0, len(m.status))
	for _, st := range m.status {
		cp := *st
		cp.Tools = append([]string(nil), st.Tools...)
		out = append(out, cp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Close shuts down all clients, joining any teardown errors.
func (m *Manager) Close() error {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.clients = make(map[string]*Client)
	m.mu.Unlock()

	var errs []error
	for _, c := range clients {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) setStatus(name string, st MCPStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := st
	m.status[name] = &cp
}

func (m *Manager) setStatusError(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status[name]
	if st == nil {
		st = &MCPStatus{Name: name}
		m.status[name] = st
	}
	st.Error = err.Error()
}

func (m *Manager) setStatusTools(name string, tools []MCPTool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status[name]
	if st == nil {
		st = &MCPStatus{Name: name}
		m.status[name] = st
	}
	st.Connected = true
	st.Error = ""
	st.ToolCount = len(tools)
	st.Tools = toolNames(tools)
}

func toolNames(tools []MCPTool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}
