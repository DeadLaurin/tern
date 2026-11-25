// Package ext implements user-facing extension points for tern: lifecycle
// hooks (user scripts driven over JSON on stdin/stdout) and an MCP
// (Model Context Protocol) client that can talk to tool servers over stdio or
// HTTP.
//
// The package depends only on the Go standard library.
package ext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"
)

// HookEvent identifies a lifecycle point at which hooks may run.
type HookEvent string

const (
	// HookBeforeTool runs before a tool call. A non-zero exit code blocks the
	// call.
	HookBeforeTool HookEvent = "before_tool"
	// HookAfterTool runs after a tool call and can observe the result.
	HookAfterTool HookEvent = "after_tool"
	// HookSessionStart runs when a session starts.
	HookSessionStart HookEvent = "session_start"
	// HookSessionEnd runs when a session ends.
	HookSessionEnd HookEvent = "session_end"
	// HookOnCompact runs when the conversation is compacted.
	HookOnCompact HookEvent = "on_compact"
	// HookSubagentStart runs when a subagent starts.
	HookSubagentStart HookEvent = "subagent_start"
	// HookSubagentEnd runs when a subagent finishes.
	HookSubagentEnd HookEvent = "subagent_end"
)

// DefaultHookTimeout is applied to hooks that do not configure a Timeout.
const DefaultHookTimeout = 10 * time.Second

// KnownHookEvents lists every supported hook event in execution order groups.
var KnownHookEvents = []HookEvent{
	HookSessionStart,
	HookBeforeTool,
	HookAfterTool,
	HookSubagentStart,
	HookSubagentEnd,
	HookOnCompact,
	HookSessionEnd,
}

// Valid reports whether e is a supported hook event.
func (e HookEvent) Valid() bool {
	for _, known := range KnownHookEvents {
		if e == known {
			return true
		}
	}
	return false
}

// HookConfig describes a single user hook.
type HookConfig struct {
	// Event is the lifecycle event that triggers the hook.
	Event HookEvent
	// Command is the executable to run. It is required.
	Command string
	// Args are passed to Command as arguments.
	Args []string
	// Matcher is an optional glob (see path.Match) matched against the tool
	// name (tool hooks) or agent name (subagent hooks). An empty matcher
	// matches everything.
	Matcher string
	// Timeout bounds the hook's runtime. Zero means DefaultHookTimeout.
	Timeout time.Duration
}

// WithDefaults returns a copy of the config with the default timeout applied.
func (c HookConfig) WithDefaults() HookConfig {
	if c.Timeout <= 0 {
		c.Timeout = DefaultHookTimeout
	}
	return c
}

// Validate reports whether the hook config can be executed.
func (c HookConfig) Validate() error {
	if c.Event == "" {
		return errors.New("ext: hook event is required")
	}
	if !c.Event.Valid() {
		return fmt.Errorf("ext: unknown hook event %q", c.Event)
	}
	if strings.TrimSpace(c.Command) == "" {
		return fmt.Errorf("ext: hook command is required for event %q", c.Event)
	}
	if _, err := path.Match(c.Matcher, "x"); err != nil && c.Matcher != "" {
		return fmt.Errorf("ext: invalid hook matcher %q: %w", c.Matcher, err)
	}
	return nil
}

// HookInput is serialized to JSON and delivered to the hook script on stdin.
type HookInput struct {
	Event     HookEvent       `json:"event"`
	Tool      string          `json:"tool,omitempty"`
	Agent     string          `json:"agent,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	CWD       string          `json:"cwd,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	// Extra carries event specific data such as tool output or compaction
	// summaries.
	Extra map[string]any `json:"extra,omitempty"`
}

// Target returns the name the hook matcher is applied to: the tool name for
// tool events, the agent name for subagent events, and "" otherwise.
func (in HookInput) Target() string {
	switch in.Event {
	case HookBeforeTool, HookAfterTool:
		return in.Tool
	case HookSubagentStart, HookSubagentEnd:
		return in.Agent
	default:
		return ""
	}
}

// HookResult is the outcome of executing a single hook.
type HookResult struct {
	// Hook is the config that produced this result.
	Hook HookConfig `json:"-"`
	// Event is the lifecycle event the hook ran for.
	Event HookEvent `json:"event"`
	// Allow reports whether the hook permitted the action. It is false when
	// the hook exited non-zero or timed out.
	Allow bool `json:"allow"`
	// Message is the hook's stdout (falling back to stderr) trimmed of
	// surrounding whitespace.
	Message string `json:"message,omitempty"`
	// ExitCode is the process exit code, or -1 when the hook could not run or
	// was killed after timing out.
	ExitCode int `json:"exit_code"`
	// Stdout and Stderr hold the captured output.
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	// Duration is the wall-clock runtime of the hook.
	Duration time.Duration `json:"duration"`
	// TimedOut reports whether the hook exceeded its timeout.
	TimedOut bool `json:"timed_out,omitempty"`
}

// Blocked reports whether any result denies the action.
func Blocked(results []HookResult) bool {
	for _, r := range results {
		if !r.Allow {
			return true
		}
	}
	return false
}

// Runner executes a set of hooks. It is safe to reuse across events and
// sessions.
type Runner struct {
	// Hooks are evaluated in slice order for every event.
	Hooks []HookConfig
	// Dir is the working directory for hook processes. Empty inherits the
	// current process directory.
	Dir string
	// Env holds additional KEY=VALUE entries appended to the inherited
	// environment.
	Env []string
}

// NewRunner returns a Runner for the given hook configs.
func NewRunner(hooks ...HookConfig) *Runner {
	return &Runner{Hooks: hooks}
}

// Add appends hooks to the runner.
func (r *Runner) Add(hooks ...HookConfig) {
	r.Hooks = append(r.Hooks, hooks...)
}

// Matching returns the hooks that would run for event and target.
func (r *Runner) Matching(event HookEvent, target string) []HookConfig {
	var out []HookConfig
	for _, cfg := range r.Hooks {
		if cfg.Event != event {
			continue
		}
		if !MatchHook(cfg.Matcher, target) {
			continue
		}
		out = append(out, cfg)
	}
	return out
}

// MatchHook reports whether target matches the glob matcher. An empty matcher
// matches everything; an invalid pattern behaves like an exact comparison.
func MatchHook(matcher, target string) bool {
	if matcher == "" || matcher == "*" {
		return true
	}
	// Tool and agent names are slash-free, but path.Match globs are still a
	// natural fit and support '*', '?' and character classes.
	ok, err := path.Match(matcher, target)
	if err != nil {
		return matcher == target
	}
	return ok
}

// Run executes all hooks matching in.Event in configuration order. Execution
// stops early when a before_tool hook denies the call.
//
// Hooks that exit non-zero are recorded in the results rather than returned as
// an error; an error is only returned when a hook cannot be started or times
// out (the context error is joined in). Callers should inspect
// Blocked(results) / results[i].Allow for policy decisions.
func (r *Runner) Run(ctx context.Context, in HookInput) ([]HookResult, error) {
	if in.Event == "" {
		return nil, errors.New("ext: hook event is required")
	}
	target := in.Target()
	var (
		results []HookResult
		errs    []error
	)
	for _, cfg := range r.Hooks {
		if cfg.Event != in.Event || !MatchHook(cfg.Matcher, target) {
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		res, err := r.runHook(ctx, cfg, in)
		results = append(results, res)
		if err != nil {
			errs = append(errs, err)
		}
		if in.Event == HookBeforeTool && !res.Allow {
			// The call is denied: remaining hooks must not run.
			break
		}
	}
	return results, errors.Join(errs...)
}

// RunBeforeTool runs the before_tool hooks for a tool call and merges their
// results into a single decision. The returned result always carries the event
// and has Allow set to false when any hook denied the call.
func (r *Runner) RunBeforeTool(ctx context.Context, tool string, args json.RawMessage) (HookResult, error) {
	in := HookInput{Event: HookBeforeTool, Tool: tool, Args: args}
	results, err := r.Run(ctx, in)
	merged := HookResult{Event: HookBeforeTool, Allow: true, ExitCode: 0}
	var messages []string
	for _, res := range results {
		if res.Message != "" {
			messages = append(messages, res.Message)
		}
		if !res.Allow {
			merged.Allow = false
			merged.ExitCode = res.ExitCode
			merged.TimedOut = merged.TimedOut || res.TimedOut
		}
	}
	merged.Message = strings.Join(messages, "\n")
	return merged, err
}

func (r *Runner) runHook(ctx context.Context, cfg HookConfig, in HookInput) (HookResult, error) {
	cfg = cfg.WithDefaults()
	res := HookResult{Hook: cfg, Event: in.Event, Allow: true, ExitCode: 0}
	if err := cfg.Validate(); err != nil {
		res.Allow = false
		res.ExitCode = -1
		return res, err
	}

	payload, err := json.Marshal(in)
	if err != nil {
		res.Allow = false
		res.ExitCode = -1
		return res, fmt.Errorf("ext: encode hook input for %s: %w", cfg.Event, err)
	}

	hctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(hctx, cfg.Command, cfg.Args...)
	cmd.Stdin = bytes.NewReader(payload)
	stdout := &lockedBuffer{max: maxHookOutput}
	stderr := &lockedBuffer{max: maxHookOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Dir = r.Dir
	if len(r.Env) > 0 {
		cmd.Env = append(os.Environ(), r.Env...)
	}
	// If the hook process exits but leaves descendants holding the output
	// pipes open, WaitDelay bounds the wait so Run cannot hang.
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	runErr := cmd.Run()
	res.Duration = time.Since(start)
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	res.Message = firstNonEmpty(res.Stdout, res.Stderr)

	timedOut := errors.Is(hctx.Err(), context.DeadlineExceeded)
	res.TimedOut = timedOut

	switch {
	case timedOut:
		res.Allow = false
		res.ExitCode = -1
		res.Message = fmt.Sprintf("hook timed out after %s", cfg.Timeout)
		if res.Stderr != "" {
			res.Message += ": " + res.Stderr
		}
		return res, fmt.Errorf("ext: hook %q for %s timed out after %s: %w",
			cfg.Command, cfg.Event, cfg.Timeout, context.DeadlineExceeded)
	case runErr == nil:
		res.ExitCode = 0
	case errors.Is(runErr, context.Canceled) && ctx.Err() != nil:
		res.Allow = false
		res.ExitCode = -1
		return res, fmt.Errorf("ext: hook %q for %s canceled: %w", cfg.Command, cfg.Event, ctx.Err())
	default:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			if res.ExitCode == 0 {
				// The process was signaled, e.g. killed by the context.
				res.ExitCode = -1
			}
			// A non-zero exit denies before_tool calls but is otherwise
			// recorded without failing the run.
			res.Allow = res.ExitCode == 0
			if !res.Allow && res.Message == "" {
				res.Message = fmt.Sprintf("hook %q exited with code %d", cfg.Command, res.ExitCode)
			}
			return res, nil
		}
		res.Allow = false
		res.ExitCode = -1
		return res, fmt.Errorf("ext: run hook %q for %s: %w", cfg.Command, cfg.Event, runErr)
	}

	return res, nil
}

const maxHookOutput = 64 << 10 // 64 KiB per stream

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// lockedBuffer is a concurrency-safe, size-capped buffer. os/exec writes to
// cmd.Stderr from its own goroutine, so reads must be synchronized.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if b.max > 0 && b.buf.Len() > b.max {
		// Retain the tail of the output (most recent, usually most relevant).
		data := b.buf.Bytes()
		tail := append([]byte(nil), data[len(data)-b.max:]...)
		b.buf.Reset()
		b.buf.Write(tail)
	}
	return n, err
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	if b.max > 0 && len(s) > b.max {
		s = s[len(s)-b.max:]
	}
	return s
}
