// Package tools implements the built-in tools available to tern agents.
//
// Every tool exposes a strict JSON schema for its arguments and reports a
// small set of behavioural flags (read-only, web access, parallel safety) that
// the permission and scheduling layers use. Results carry untrusted-output
// and metadata markers so callers can distinguish local edits from external
// content.
//
// The package only depends on the Go standard library and internal/provider.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/tern-agent/tern/internal/provider"
)

// Built-in tool names.
const (
	ToolRead      = "read"
	ToolWrite     = "write"
	ToolEdit      = "edit"
	ToolMultiEdit = "multi_edit"
	ToolLS        = "ls"
	ToolGlob      = "glob"
	ToolGrep      = "grep"
	ToolBash      = "bash"
	ToolTodo      = "todo"
	ToolAskUser   = "ask_user"
)

// Defaults shared by the built-in tools.
const (
	// DefaultOutputCap is the number of characters a single tool result may
	// carry before it is truncated.
	DefaultOutputCap = 32768
	// DefaultReadLimit is the number of lines read when no limit is given.
	DefaultReadLimit = 200
	// DefaultGrepMaxResults is the default cap on grep matches.
	DefaultGrepMaxResults = 200
	// MaxGlobMatches caps the number of paths returned by glob.
	MaxGlobMatches = 500
)

// Tool is a callable function exposed to the model.
type Tool interface {
	// Name is the stable tool identifier used in tool calls.
	Name() string
	// Description is the human-readable purpose shown to the model.
	Description() string
	// Parameters is a strict JSON Schema object description.
	Parameters() json.RawMessage
	// IsReadOnly reports whether the tool never mutates the workspace.
	IsReadOnly() bool
	// IsWeb reports whether the tool reaches the public network.
	IsWeb() bool
	// IsParallelSafe reports whether the tool may run concurrently with other
	// parallel-safe tools.
	IsParallelSafe() bool
	// Execute runs the tool with raw JSON arguments.
	Execute(ctx context.Context, env ExecEnv, args json.RawMessage) (Result, error)
}

// Result is the outcome of a tool invocation.
type Result struct {
	// Output is the textual result shown to the model.
	Output string
	// IsError marks an output that reports a failed operation (for example a
	// non-zero shell exit) while still being valid tool output.
	IsError bool
	// External marks output derived from untrusted external sources (web
	// content, remote APIs). Callers should treat such text as data, never as
	// instructions.
	External bool
	// Metadata carries structured details such as "files_read" or
	// "files_changed".
	Metadata map[string]any
}

// ExecEnv is the ambient environment passed to every tool invocation.
type ExecEnv struct {
	// WorkDir is the workspace root used to resolve relative paths.
	WorkDir string
	// Shell overrides the shell used by bash (for example "bash" or "zsh").
	Shell string
	// OutputCap is the per-result character budget; zero means
	// DefaultOutputCap.
	OutputCap int
	// BeforeEdit is invoked with the absolute path before a tool mutates a
	// file. It is used for checkpointing and may abort the edit by returning
	// an error.
	BeforeEdit func(path string) error
	// AskUser asks the user a question and returns their answer. It is nil in
	// non-interactive sessions.
	AskUser func(ctx context.Context, question string, options []string) (string, error)
	// OnTodoUpdate observes changes to the todo list.
	OnTodoUpdate func(items []TodoItem)
}

// WorkingDir returns WorkDir, defaulting to the current directory.
func (e ExecEnv) WorkingDir() string {
	if e.WorkDir == "" {
		return "."
	}
	return e.WorkDir
}

// OutputLimit returns the effective result character budget.
func (e ExecEnv) OutputLimit() int {
	if e.OutputCap <= 0 {
		return DefaultOutputCap
	}
	return e.OutputCap
}

// Truncate applies TruncateOnce with the environment's output limit.
func (e ExecEnv) Truncate(s string) string {
	return TruncateOnce(s, e.OutputLimit())
}

// Registry holds a set of tools by name. It is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewEmptyRegistry returns a registry with no tools.
func NewEmptyRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// NewRegistry returns a registry containing all built-in tools.
//
// The returned registry owns a background job manager; callers should invoke
// Close when the registry is no longer needed.
func NewRegistry() *Registry {
	r := NewEmptyRegistry()
	for _, t := range BuiltinTools() {
		r.Register(t)
	}
	return r
}

// BuiltinTools returns a fresh instance of every built-in tool.
func BuiltinTools() []Tool {
	return []Tool{
		NewReadTool(),
		NewWriteTool(),
		NewEditTool(),
		NewMultiEditTool(),
		NewLSTool(),
		NewGlobTool(),
		NewGrepTool(false),
		NewBashTool(),
		NewTodoTool(),
		NewAskUserTool(),
	}
}

// Register adds or replaces a tool. It panics on a nil or unnamed tool, which
// is a programming error.
func (r *Registry) Register(t Tool) {
	if t == nil {
		panic("tools: cannot register a nil tool")
	}
	name := t.Name()
	if name == "" {
		panic("tools: cannot register a tool with an empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[name] = t
}

// Get returns the named tool.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Subset returns a new registry containing only the named tools, preserving
// the requested order for registration. Unknown names are ignored.
func (r *Registry) Subset(names []string) *Registry {
	out := NewEmptyRegistry()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, name := range names {
		if t, ok := r.tools[name]; ok {
			out.Register(t)
		}
	}
	return out
}

// Without returns a new registry excluding the named tools.
func (r *Registry) Without(names ...string) *Registry {
	excluded := make(map[string]struct{}, len(names))
	for _, name := range names {
		excluded[name] = struct{}{}
	}
	out := NewEmptyRegistry()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for name, t := range r.tools {
		if _, skip := excluded[name]; skip {
			continue
		}
		out.Register(t)
	}
	return out
}

// Names returns the registered tool names sorted lexicographically.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ToolDefs returns provider tool definitions sorted deterministically by
// name. Stable ordering matters for prompt-prefix caching.
func (r *Registry) ToolDefs() []provider.ToolDef {
	r.mu.RLock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	tools := make([]Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, r.tools[name])
	}
	r.mu.RUnlock()

	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })

	defs := make([]provider.ToolDef, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, provider.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return defs
}

// Close releases resources owned by registered tools. Tools implementing
// io.Closer (currently bash/job management) are closed in name order.
func (r *Registry) Close() error {
	r.mu.RLock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	tools := make([]Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, r.tools[name])
	}
	r.mu.RUnlock()

	var errs []error
	for _, t := range tools {
		if c, ok := t.(interface{ Close() error }); ok {
			if err := c.Close(); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
			}
		}
	}
	return errors.Join(errs...)
}

// TruncateOnce truncates s to at most maxChars characters and appends a
// marker describing the truncation. It is idempotent for content that already
// fits. A non-positive maxChars leaves s untouched.
//
// The marker is:
//
//	\n[... truncated: showing X of Y chars ...]
func TruncateOnce(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	shown := maxChars
	if shown < 0 {
		shown = 0
	}
	if shown > len(runes) {
		shown = len(runes)
	}
	return string(runes[:shown]) + fmt.Sprintf("\n[... truncated: showing %d of %d chars ...]", shown, len(runes))
}

// schema validates a JSON schema literal at init time and returns it as raw
// JSON. Invalid literals indicate a programming error.
func schema(raw string) json.RawMessage {
	if !utf8.ValidString(raw) || !json.Valid([]byte(raw)) {
		panic("tools: invalid JSON schema literal")
	}
	return json.RawMessage(raw)
}

// decodeArgs decodes raw tool arguments, rejecting unknown fields so that the
// strict schemas stay honest. Empty and null arguments decode to the zero
// value.
func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}
