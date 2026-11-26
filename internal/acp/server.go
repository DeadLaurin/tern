package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
)

// AvailableModels lists the models tern exposes to ACP hosts.
var AvailableModels = []modelInfo{
	{ModelID: "deepseek-flash", Name: "DeepSeek Flash", Description: "Fast DeepSeek model with vision support"},
	{ModelID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", Description: "Higher-reasoning DeepSeek model"},
}

// AvailableModes lists the permission modes tern exposes to ACP hosts.
var AvailableModes = []modeInfo{
	{ID: string(permission.ModeAsk), Name: "Ask", Description: "Ask before mutating tool calls"},
	{ID: string(permission.ModeAutoEdit), Name: "Auto Edit", Description: "Apply workspace edits automatically; ask for bash"},
	{ID: string(permission.ModePlan), Name: "Plan", Description: "Read-only planning mode"},
	{ID: string(permission.ModeYolo), Name: "YOLO", Description: "Allow every non-denied tool call"},
}

// Options wires the ACP server to an already constructed tern runtime.
type Options struct {
	Version      string
	WorkDir      string
	Agent        *agent.Agent
	Permissions  *permission.Evaluator
	Session      *session.Session
	SessionStore *session.Store
}

// Server speaks ACP over a pair of streams. It is safe for concurrent use by
// the JSON-RPC read loop, agent event callbacks and prompt goroutines.
type Server struct {
	opts  Options
	agent *agent.Agent
	store *session.Store

	mu           sync.Mutex
	sess         *session.Session
	perms        *permission.Evaluator
	promptCancel context.CancelFunc
	promptActive bool

	out     *nlineWriter
	ctx     context.Context
	pending map[string]chan rpcIncoming
	nextID  int64
	wg      sync.WaitGroup

	// previous handlers are restored when Serve returns, so embedding the
	// server does not permanently hijack an agent.
	prevOnEvent   func(agent.Event)
	prevOnApprove func(context.Context, agent.ApprovalRequest) (bool, error)
}

// NewServer constructs an ACP server for an existing agent runtime.
func NewServer(opts Options) *Server {
	if opts.Agent != nil {
		if opts.Session == nil {
			opts.Session = opts.Agent.Session
		}
		if opts.Permissions == nil {
			opts.Permissions = opts.Agent.Permissions
		}
		if strings.TrimSpace(opts.WorkDir) == "" {
			opts.WorkDir = opts.Agent.WorkDir
		}
	}
	if opts.Permissions == nil {
		opts.Permissions = permission.Default()
	}
	return &Server{
		opts:    opts,
		agent:   opts.Agent,
		store:   opts.SessionStore,
		sess:    opts.Session,
		perms:   opts.Permissions,
		pending: map[string]chan rpcIncoming{},
	}
}

// nlineWriter serializes one JSON object per line. Every notification and
// response goes through the same mutex so concurrent streaming never
// interleaves partial lines.
type nlineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (n *nlineWriter) write(v any) error {
	if n == nil || n.w == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n.mu.Lock()
	defer n.mu.Unlock()
	_, err = n.w.Write(data)
	return err
}

// Serve runs the JSON-RPC 2.0 stdio loop until the input stream ends or ctx is
// cancelled.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if in == nil || out == nil {
		return errors.New("acp: stdin and stdout are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s.agent == nil {
		return errors.New("acp: server requires an agent")
	}

	s.mu.Lock()
	s.ctx = ctx
	s.out = &nlineWriter{w: out}
	s.prevOnEvent = s.agent.OnEvent
	s.prevOnApprove = s.agent.OnApprove
	s.agent.OnEvent = s.handleEvent
	s.agent.OnApprove = s.approve
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.agent.OnEvent = s.prevOnEvent
		s.agent.OnApprove = s.prevOnApprove
		s.mu.Unlock()
	}()

	dec := json.NewDecoder(bufio.NewReaderSize(in, 64*1024))
	for {
		if err := ctx.Err(); err != nil {
			s.shutdown()
			return nil
		}
		var msg rpcIncoming
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				s.shutdown()
				return nil
			}
			if ctx.Err() != nil {
				s.shutdown()
				return nil
			}
			return fmt.Errorf("acp: decode request: %w", err)
		}

		// A message without a method is a response to a server-initiated
		// request (session/request_permission).
		if strings.TrimSpace(msg.Method) == "" {
			s.deliver(msg)
			continue
		}
		// A message without an id is a notification.
		if len(msg.ID) == 0 || string(msg.ID) == "null" {
			s.handleNotification(msg)
			continue
		}
		s.handleRequest(msg)
	}
}

func (s *Server) shutdown() {
	s.mu.Lock()
	cancel := s.promptCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// ---------------------------------------------------------------------------
// Request dispatch
// ---------------------------------------------------------------------------

func (s *Server) handleNotification(msg rpcIncoming) {
	switch msg.Method {
	case "session/cancel":
		s.cancelPrompt()
	case "initialized", "$/cancelRequest":
		// Host acknowledgements need no action.
	}
}

func (s *Server) handleRequest(msg rpcIncoming) {
	switch msg.Method {
	case "initialize":
		s.respond(msg.ID, initializeResult{
			ProtocolVersion: ProtocolVersion,
			AgentInfo:       agentInfo{Name: "tern", Title: "tern", Version: s.opts.Version},
			AgentCapabilities: agentCapabilities{
				LoadSession:        true,
				PromptCapabilities: promptCapabilities{Image: true, EmbeddedContext: true},
			},
		})
	case "session/new":
		s.handleNewSession(msg)
	case "session/load":
		s.handleLoadSession(msg)
	case "session/set_model":
		s.handleSetModel(msg)
	case "session/set_mode":
		s.handleSetMode(msg)
	case "session/prompt":
		s.handlePrompt(msg)
	case "session/cancel":
		cancelled := s.cancelPrompt()
		s.respond(msg.ID, map[string]any{"cancelled": cancelled})
	case "session/stats":
		s.handleStats(msg)
	default:
		s.respondError(msg.ID, CodeMethodNotFound, fmt.Sprintf("unknown method %q", msg.Method))
	}
}

func (s *Server) handleNewSession(msg rpcIncoming) {
	var params newSessionParams
	if err := decodeParams(msg.Params, &params); err != nil {
		s.respondError(msg.ID, CodeInvalidParams, err.Error())
		return
	}
	s.mu.Lock()
	store := s.store
	agentRef := s.agent
	current := s.sess
	s.mu.Unlock()

	var sess *session.Session
	switch {
	case store != nil:
		workDir := strings.TrimSpace(params.Cwd)
		if workDir == "" {
			workDir = s.opts.WorkDir
		}
		created, err := store.Create(session.SessionMeta{
			Model:   agentRef.Config.Model,
			WorkDir: workDir,
		})
		if err != nil {
			s.respondError(msg.ID, CodeInternalError, err.Error())
			return
		}
		sess = created
	case current != nil:
		sess = current
	default:
		s.respondError(msg.ID, CodeInternalError, "no session store configured")
		return
	}

	agentRef.SwitchSession(sess)
	s.mu.Lock()
	s.sess = sess
	s.mu.Unlock()
	s.respond(msg.ID, s.sessionState())
}

func (s *Server) handleLoadSession(msg rpcIncoming) {
	var params loadSessionParams
	if err := decodeParams(msg.Params, &params); err != nil {
		s.respondError(msg.ID, CodeInvalidParams, err.Error())
		return
	}
	s.mu.Lock()
	store := s.store
	agentRef := s.agent
	s.mu.Unlock()
	if store == nil {
		s.respondError(msg.ID, CodeInternalError, "no session store configured")
		return
	}
	sess, err := store.Open(strings.TrimSpace(params.SessionID))
	if err != nil {
		s.respondError(msg.ID, CodeInvalidParams, err.Error())
		return
	}
	agentRef.SwitchSession(sess)
	s.mu.Lock()
	s.sess = sess
	s.mu.Unlock()
	s.respond(msg.ID, s.sessionState())
}

func (s *Server) handleSetModel(msg rpcIncoming) {
	var params setModelParams
	if err := decodeParams(msg.Params, &params); err != nil {
		s.respondError(msg.ID, CodeInvalidParams, err.Error())
		return
	}
	model := strings.TrimSpace(params.ModelID)
	if model != "" {
		s.agent.SetModel(model)
	}
	effort := strings.TrimSpace(firstNonEmpty(params.Effort, params.ReasoningEffort))
	// Some hosts put the reasoning effort in configId (for example
	// "deepseek-v4-pro[high]").
	if effort == "" && params.ConfigID != "" {
		if idx := strings.LastIndex(params.ConfigID, "["); idx >= 0 && strings.HasSuffix(params.ConfigID, "]") {
			if m := strings.TrimSpace(params.ConfigID[:idx]); m != "" {
				s.agent.SetModel(m)
			}
			effort = strings.TrimSpace(strings.TrimSuffix(params.ConfigID[idx+1:], "]"))
		} else if isReasoningEffort(params.ConfigID) {
			effort = params.ConfigID
		}
	}
	if effort != "" && isReasoningEffort(effort) {
		s.agent.SetReasoningEffort(strings.ToLower(effort))
	}
	s.respond(msg.ID, s.sessionState())
}

func (s *Server) handleSetMode(msg rpcIncoming) {
	var params setModeParams
	if err := decodeParams(msg.Params, &params); err != nil {
		s.respondError(msg.ID, CodeInvalidParams, err.Error())
		return
	}
	mode, err := parseModeID(params.ModeID)
	if err != nil {
		s.respondError(msg.ID, CodeInvalidParams, err.Error())
		return
	}
	s.mu.Lock()
	policy := s.perms.Policy()
	policy.Mode = mode
	updated := permission.New(policy)
	s.perms = updated
	s.agent.Permissions = updated
	s.mu.Unlock()
	s.respond(msg.ID, s.sessionState())
}

func (s *Server) handleStats(msg rpcIncoming) {
	s.mu.Lock()
	sess := s.sess
	store := s.store
	s.mu.Unlock()

	result := sessionStatsResult{
		ContextWindow: s.contextWindow(),
	}
	if s.agent != nil {
		result.ContextTokens = s.agent.ContextTokens()
	}
	if sess != nil {
		result.SessionID = sess.ID()
		usage, cost := sess.TotalUsage()
		result.Usage = usage
		result.TotalCostUSD = cost
		result.CacheHitRate = usageCacheHitRate(usage)
	}
	if store != nil {
		if stats, err := store.Stats(); err == nil {
			result.Store = stats
		}
	}
	s.respond(msg.ID, result)
}

// ---------------------------------------------------------------------------
// session/prompt
// ---------------------------------------------------------------------------

func (s *Server) handlePrompt(msg rpcIncoming) {
	var params promptParams
	if err := decodeParams(msg.Params, &params); err != nil {
		s.respondError(msg.ID, CodeInvalidParams, err.Error())
		return
	}
	text, attachments := buildPrompt(params.Prompt)

	s.mu.Lock()
	if s.promptActive {
		s.mu.Unlock()
		s.respondError(msg.ID, CodeInvalidRequest, "a prompt is already running for this session")
		return
	}
	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	turnCtx, cancel := context.WithCancel(parent)
	s.promptCancel = cancel
	s.promptActive = true
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		res, err := s.agent.RunTurn(turnCtx, text, attachments)
		s.mu.Lock()
		s.promptActive = false
		s.promptCancel = nil
		s.mu.Unlock()
		s.respondPrompt(msg.ID, res, err)
	}()
}

func (s *Server) respondPrompt(id json.RawMessage, res *agent.TurnResult, err error) {
	if res == nil {
		if errors.Is(err, context.Canceled) {
			s.respond(id, promptResult{
				StopReason: "cancelled",
				Meta:       promptMeta{ContextWindow: s.contextWindow()},
			})
			return
		}
		if err != nil {
			s.respondError(id, CodeInternalError, err.Error())
			return
		}
		s.respond(id, promptResult{StopReason: "end_turn"})
		return
	}

	usage := promptUsage{
		InputTokens:      res.Usage.PromptTokens,
		OutputTokens:     res.Usage.CompletionTokens,
		TotalTokens:      res.Usage.TotalTokens,
		CachedReadTokens: res.Usage.PromptCacheHitTokens,
		ThoughtTokens:    res.Usage.ReasoningTokens,
	}
	meta := promptMeta{
		CostUSD:       res.CostUSD,
		TotalCostUSD:  res.TotalCostUSD,
		ContextTokens: res.ContextTokens,
		ContextWindow: s.contextWindow(),
		CacheHitRate:  res.CacheHitRate,
		TokensPerSec:  res.TokensPerSec,
		Steps:         res.Steps,
		ToolCalls:     res.ToolCalls,
		DurationMs:    res.DurationMs,
	}
	s.respond(id, promptResult{
		StopReason: promptStopReason(res.StopReason, err),
		Usage:      usage,
		Meta:       meta,
	})
}

func (s *Server) cancelPrompt() bool {
	s.mu.Lock()
	cancel := s.promptCancel
	s.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// ---------------------------------------------------------------------------
// Agent events -> session/update notifications
// ---------------------------------------------------------------------------

func (s *Server) handleEvent(ev agent.Event) {
	s.mu.Lock()
	sid := ""
	if s.sess != nil {
		sid = s.sess.ID()
	}
	s.mu.Unlock()

	switch ev.Type {
	case agent.EventReasoningDelta:
		if ev.Text != "" {
			s.notify("session/update", textChunkUpdate{
				SessionID:     sid,
				SessionUpdate: "agent_thought_chunk",
				Content:       contentBlock{Type: "text", Text: ev.Text},
			})
		}
	case agent.EventTextDelta:
		if ev.Text != "" {
			s.notify("session/update", textChunkUpdate{
				SessionID:     sid,
				SessionUpdate: "agent_message_chunk",
				Content:       contentBlock{Type: "text", Text: ev.Text},
			})
		}
	case agent.EventToolStart:
		s.notify("session/update", toolCallUpdate{
			SessionID:     sid,
			SessionUpdate: "tool_call",
			ToolCallID:    ev.ToolCallID,
			Title:         toolTitle(ev.ToolName, ev.Args),
			Kind:          toolKind(ev.ToolName),
			Status:        "in_progress",
			RawInput:      rawJSON(ev.Args),
		})
	case agent.EventToolEnd:
		status := "completed"
		if ev.IsError {
			status = "failed"
		}
		s.notify("session/update", toolCallUpdate{
			SessionID:     sid,
			SessionUpdate: "tool_call_update",
			ToolCallID:    ev.ToolCallID,
			Status:        status,
			RawOutput:     ev.Output,
			Content: []toolCallContent{{
				Type:    "content",
				Content: contentBlock{Type: "text", Text: ev.Output},
			}},
		})
	case agent.EventUsage:
		if ev.Usage != nil {
			s.notify("session/update", usageUpdate{
				SessionID:     sid,
				SessionUpdate: "usage_update",
				Used:          ev.ContextTokens,
				Size:          s.contextWindow(),
				Cost:          usageCost{Amount: ev.TotalCostUSD, Currency: "USD"},
				Meta: usageMeta{
					InputTokens:      ev.Usage.PromptTokens,
					OutputTokens:     ev.Usage.CompletionTokens,
					TotalTokens:      ev.Usage.TotalTokens,
					CachedReadTokens: ev.Usage.PromptCacheHitTokens,
					CacheMissTokens:  ev.Usage.PromptCacheMissTokens,
					ReasoningTokens:  ev.Usage.ReasoningTokens,
					CacheHitRate:     ev.CacheHitRate,
					TokensPerSec:     ev.TokensPerSec,
					TurnCostUSD:      ev.CostUSD,
					TotalCostUSD:     ev.TotalCostUSD,
				},
			})
		}
	}
}

func (s *Server) notify(method string, params any) {
	s.mu.Lock()
	out := s.out
	s.mu.Unlock()
	if out == nil {
		return
	}
	_ = out.write(RPCNotification{JSONRPC: JSONRPCVersion, Method: method, Params: params})
}

// ---------------------------------------------------------------------------
// session/request_permission
// ---------------------------------------------------------------------------

func (s *Server) approve(ctx context.Context, req agent.ApprovalRequest) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	out := s.out
	sid := ""
	if s.sess != nil {
		sid = s.sess.ID()
	}
	s.nextID++
	key := fmt.Sprintf("%q", "tern-permission-"+strconv.FormatInt(s.nextID, 10))
	ch := make(chan rpcIncoming, 1)
	s.pending[key] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
	}()

	if out == nil {
		return false, errors.New("acp: server is not serving")
	}
	params := permissionRequestParams{
		SessionID: sid,
		ToolCall: permissionToolCall{
			ToolCallID: fmt.Sprintf("approval-%d-%d", req.Turn, req.Step),
			Title:      toolTitle(req.ToolName, req.Args),
			Kind:       toolKind(req.ToolName),
			Status:     "pending",
			RawInput:   rawJSON(req.Args),
		},
		Options: []permissionOption{
			{OptionID: "allow_once", Name: "Allow", Kind: "allow_once"},
			{OptionID: "allow_always", Name: "Always allow", Kind: "allow_always"},
			{OptionID: "reject_once", Name: "Reject", Kind: "reject_once"},
			{OptionID: "reject_always", Name: "Always reject", Kind: "reject_always"},
		},
	}
	if err := out.write(RPCRequest{
		JSONRPC: JSONRPCVersion,
		ID:      json.RawMessage(key),
		Method:  "session/request_permission",
		Params:  params,
	}); err != nil {
		return false, err
	}

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return false, resp.Error
		}
		var result permissionResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return false, fmt.Errorf("acp: decode permission result: %w", err)
		}
		if result.Outcome.Outcome != "selected" {
			return false, nil
		}
		return strings.HasPrefix(result.Outcome.OptionID, "allow"), nil
	}
}

func (s *Server) deliver(msg rpcIncoming) {
	key := string(msg.ID)
	s.mu.Lock()
	ch := s.pending[key]
	s.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- msg:
	default:
	}
}

// ---------------------------------------------------------------------------
// Responses and helpers
// ---------------------------------------------------------------------------

func (s *Server) respond(id json.RawMessage, result any) {
	s.mu.Lock()
	out := s.out
	s.mu.Unlock()
	if out == nil {
		return
	}
	_ = out.write(RPCResponse{JSONRPC: JSONRPCVersion, ID: id, Result: result})
}

func (s *Server) respondError(id json.RawMessage, code int, message string) {
	s.mu.Lock()
	out := s.out
	s.mu.Unlock()
	if out == nil {
		return
	}
	_ = out.write(RPCResponse{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Error:   &RPCError{Code: code, Message: message},
	})
}

func (s *Server) sessionState() sessionState {
	s.mu.Lock()
	sid := ""
	if s.sess != nil {
		sid = s.sess.ID()
	}
	mode := s.perms.Mode()
	s.mu.Unlock()

	model := ""
	if s.agent != nil && s.agent.Config != nil {
		model = s.agent.Config.Model
	}
	availableModels := make([]modelInfo, 0, len(AvailableModels))
	availableModels = append(availableModels, AvailableModels...)
	if model != "" && !hasModel(availableModels, model) {
		availableModels = append(availableModels, modelInfo{ModelID: model, Name: model})
	}
	return sessionState{
		SessionID: sid,
		Models: modelsState{
			CurrentModelID:  model,
			AvailableModels: availableModels,
		},
		Modes: modesState{
			CurrentModeID:  string(mode),
			AvailableModes: append([]modeInfo(nil), AvailableModes...),
		},
	}
}

func (s *Server) contextWindow() int {
	if s.agent == nil || s.agent.Provider == nil {
		return provider.DefaultContextWindow
	}
	model := ""
	if s.agent.Config != nil {
		model = s.agent.Config.Model
	}
	caps := s.agent.Provider.ModelInfo(model)
	if caps.ContextWindow > 0 {
		return caps.ContextWindow
	}
	return provider.DefaultContextWindow
}

func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func buildPrompt(blocks []contentBlock) (string, []provider.ContentPart) {
	var text strings.Builder
	var attachments []provider.ContentPart
	for _, block := range blocks {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "image":
			if block.Data != "" {
				mime := block.MimeType
				if mime == "" {
					mime = "image/png"
				}
				attachments = append(attachments, provider.ContentPart{
					Type:     "image_url",
					ImageURL: &provider.ImageURL{URL: "data:" + mime + ";base64," + block.Data},
				})
			}
		case "resource":
			uri := block.URI
			resourceText := block.Text
			if block.Resource != nil {
				if uri == "" {
					uri = block.Resource.URI
				}
				if resourceText == "" {
					resourceText = block.Resource.Text
				}
			}
			if resourceText != "" {
				if text.Len() > 0 {
					text.WriteString("\n\n")
				}
				if uri != "" {
					fmt.Fprintf(&text, "<resource uri=%q>\n%s\n</resource>", uri, resourceText)
				} else {
					text.WriteString(resourceText)
				}
			}
		}
	}
	return text.String(), attachments
}

func stopReason(reason agent.StopReason) string {
	switch reason {
	case agent.StopCancelled:
		return "cancelled"
	case agent.StopMaxSteps, agent.StopBudget:
		return "max_turn_requests"
	case agent.StopError:
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// promptStopReason prefers the transport error over the agent's generic error
// stop reason. A cancelled context must surface as "cancelled" even when the
// provider reports the cancellation as a stream error.
func promptStopReason(reason agent.StopReason, err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	return stopReason(reason)
}

func parseModeID(id string) (permission.Mode, error) {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "ask", "approval-required", "approval_required":
		return permission.ModeAsk, nil
	case "auto-edit", "auto_edit", "auto-accept-edits", "auto_accept_edits":
		return permission.ModeAutoEdit, nil
	case "plan":
		return permission.ModePlan, nil
	case "yolo", "full-access", "full_access", "auto":
		return permission.ModeYolo, nil
	default:
		return "", fmt.Errorf("acp: unknown mode %q", id)
	}
}

func isReasoningEffort(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "low", "high", "max":
		return true
	}
	return false
}

func hasModel(models []modelInfo, id string) bool {
	for _, m := range models {
		if m.ModelID == id {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func rawJSON(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

func toolTitle(name string, args json.RawMessage) string {
	title := strings.TrimSpace(name)
	if title == "" {
		title = "tool"
	}
	var parsed map[string]any
	if len(args) > 0 && json.Unmarshal(args, &parsed) == nil {
		for _, key := range []string{"path", "command", "query", "url"} {
			if v, ok := parsed[key].(string); ok && strings.TrimSpace(v) != "" {
				return title + ": " + v
			}
		}
	}
	return title
}

func toolKind(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read", "ls", "glob", "grep":
		return "read"
	case "write", "edit", "multi_edit", "patch", "str_replace":
		return "edit"
	case "bash", "shell", "execute":
		return "execute"
	case "web_fetch":
		return "fetch"
	case "web_search":
		return "search"
	case "task", "todo":
		return "think"
	default:
		return "other"
	}
}

func usageCacheHitRate(u provider.Usage) float64 {
	denom := u.PromptCacheHitTokens + u.PromptCacheMissTokens
	if denom <= 0 {
		denom = u.PromptTokens
	}
	if denom <= 0 {
		return 0
	}
	return float64(u.PromptCacheHitTokens) / float64(denom)
}
