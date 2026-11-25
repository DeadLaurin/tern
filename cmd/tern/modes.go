package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/subagent"
)

// ---------------------------------------------------------------------------
// NDJSON output
// ---------------------------------------------------------------------------

// ndjsonWriter serializes one JSON object per line. A single mutex protects
// all writes so live agent events and RPC responses can never interleave
// partial lines.
type ndjsonWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newNDJSONWriter(w io.Writer) *ndjsonWriter { return &ndjsonWriter{w: w} }

func (n *ndjsonWriter) write(v any) error {
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

// subagentEventJSON mirrors subagent.Event but keeps agent_id and
// parent_tool_call_id present even when empty, so consumers can rely on the
// envelope shape.
type subagentEventJSON struct {
	Type             string         `json:"type"`
	AgentID          string         `json:"agent_id"`
	ParentToolCallID string         `json:"parent_tool_call_id"`
	AgentType        string         `json:"agent_type,omitempty"`
	Description      string         `json:"description,omitempty"`
	Status           string         `json:"status,omitempty"`
	Step             int            `json:"step,omitempty"`
	CurrentTool      string         `json:"current_tool,omitempty"`
	Usage            provider.Usage `json:"usage,omitempty"`
	CostUSD          float64        `json:"cost_usd,omitempty"`
	Elapsed          time.Duration  `json:"elapsed,omitempty"`
	Summary          string         `json:"summary,omitempty"`
	Detail           string         `json:"detail,omitempty"`
}

func (n *ndjsonWriter) WriteEvent(ev agent.Event) error { return n.write(ev) }

func (n *ndjsonWriter) WriteSubagent(ev subagent.Event) error {
	return n.write(subagentEventJSON{
		Type:             ev.Type,
		AgentID:          ev.AgentID,
		ParentToolCallID: ev.ParentToolCallID,
		AgentType:        ev.AgentType,
		Description:      ev.Description,
		Status:           ev.Status,
		Step:             ev.Step,
		CurrentTool:      ev.CurrentTool,
		Usage:            ev.Usage,
		CostUSD:          ev.CostUSD,
		Elapsed:          ev.Elapsed,
		Summary:          ev.Summary,
		Detail:           ev.Detail,
	})
}

// ---------------------------------------------------------------------------
// Print mode
// ---------------------------------------------------------------------------

type printStream struct {
	mu    sync.Mutex
	out   io.Writer
	wrote bool
}

func newPrintStream(out io.Writer) *printStream {
	return &printStream{out: out}
}

func (p *printStream) onEvent(ev agent.Event) {
	if ev.Type != agent.EventTextDelta || ev.Text == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = io.WriteString(p.out, ev.Text)
	p.wrote = true
}

func (p *printStream) finish(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.wrote && text != "" {
		_, _ = io.WriteString(p.out, text)
		p.wrote = true
	}
	if p.wrote {
		_, _ = io.WriteString(p.out, "\n")
	}
}

func printModeSubagentEvent(errOut io.Writer) func(subagent.Event) {
	return func(ev subagent.Event) {
		switch ev.Type {
		case subagent.EventSubagentStart:
			// The manager emits a queued start followed by a running start.
			// Log only the running one so the line is not duplicated.
			if ev.Status != subagent.StatusRunning {
				return
			}
			fmt.Fprintf(errOut, "[subagent] start %s: %s\n", ev.AgentType, ev.Description)
		case subagent.EventSubagentEnd:
			fmt.Fprintf(errOut, "[subagent] done %s (%d steps, %.1fs, $%.3f)\n",
				ev.AgentType, ev.Step, ev.Elapsed.Seconds(), ev.CostUSD)
		}
	}
}

func runPrint(ctx context.Context, rt *Runtime, prompt string, out, errOut io.Writer) int {
	res, err := rt.Agent.RunTurn(ctx, prompt, nil)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintf(errOut, "tern: %v\n", err)
		}
		return 1
	}
	// The streamed text has already been written by the event callback. If the
	// provider produced no text deltas, print the accumulated result once.
	if ps, ok := rt.agentPrintStream(); ok {
		ps.finish(res.Text)
	} else if res.Text != "" {
		fmt.Fprintln(out, res.Text)
	}
	return 0
}

// agentPrintStream retrieves the print stream from the runtime's event
// callback via a small registry. Keeping it out of Runtime avoids polluting
// the production struct with a CLI-only concern.
func (rt *Runtime) agentPrintStream() (*printStream, bool) {
	if rt == nil {
		return nil, false
	}
	return rt.printStream, rt.printStream != nil
}

// ---------------------------------------------------------------------------
// JSON mode
// ---------------------------------------------------------------------------

func runJSON(ctx context.Context, rt *Runtime, prompt string, out, errOut io.Writer) int {
	if _, err := rt.Agent.RunTurn(ctx, prompt, nil); err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintf(errOut, "tern: %v\n", err)
		}
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// RPC mode
// ---------------------------------------------------------------------------

type rpcRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     string `json:"id,omitempty"`
	Type   string `json:"type"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type rpcStatus struct {
	SessionID  string         `json:"session_id"`
	Model      string         `json:"model"`
	Permission string         `json:"permission"`
	Running    int            `json:"running"`
	Usage      provider.Usage `json:"usage"`
	CostUSD    float64        `json:"cost_usd"`
	Agents     int            `json:"agents"`
}

type rpcPromptParams struct {
	Text   string `json:"text"`
	Prompt string `json:"prompt,omitempty"`
}

type rpcCancelAgentParams struct {
	AgentID string `json:"agent_id"`
}

type rpcCompactParams struct {
	Instructions string `json:"instructions,omitempty"`
}

type rpcServer struct {
	rt     *Runtime
	sink   *ndjsonWriter
	errOut io.Writer

	mu      sync.Mutex
	cancel  context.CancelFunc
	running int
	wg      sync.WaitGroup
}

func runRPC(ctx context.Context, rt *Runtime, in io.Reader, sink *ndjsonWriter, errOut io.Writer) int {
	if sink == nil {
		return 1
	}
	s := &rpcServer{rt: rt, sink: sink, errOut: errOut}
	dec := json.NewDecoder(bufio.NewReaderSize(in, 64*1024))
	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			_ = sink.write(rpcResponse{Type: "error", Error: "invalid request: " + err.Error()})
			return 1
		}
		method := strings.ToLower(strings.TrimSpace(req.Method))
		switch method {
		case "prompt":
			text, err := decodePromptParams(req.Params)
			if err != nil {
				s.respondError(req.ID, err)
				continue
			}
			if strings.TrimSpace(text) == "" {
				s.respondError(req.ID, errors.New("prompt requires params.text"))
				continue
			}
			s.startPrompt(ctx, req.ID, text)
		case "cancel":
			s.mu.Lock()
			cancel := s.cancel
			s.mu.Unlock()
			cancelled := false
			if cancel != nil {
				cancel()
				cancelled = true
			}
			s.respond(req.ID, map[string]any{"cancelled": cancelled}, nil)
		case "cancel_agent":
			var params rpcCancelAgentParams
			_ = decodeParams(req.Params, &params)
			cancelled := false
			if params.AgentID != "" {
				cancelled = rt.Subagents.CancelOne(params.AgentID)
			}
			s.respond(req.ID, map[string]any{"cancelled": cancelled}, nil)
		case "compact":
			var params rpcCompactParams
			_ = decodeParams(req.Params, &params)
			res, err := rt.Agent.Compact(ctx, params.Instructions)
			s.respond(req.ID, res, err)
		case "status":
			s.respond(req.ID, s.status(), nil)
		case "quit":
			s.wg.Wait()
			s.respond(req.ID, map[string]any{"quit": true}, nil)
			return 0
		default:
			s.respondError(req.ID, fmt.Errorf("unknown method %q", req.Method))
		}
	}
	s.wg.Wait()
	return 0
}

func (s *rpcServer) startPrompt(parent context.Context, id, text string) {
	turnCtx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	s.cancel = cancel
	s.running++
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		res, err := s.rt.Agent.RunTurn(turnCtx, text, nil)
		cancel()
		s.mu.Lock()
		s.running--
		if s.running == 0 {
			s.cancel = nil
		}
		s.mu.Unlock()
		s.respond(id, res, err)
	}()
}

func (s *rpcServer) status() rpcStatus {
	usage, cost := s.rt.Agent.TotalUsage()
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	return rpcStatus{
		SessionID:  s.rt.Session.ID(),
		Model:      s.rt.Config.Model,
		Permission: s.rt.Permissions.Mode().String(),
		Running:    running,
		Usage:      usage,
		CostUSD:    cost,
		Agents:     len(s.rt.Subagents.List()),
	}
}

func (s *rpcServer) respond(id string, result any, err error) {
	resp := rpcResponse{ID: id, Type: "response", Result: result}
	if err != nil {
		resp.Error = err.Error()
		if resp.Result == nil {
			resp.Result = nil
		}
	}
	if werr := s.sink.write(resp); werr != nil && s.errOut != nil {
		fmt.Fprintf(s.errOut, "tern: rpc write: %v\n", werr)
	}
}

func (s *rpcServer) respondError(id string, err error) {
	s.respond(id, nil, err)
}

func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func decodePromptParams(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", err
		}
		return text, nil
	}
	var params rpcPromptParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return "", err
	}
	if strings.TrimSpace(params.Text) != "" {
		return params.Text, nil
	}
	return params.Prompt, nil
}
