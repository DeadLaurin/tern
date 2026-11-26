// Package serve implements tern's lightweight, LAN-accessible web UI.
//
// The server is a thin transport over an already-wired agent.Agent: it streams
// the agent's Event bus to browsers over Server-Sent Events, accepts prompts,
// steering messages and cancellations, and resolves tool approvals from the
// browser. It has no external dependencies beyond the Go standard library and
// tern's own packages.
package serve

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
	"github.com/tern-agent/tern/internal/theme"
)

//go:embed ui.html
var uiHTML []byte

// sseKeepAlive is how often a comment frame is written to idle SSE clients.
const sseKeepAlive = 20 * time.Second

// Options configures a Server. Agent is required for any real work; the
// remaining fields are optional but make the status and history endpoints
// complete.
type Options struct {
	Context context.Context
	Agent   *agent.Agent
	Session *session.Session
	Store   *session.Store
	Config  *config.Config
	// Permissions is tern's permission evaluator. (There is no
	// permission.Manager type in this codebase.)
	Permissions *permission.Evaluator
	Version         string
	WorkDir         string
	GlobalConfigDir string
	// Password enables password authentication when non-empty.
	Password string
	// AuthEnabled forces authentication on or off. When Password is set,
	// authentication is enabled regardless.
	AuthEnabled bool
}

// Server is the HTTP handler set for `tern serve`.
type Server struct {
	Agent       *agent.Agent
	Session     *session.Session
	Store       *session.Store
	Config      *config.Config
	Permissions *permission.Evaluator
	Version         string
	WorkDir         string
	GlobalConfigDir string
	Password        string
	AuthEnabled     bool

	auth   *authManager
	ctx    context.Context
	cancel context.CancelFunc

	mu            sync.Mutex
	running       bool
	cancelTurn    context.CancelFunc
	model         string
	effort        string
	usage         provider.Usage
	totalCost     float64
	cacheHit      float64
	contextTokens int
	tokensPerSec  float64
	startedAt     time.Time
	clients       map[chan sseFrame]struct{}
	approvals     map[string]*pendingApproval
	pending       *approvalWire
}

// sseFrame is one Server-Sent Event ready for the wire.
type sseFrame struct {
	event string
	data  []byte
}

// approvalWire is the JSON shape sent to browsers for tool approvals.
type approvalWire struct {
	ID        string          `json:"id"`
	ToolName  string          `json:"tool_name"`
	Args      json.RawMessage `json:"args,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Path      string          `json:"path,omitempty"`
	Command   string          `json:"command,omitempty"`
	Diff      string          `json:"diff,omitempty"`
	IsWeb     bool            `json:"is_web,omitempty"`
	ReadOnly  bool            `json:"read_only,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
}

// approvalDecision is a resolved approval sent back to the waiting agent.
type approvalDecision struct {
	allow    bool
	decision string
}

type pendingApproval struct {
	wire approvalWire
	ch   chan approvalDecision
}

// New builds a Server and wires the agent's event and approval callbacks.
func New(opts Options) *Server {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)

	s := &Server{
		Agent:       opts.Agent,
		Session:     opts.Session,
		Store:       opts.Store,
		Config:      opts.Config,
		Permissions: opts.Permissions,
		Version:         opts.Version,
		WorkDir:         opts.WorkDir,
		GlobalConfigDir: opts.GlobalConfigDir,
		Password:        opts.Password,
		AuthEnabled: opts.AuthEnabled || strings.TrimSpace(opts.Password) != "",

		auth:      newAuthManager(opts.Password, opts.AuthEnabled || strings.TrimSpace(opts.Password) != ""),
		ctx:       ctx,
		cancel:    cancel,
		startedAt: time.Now(),
		clients:   make(map[chan sseFrame]struct{}),
		approvals: make(map[string]*pendingApproval),
	}

	if s.Config == nil && s.Agent != nil {
		s.Config = s.Agent.Config
	}
	if s.Config == nil {
		s.Config = config.Default()
	}
	if s.WorkDir == "" && s.Agent != nil {
		s.WorkDir = s.Agent.WorkDir
	}
	if s.Session == nil && s.Agent != nil {
		s.Session = s.Agent.Session
	}
	if s.Permissions == nil && s.Agent != nil {
		s.Permissions = s.Agent.Permissions
	}

	s.model = s.Config.Model
	s.effort = s.Config.ReasoningEffort

	// Seed accounting from the session (or the agent) before any turn runs, so
	// the first status request already shows restored usage.
	if s.Session != nil {
		s.usage, s.totalCost = s.Session.Usage()
	} else if s.Agent != nil {
		s.usage, s.totalCost = s.Agent.TotalUsage()
	}
	s.cacheHit = cacheHitRate(s.usage)
	if s.Agent != nil {
		s.contextTokens = s.Agent.ContextTokens()
		s.Agent.OnEvent = s.onEvent
		s.Agent.OnApprove = s.onApprove
	}
	return s
}

// Close stops any active turn and releases SSE connections.
func (s *Server) Close() {
	if s == nil || s.cancel == nil {
		return
	}
	s.mu.Lock()
	cancelTurn := s.cancelTurn
	s.mu.Unlock()
	if cancelTurn != nil {
		cancelTurn()
	}
	s.cancel()
}

// Handler returns the complete HTTP handler including the auth middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/theme.css", s.handleThemeCSS)
	mux.HandleFunc("/api/auth/status", s.handleAuthStatus)
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/prompt", s.handlePrompt)
	mux.HandleFunc("/api/cancel", s.handleCancel)
	mux.HandleFunc("/api/approve", s.handleApprove)
	mux.HandleFunc("/api/model", s.handleModel)
	mux.HandleFunc("/api/permission", s.handlePermission)
	mux.HandleFunc("/api/compact", s.handleCompact)
	return s.authMiddleware(mux)
}

// ---------------------------------------------------------------------------
// Static UI
// ---------------------------------------------------------------------------

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(uiHTML)
}

func (s *Server) handleThemeCSS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	loaded, err := theme.Load(s.WorkDir, s.GlobalConfigDir)
	if err != nil || loaded == nil || !loaded.HasCustom() {
		return
	}
	_, _ = io.WriteString(w, loaded.RawCSS)
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

type statusResponse struct {
	Version         string         `json:"version"`
	Model           string         `json:"model"`
	Effort          string         `json:"effort,omitempty"`
	WorkDir         string         `json:"work_dir"`
	SessionID       string         `json:"session_id"`
	Permission      string         `json:"permission"`
	Running         bool           `json:"running"`
	ContextTokens   int            `json:"context_tokens"`
	ContextWindow   int            `json:"context_window"`
	CacheHitRate    float64        `json:"cache_hit_rate"`
	TotalCostUSD    float64        `json:"total_cost_usd"`
	TokensPerSec    float64        `json:"tokens_per_sec"`
	UptimeSec       int64          `json:"uptime_sec"`
	Usage           provider.Usage `json:"usage"`
	PendingApproval *approvalWire  `json:"pending_approval,omitempty"`
	AuthEnabled     bool           `json:"auth_enabled"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.status())
}

func (s *Server) status() statusResponse {
	s.mu.Lock()
	resp := statusResponse{
		Version:       s.Version,
		Model:         s.model,
		Effort:        s.effort,
		WorkDir:       s.WorkDir,
		Permission:    s.permissionMode(),
		Running:       s.running,
		ContextTokens: s.contextTokens,
		CacheHitRate:  s.cacheHit,
		TotalCostUSD:  s.totalCost,
		TokensPerSec:  s.tokensPerSec,
		UptimeSec:     int64(time.Since(s.startedAt).Seconds()),
		Usage:         s.usage,
		AuthEnabled:   s.auth != nil && s.auth.Enabled(),
	}
	if s.pending != nil {
		pending := *s.pending
		resp.PendingApproval = &pending
	}
	model := s.model
	s.mu.Unlock()

	if s.Session != nil {
		resp.SessionID = s.Session.ID()
	} else if s.Agent != nil && s.Agent.Session != nil {
		resp.SessionID = s.Agent.Session.ID()
	}
	resp.ContextWindow = s.contextWindow(model)
	return resp
}

func (s *Server) permissionMode() string {
	if s.Permissions != nil {
		return s.Permissions.Mode().String()
	}
	if s.Config != nil && strings.TrimSpace(s.Config.PermissionMode) != "" {
		return s.Config.PermissionMode
	}
	return string(permission.DefaultMode)
}

func (s *Server) contextWindow(model string) int {
	if s.Agent != nil && s.Agent.Provider != nil {
		if caps := s.Agent.Provider.ModelInfo(model); caps.ContextWindow > 0 {
			return caps.ContextWindow
		}
	}
	if s.Config != nil {
		if p, ok := s.Config.Pricing[model]; ok && p.ContextWindow > 0 {
			return p.ContextWindow
		}
	}
	return provider.DefaultContextWindow
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

type toolCallView struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type messageView struct {
	Role       string         `json:"role"`
	Text       string         `json:"text,omitempty"`
	Reasoning  string         `json:"reasoning,omitempty"`
	ToolCalls  []toolCallView `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
	External   bool           `json:"external,omitempty"`
}

type historyResponse struct {
	SessionID string        `json:"session_id"`
	Messages  []messageView `json:"messages"`
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	msgs, flags := s.history()
	views := make([]messageView, 0, len(msgs))
	for i, msg := range msgs {
		view := messageView{
			Role:       string(msg.Role),
			Text:       messageText(msg),
			Reasoning:  msg.ReasoningContent,
			ToolCallID: msg.ToolCallID,
			Name:       msg.Name,
		}
		if i < len(flags) {
			view.External = flags[i]
		}
		for _, call := range msg.ToolCalls {
			view.ToolCalls = append(view.ToolCalls, toolCallView{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: json.RawMessage(call.Function.Arguments),
			})
		}
		views = append(views, view)
	}
	resp := historyResponse{Messages: views}
	if s.Session != nil {
		resp.SessionID = s.Session.ID()
	}
	writeJSON(w, http.StatusOK, resp)
}

// history returns the persisted conversation. It deliberately reads from the
// session (whose lock is short) rather than Agent.History, which would block
// for the full duration of a running turn.
func (s *Server) history() ([]provider.Message, []bool) {
	if s.Session != nil {
		return s.Session.ReplayMessagesWithFlags()
	}
	if s.Agent != nil {
		return s.Agent.History(), nil
	}
	return nil, nil
}

func messageText(msg provider.Message) string {
	if len(msg.Parts) == 0 {
		return msg.Content
	}
	var b strings.Builder
	for _, part := range msg.Parts {
		if part.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Server-Sent Events
// ---------------------------------------------------------------------------

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := make(chan sseFrame, 256)
	s.mu.Lock()
	s.clients[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, ch)
		s.mu.Unlock()
	}()

	ticker := time.NewTicker(sseKeepAlive)
	defer ticker.Stop()

	for {
		select {
		case frame := <-ch:
			if err := writeSSE(w, frame); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		}
	}
}

func writeSSE(w io.Writer, frame sseFrame) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", frame.event); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", frame.data); err != nil {
		return err
	}
	return nil
}

// broadcast fans a frame out to every connected browser. Slow clients drop
// frames instead of blocking the agent loop.
func (s *Server) broadcast(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	frame := sseFrame{event: event, data: data}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- frame:
		default:
		}
	}
}

// onEvent is installed as Agent.OnEvent. It updates the status counters and
// forwards the event to every browser.
func (s *Server) onEvent(ev agent.Event) {
	s.mu.Lock()
	switch ev.Type {
	case agent.EventUsage:
		if ev.Usage != nil {
			s.usage = addUsage(s.usage, *ev.Usage)
		}
		s.totalCost = ev.TotalCostUSD
		s.cacheHit = ev.CacheHitRate
		if ev.ContextTokens > 0 {
			s.contextTokens = ev.ContextTokens
		}
		if ev.TokensPerSec > 0 {
			s.tokensPerSec = ev.TokensPerSec
		}
	case agent.EventTurnEnd:
		s.totalCost = ev.TotalCostUSD
		s.cacheHit = ev.CacheHitRate
		if ev.ContextTokens > 0 {
			s.contextTokens = ev.ContextTokens
		}
		if ev.TokensPerSec > 0 {
			s.tokensPerSec = ev.TokensPerSec
		}
	case agent.EventCompact:
		s.cacheHit = 0
		if ev.ContextTokens > 0 {
			s.contextTokens = ev.ContextTokens
		}
	}
	s.mu.Unlock()
	s.broadcast(string(ev.Type), ev)
}

// ---------------------------------------------------------------------------
// Prompt, steer, cancel
// ---------------------------------------------------------------------------

type promptRequest struct {
	Text string `json:"text"`
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req promptRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	if s.Agent == nil {
		writeError(w, http.StatusServiceUnavailable, "agent is not available")
		return
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		if !s.Agent.Steer(text) {
			writeError(w, http.StatusBadRequest, "could not steer the active turn")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"steered": true})
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.running = true
	s.cancelTurn = cancel
	s.mu.Unlock()

	go func() {
		defer cancel()
		_, err := s.Agent.RunTurn(ctx, text, nil)
		s.mu.Lock()
		s.running = false
		s.cancelTurn = nil
		s.mu.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			// RunTurn already emits an error event; nothing else to do here.
			_ = err
		}
	}()

	writeJSON(w, http.StatusOK, map[string]any{"started": true})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.mu.Lock()
	cancel := s.cancelTurn
	running := s.running
	s.mu.Unlock()
	if !running || cancel == nil {
		writeError(w, http.StatusConflict, "no active turn to cancel")
		return
	}
	cancel()
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

// ---------------------------------------------------------------------------
// Approvals
// ---------------------------------------------------------------------------

// onApprove is installed as Agent.OnApprove. It publishes the request to the
// browser and blocks until the user answers, the turn is cancelled, or the
// server shuts down.
func (s *Server) onApprove(ctx context.Context, req agent.ApprovalRequest) (bool, error) {
	wire := approvalWire{
		ID:        newRequestID(),
		ToolName:  req.ToolName,
		Args:      req.Args,
		Reason:    req.Reason,
		Path:      req.Path,
		Command:   req.Command,
		Diff:      req.Diff,
		IsWeb:     req.IsWeb,
		ReadOnly:  req.ReadOnly,
		SessionID: req.SessionID,
	}
	pending := &pendingApproval{wire: wire, ch: make(chan approvalDecision, 1)}

	s.mu.Lock()
	s.approvals[wire.ID] = pending
	s.pending = &wire
	s.mu.Unlock()

	s.broadcast("approval_request", wire)

	defer func() {
		s.mu.Lock()
		delete(s.approvals, wire.ID)
		if s.pending != nil && s.pending.ID == wire.ID {
			s.pending = nil
		}
		s.mu.Unlock()
	}()

	select {
	case decision := <-pending.ch:
		return decision.allow, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-s.ctx.Done():
		return false, s.ctx.Err()
	}
}

type approveRequest struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req approveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	decision := strings.ToLower(strings.TrimSpace(req.Decision))
	var allow bool
	switch decision {
	case "allow":
		allow = true
	case "allow_session":
		// Session-wide approval is treated as a single allow for this call.
		// The permission policy is immutable during a turn, so persisting a
		// rule would require rebuilding the evaluator under the agent lock.
		allow = true
	case "deny":
		allow = false
	default:
		writeError(w, http.StatusBadRequest, "decision must be allow, allow_session or deny")
		return
	}

	s.mu.Lock()
	pending := s.approvals[strings.TrimSpace(req.ID)]
	if pending != nil {
		delete(s.approvals, pending.wire.ID)
		if s.pending != nil && s.pending.ID == pending.wire.ID {
			s.pending = nil
		}
	}
	s.mu.Unlock()
	if pending == nil {
		writeError(w, http.StatusNotFound, "no pending approval with that id")
		return
	}

	select {
	case pending.ch <- approvalDecision{allow: allow, decision: decision}:
	default:
	}
	s.broadcast("approval_resolved", map[string]any{
		"id":       pending.wire.ID,
		"decision": decision,
		"allow":    allow,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "decision": decision})
}

// ---------------------------------------------------------------------------
// Model and effort
// ---------------------------------------------------------------------------

type modelRequest struct {
	Model  string  `json:"model"`
	Effort *string `json:"effort"`
}

var validEfforts = map[string]bool{"none": true, "low": true, "high": true, "max": true}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req modelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	model := strings.TrimSpace(req.Model)
	effort := ""
	if req.Effort != nil {
		effort = strings.ToLower(strings.TrimSpace(*req.Effort))
		if effort != "" && !validEfforts[effort] {
			writeError(w, http.StatusBadRequest, "effort must be one of none, low, high, max")
			return
		}
	}
	if model == "" && req.Effort == nil {
		writeError(w, http.StatusBadRequest, "model or effort is required")
		return
	}

	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		writeError(w, http.StatusConflict, "cannot change model while a turn is running")
		return
	}
	if s.Agent == nil {
		writeError(w, http.StatusServiceUnavailable, "agent is not available")
		return
	}

	s.mu.Lock()
	if model != "" {
		s.Agent.SetModel(model)
		s.model = model
	}
	if req.Effort != nil {
		s.Agent.SetReasoningEffort(effort)
		s.effort = effort
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, s.status())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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

func cacheHitRate(u provider.Usage) float64 {
	total := u.PromptCacheHitTokens + u.PromptCacheMissTokens
	if total <= 0 {
		total = u.PromptTokens
	}
	if total <= 0 {
		return 0
	}
	return float64(u.PromptCacheHitTokens) / float64(total)
}

// ---------------------------------------------------------------------------
// Permission & Compaction
// ---------------------------------------------------------------------------

type permissionRequest struct {
	Mode string `json:"mode"`
}

func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req permissionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	mode, err := permission.ParseMode(strings.TrimSpace(req.Mode))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "cannot change permission mode while a turn is running")
		return
	}
	var nextEval *permission.Evaluator
	if s.Permissions != nil {
		policy := s.Permissions.Policy()
		policy.Mode = mode
		nextEval = permission.New(policy)
	} else {
		nextEval = permission.New(permission.Policy{Mode: mode, WorkDir: s.WorkDir})
	}
	s.Permissions = nextEval
	if s.Agent != nil {
		s.Agent.Permissions = nextEval
	}
	if s.Config != nil {
		s.Config.PermissionMode = mode.String()
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.status())
}

type compactRequest struct {
	Instructions string `json:"instructions,omitempty"`
}

func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.Agent == nil {
		writeError(w, http.StatusServiceUnavailable, "agent is not available")
		return
	}
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		writeError(w, http.StatusConflict, "cannot compact while a turn is running")
		return
	}
	var req compactRequest
	_ = decodeJSON(r, &req)
	res, err := s.Agent.Compact(r.Context(), req.Instructions)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"compacted": true, "result": res, "status": s.status()})
}
