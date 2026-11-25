// Package permission implements Tern's tool-permission engine.
//
// The engine answers a single question: may the agent run a particular tool
// call? It combines a coarse permission mode (ask, auto-edit, yolo, plan) with
// an explicit rule set (per-tool, per-path and per-bash-command globs), a
// separate web policy, and a set of built-in deny rules that are always active
// unless explicitly disabled.
//
// Evaluation order is deliberately deny-first so that a broad allow rule can
// never override a deny rule or a built-in safety guard. Sub-agents receive a
// derived evaluator via DeriveChild which inherits the parent's mode and rules
// but can never escalate privileges.
package permission

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode is the coarse permission mode of a session.
type Mode string

// Supported permission modes. ModeAsk is the default.
const (
	// ModeAsk prompts the user before any mutating tool call. Read-only tools
	// are allowed.
	ModeAsk Mode = "ask"
	// ModeAutoEdit allows file writes/edits inside the workspace without
	// prompting; bash and out-of-workspace mutations still ask.
	ModeAutoEdit Mode = "auto-edit"
	// ModeYolo allows everything except explicit deny rules.
	ModeYolo Mode = "yolo"
	// ModePlan is strictly read-only: every write/edit or mutating bash
	// command is denied.
	ModePlan Mode = "plan"
)

// DefaultMode is used when no mode is configured.
const DefaultMode = ModeAsk

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool {
	switch m {
	case ModeAsk, ModeAutoEdit, ModeYolo, ModePlan:
		return true
	}
	return false
}

// String returns the canonical mode name.
func (m Mode) String() string {
	if m == "" {
		return string(DefaultMode)
	}
	return string(m)
}

// Rank returns the permissiveness of a mode, from most restrictive (plan = 0)
// to most permissive (yolo = 3). Unknown modes rank with ask so that they can
// never escalate anything.
func (m Mode) Rank() int {
	switch m {
	case ModePlan:
		return 0
	case ModeYolo:
		return 3
	case ModeAutoEdit:
		return 2
	default:
		return 1
	}
}

// ParseMode parses a configured mode string. An empty string yields the
// default mode. Unknown values are an error.
func ParseMode(s string) (Mode, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return DefaultMode, nil
	}
	m := Mode(s)
	if !m.Valid() {
		return DefaultMode, fmt.Errorf("permission: unknown mode %q (want ask, auto-edit, yolo or plan)", s)
	}
	return m, nil
}

// WebPolicy controls network tools (web_fetch, web_search). It is kept
// separate from Mode because network access is a distinct risk category.
type WebPolicy string

// Supported web policies.
const (
	WebPolicyAsk   WebPolicy = "ask"
	WebPolicyAllow WebPolicy = "allow"
	WebPolicyDeny  WebPolicy = "deny"
)

// Aliases for readability at call sites.
const (
	WebAsk   = WebPolicyAsk
	WebAllow = WebPolicyAllow
	WebDeny  = WebPolicyDeny
)

// DefaultWebPolicy is used when no web policy is configured.
const DefaultWebPolicy = WebPolicyAsk

// Valid reports whether w is a known web policy.
func (w WebPolicy) Valid() bool {
	switch w {
	case WebPolicyAsk, WebPolicyAllow, WebPolicyDeny:
		return true
	}
	return false
}

// String returns the canonical policy name.
func (w WebPolicy) String() string {
	if w == "" {
		return string(DefaultWebPolicy)
	}
	return string(w)
}

// ParseWebPolicy parses a configured web policy. An empty string yields the
// default policy. Unknown values are an error.
func ParseWebPolicy(s string) (WebPolicy, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return DefaultWebPolicy, nil
	}
	w := WebPolicy(s)
	if !w.Valid() {
		return DefaultWebPolicy, fmt.Errorf("permission: unknown web policy %q (want ask, allow or deny)", s)
	}
	return w, nil
}

// DecisionKind is the outcome of a permission check.
type DecisionKind string

// Possible permission outcomes.
const (
	// DecisionAllow lets the tool call proceed without user interaction.
	DecisionAllow DecisionKind = "allow"
	// DecisionDeny blocks the tool call outright.
	DecisionDeny DecisionKind = "deny"
	// DecisionAsk requires explicit user confirmation.
	DecisionAsk DecisionKind = "ask"
)

// String returns the canonical decision name.
func (k DecisionKind) String() string { return string(k) }

// Decision is the result of Evaluator.Check: an outcome plus a human-readable
// explanation that can be shown to the user or logged.
type Decision struct {
	Kind   DecisionKind
	Reason string
}

// Allow builds an allow decision.
func Allow(reason string) Decision { return Decision{Kind: DecisionAllow, Reason: reason} }

// Deny builds a deny decision.
func Deny(reason string) Decision { return Decision{Kind: DecisionDeny, Reason: reason} }

// Ask builds an ask decision.
func Ask(reason string) Decision { return Decision{Kind: DecisionAsk, Reason: reason} }

// IsAllow reports whether d allows the call.
func (d Decision) IsAllow() bool { return d.Kind == DecisionAllow }

// IsDeny reports whether d denies the call.
func (d Decision) IsDeny() bool { return d.Kind == DecisionDeny }

// IsAsk reports whether d requires confirmation.
func (d Decision) IsAsk() bool { return d.Kind == DecisionAsk }

// String renders a decision for logs and tests.
func (d Decision) String() string {
	if d.Reason == "" {
		return string(d.Kind)
	}
	return string(d.Kind) + ": " + d.Reason
}

// CheckRequest describes one tool call to evaluate.
//
// Only the fields relevant to the tool need to be set: Path for file tools,
// Command for bash, IsWeb for web_fetch/web_search. IsReadOnly should be true
// for tools that cannot mutate local state (read, grep, glob, web, ...).
type CheckRequest struct {
	ToolName   string
	IsReadOnly bool
	IsWeb      bool
	Path       string
	Command    string
	WorkDir    string
	AgentName  string
}

// Policy is the declarative rule set for permission evaluation.
//
// All glob lists are matched case-sensitively. Tool entries support shell
// style wildcards (for example "mcp_*"). Path entries support "~" expansion,
// "*" (does not cross a path separator) and "**" (crosses separators). Bash
// entries match whole command segments with "*" matching any characters.
type Policy struct {
	Mode Mode
	Web  WebPolicy

	AllowTools []string
	DenyTools  []string

	AllowPaths []string
	DenyPaths  []string

	AllowBash []string
	DenyBash  []string

	// WorkDir is the workspace root used to resolve relative paths and to
	// decide whether an auto-edit write stays inside the workspace.
	WorkDir string

	// DisableDefaultDeny turns off the built-in deny rules. It exists for
	// tests and for embedding the policy in a sandbox that enforces its own
	// guards; it should not be enabled for normal sessions.
	DisableDefaultDeny bool
}

func (p Policy) normalized() Policy {
	if !p.Mode.Valid() {
		p.Mode = DefaultMode
	}
	if !p.Web.Valid() {
		p.Web = DefaultWebPolicy
	}
	return p
}

// Evaluator evaluates tool calls against a Policy. It is safe for concurrent
// use because it holds only immutable state.
type Evaluator struct {
	policy Policy
}

// New creates an Evaluator from a Policy, filling in defaults for zero values.
func New(p Policy) *Evaluator {
	return &Evaluator{policy: p.normalized()}
}

// NewEvaluator is an alias for New.
func NewEvaluator(p Policy) *Evaluator { return New(p) }

// Policy returns a copy of the evaluator's policy.
func (e *Evaluator) Policy() Policy {
	if e == nil {
		return Policy{}.normalized()
	}
	return e.policy
}

// Mode returns the evaluator's effective mode.
func (e *Evaluator) Mode() Mode { return e.Policy().Mode }

// WebPolicy returns the evaluator's effective web policy.
func (e *Evaluator) WebPolicy() WebPolicy { return e.Policy().Web }

// Default returns an evaluator with default settings (ask mode, web ask).
func Default() *Evaluator { return New(Policy{}) }

// WithMode returns a copy of the evaluator with a different mode. The new mode
// is clamped to be no more permissive than the receiver's so that this helper
// cannot be used to escalate.
func (e *Evaluator) WithMode(m Mode) *Evaluator {
	p := e.Policy()
	if m.Valid() && m.Rank() < p.Mode.Rank() {
		p.Mode = m
	}
	return New(p)
}

// WithWorkDir returns a copy of the evaluator with a different workspace root.
func (e *Evaluator) WithWorkDir(dir string) *Evaluator {
	p := e.Policy()
	p.WorkDir = dir
	return New(p)
}

// Check evaluates a tool call and returns a Decision. The first matching rule
// wins, in this order: explicit denies, the auto-allowed task tool, built-in
// denies, web policy, plan restrictions, explicit allows, read-only shortcuts,
// and finally the mode's default behaviour.
func (e *Evaluator) Check(req CheckRequest) Decision {
	if e == nil {
		e = Default()
	}
	p := e.policy
	mode := p.Mode
	workDir := firstNonEmpty(req.WorkDir, p.WorkDir)
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	decide := func(d Decision) Decision {
		if req.AgentName != "" {
			d.Reason = fmt.Sprintf("agent %q: %s", req.AgentName, d.Reason)
		}
		return d
	}

	tool := strings.TrimSpace(req.ToolName)
	path := strings.TrimSpace(req.Path)
	command := strings.TrimSpace(req.Command)

	// 1. Explicit denies always win.
	if tool != "" {
		if pat, ok := matchAnyGlob(p.DenyTools, tool); ok {
			return decide(Deny(fmt.Sprintf("tool %q is denied by rule %q", tool, pat)))
		}
	}
	if path != "" {
		if pat, ok := matchAnyPath(p.DenyPaths, path, workDir); ok {
			return decide(Deny(fmt.Sprintf("path %q is denied by rule %q", path, pat)))
		}
	}
	if command != "" {
		if pat, ok := matchAnyBash(p.DenyBash, command); ok {
			return decide(Deny(fmt.Sprintf("command %q is denied by rule %q", command, pat)))
		}
	}

	// 2. The task tool only orchestrates sub-agents. Every tool those agents
	// run is checked separately by DeriveChild, and plan mode forces them
	// read-only, so delegating never needs a prompt of its own. This sits
	// after the explicit deny checks above so denying "task" still wins.
	if tool == "task" {
		return decide(Allow("sub-agent tool calls are checked by the child permission evaluator"))
	}

	// 3. Built-in safety rules (destructive bash, sensitive write paths).
	if !p.DisableDefaultDeny {
		if command != "" {
			if reason, ok := DefaultBashDenyReason(command); ok {
				return decide(Deny(reason))
			}
		}
		if path != "" && !req.IsReadOnly {
			if pat, ok := matchAnyPath(DefaultDenyWritePaths, path, workDir); ok {
				return decide(Deny(fmt.Sprintf("refusing to modify sensitive path %q (rule %q)", path, pat)))
			}
		}
	}

	// 4. Web tools follow the dedicated web policy.
	if req.IsWeb {
		switch p.Web {
		case WebPolicyAllow:
			return decide(Allow(fmt.Sprintf("web access allowed by policy for %q", tool)))
		case WebPolicyDeny:
			return decide(Deny(fmt.Sprintf("web access denied by policy for %q", tool)))
		default:
			if mode == ModeYolo {
				return decide(Allow(fmt.Sprintf("yolo mode allows web access for %q", tool)))
			}
			return decide(Ask(fmt.Sprintf("web access for %q requires confirmation", tool)))
		}
	}

	// 5. Plan mode is strictly read-only.
	if mode == ModePlan {
		if command != "" {
			if !IsReadOnlyBashCommand(command) {
				return decide(Deny(fmt.Sprintf("plan mode is read-only; command %q may mutate state", command)))
			}
		} else if !req.IsReadOnly {
			return decide(Deny(fmt.Sprintf("plan mode is read-only; tool %q may mutate state", tool)))
		}
	}

	// 6. Explicit allows.
	if tool != "" {
		if pat, ok := matchAnyGlob(p.AllowTools, tool); ok {
			return decide(Allow(fmt.Sprintf("tool %q is allowed by rule %q", tool, pat)))
		}
	}
	if command != "" {
		if pat, ok := matchAnyBash(p.AllowBash, command); ok {
			return decide(Allow(fmt.Sprintf("command %q is allowed by rule %q", command, pat)))
		}
	}
	if path != "" {
		if pat, ok := matchAnyPath(p.AllowPaths, path, workDir); ok {
			return decide(Allow(fmt.Sprintf("path %q is allowed by rule %q", path, pat)))
		}
	}

	// 7. Read-only operations never prompt outside plan-denied territory.
	if req.IsReadOnly {
		return decide(Allow(fmt.Sprintf("read-only tool %q", tool)))
	}
	if command != "" && IsReadOnlyBashCommand(command) {
		return decide(Allow(fmt.Sprintf("read-only command %q", command)))
	}

	// 8. Mode defaults.
	switch mode {
	case ModeYolo:
		return decide(Allow("yolo mode allows all non-denied tool calls"))
	case ModeAutoEdit:
		if path != "" && command == "" && isInsideWorkspace(path, workDir) {
			return decide(Allow(fmt.Sprintf("%q is inside the workspace and auto-edit mode allows edits there", path)))
		}
		if command != "" {
			return decide(Ask(fmt.Sprintf("auto-edit mode still asks before running %q", command)))
		}
		return decide(Ask(fmt.Sprintf("auto-edit mode asks before mutating %q", firstNonEmpty(path, tool))))
	default:
		if command != "" {
			return decide(Ask(fmt.Sprintf("ask mode requires confirmation before running %q", command)))
		}
		return decide(Ask(fmt.Sprintf("ask mode requires confirmation before %q", firstNonEmpty(tool, "this action"))))
	}
}

// DeriveChild builds the evaluator for a sub-agent.
//
// The child inherits the parent's mode, web policy, allow/deny rules, workspace
// and default-denial setting, and can never escalate:
//
//	plan < ask < auto-edit < yolo
//
// overrideMode may only restrict the parent's mode; empty means "inherit". An
// unknown override is ignored. forceReadOnly always yields plan mode, which is
// itself read-only. A nil parent yields a default (ask) evaluator.
func DeriveChild(parent *Evaluator, overrideMode string, forceReadOnly bool) *Evaluator {
	p := parent.Policy()

	mode := p.Mode
	if m, err := ParseMode(overrideMode); err == nil && strings.TrimSpace(overrideMode) != "" {
		if m.Rank() < mode.Rank() {
			mode = m
		}
	}
	if forceReadOnly {
		mode = ModePlan
	}
	p.Mode = mode
	return New(p)
}

// firstNonEmpty returns the first non-empty, trimmed string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// isInsideWorkspace reports whether path resolves to workDir or one of its
// descendants.
func isInsideWorkspace(path, workDir string) bool {
	if workDir == "" {
		return false
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return false
	}
	absPath := expandHome(path)
	if !isPathAbs(absPath) {
		absPath = filepath.Join(absWork, absPath)
	}
	absPath = filepath.Clean(absPath)
	rel, err := filepath.Rel(absWork, absPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
