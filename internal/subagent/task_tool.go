package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tern-agent/tern/internal/tools"
)

// TaskTool is the parent agent's "task" tool. It delegates work to a sub-agent
// and returns that agent's structured report as an external tool result.
type TaskTool struct {
	Manager *Manager
}

// NewTaskTool returns a task tool backed by manager.
func NewTaskTool(manager *Manager) *TaskTool { return &TaskTool{Manager: manager} }

func (t *TaskTool) Name() string         { return ToolTask }
func (t *TaskTool) IsReadOnly() bool     { return false }
func (t *TaskTool) IsWeb() bool          { return false }
func (t *TaskTool) IsParallelSafe() bool { return true }

var taskSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "description": {"type": "string", "description": "Short 3-8 word label shown in the UI."},
    "prompt": {"type": "string", "description": "Complete, self-contained instructions for the sub-agent."},
    "agent": {"type": "string", "description": "Agent type name. Defaults to general."},
    "model": {"type": "string", "description": "Optional model override."},
    "tools": {"type": "array", "items": {"type": "string"}, "description": "Optional subset of the agent type's tools."},
    "max_steps": {"type": "integer", "minimum": 1, "description": "Optional step limit for this agent."},
    "isolated": {"type": "boolean", "description": "Run a write agent against a temporary workspace copy and return a patch."}
  },
  "required": ["description", "prompt"],
  "additionalProperties": false
}`)

func (t *TaskTool) Parameters() json.RawMessage { return taskSchema }

func (t *TaskTool) Description() string {
	infos := t.agentInfos()
	var b strings.Builder
	b.WriteString("Delegate a focused task to a sub-agent. Use this when the work is self-contained, would otherwise flood the main context, or benefits from parallel execution.\n\n")
	b.WriteString("Delegate when:\n")
	b.WriteString("- You need to explore many files just to answer one question.\n")
	b.WriteString("- You need web research with sources.\n")
	b.WriteString("- An implementation is independent and can be described completely up front.\n")
	b.WriteString("- You want an independent review of a diff or file set.\n")
	b.WriteString("- Several independent tasks can run at the same time (multiple task calls in one turn run concurrently).\n\n")
	b.WriteString("Do NOT delegate when:\n")
	b.WriteString("- The work needs information only you have from the ongoing conversation (unless you include it in the prompt).\n")
	b.WriteString("- A quick direct read, grep or edit is enough.\n")
	b.WriteString("- The task depends on another sub-agent's result; wait for that result first.\n")
	b.WriteString("- You need to ask the user a question.\n\n")
	b.WriteString("The prompt must be completely self-contained: the sub-agent does not see this conversation. Include goals, constraints, relevant file paths, and what the final report must contain. The description is only a short UI label.\n\n")
	b.WriteString("Available agents (sorted):\n")
	for _, info := range infos {
		fmt.Fprintf(&b, "- %s: %s\n", info.Name, info.Description)
	}
	b.WriteString("\nSet isolated=true for write-capable agents when you want their changes returned as a unified patch instead of applied directly to the workspace.")
	return b.String()
}

func (t *TaskTool) agentInfos() []AgentInfo {
	if t.Manager == nil {
		return AgentInfos(BuiltinDefinitions())
	}
	defs, err := t.Manager.Definitions()
	if err != nil || len(defs) == 0 {
		defs = BuiltinDefinitions()
	}
	infos := AgentInfos(defs)
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos
}

type taskArgs struct {
	Description string   `json:"description"`
	Prompt      string   `json:"prompt"`
	Agent       string   `json:"agent"`
	Model       string   `json:"model"`
	Tools       []string `json:"tools"`
	MaxSteps    int      `json:"max_steps"`
	Isolated    bool     `json:"isolated"`
}

func (t *TaskTool) Execute(ctx context.Context, env tools.ExecEnv, raw json.RawMessage) (tools.Result, error) {
	if t.Manager == nil {
		return tools.Result{}, fmt.Errorf("task: no sub-agent manager configured")
	}
	var args taskArgs
	if err := strictJSON(raw, &args); err != nil {
		return tools.Result{}, fmt.Errorf("task: invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Description) == "" {
		return tools.Result{}, fmt.Errorf(`task: missing required argument "description"`)
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return tools.Result{}, fmt.Errorf(`task: missing required argument "prompt"`)
	}
	agent := strings.TrimSpace(args.Agent)
	if agent == "" {
		agent = AgentGeneral
	}

	task := Task{
		Description:      strings.TrimSpace(args.Description),
		Prompt:           args.Prompt,
		Agent:            agent,
		Model:            strings.TrimSpace(args.Model),
		Tools:            append([]string(nil), args.Tools...),
		MaxSteps:         args.MaxSteps,
		Isolated:         args.Isolated,
		ParentToolCallID: ParentToolCallIDFromContext(ctx),
		Depth:            DepthFromContext(ctx),
		WorkDir:          env.WorkDir,
	}
	report := t.Manager.Run(ctx, task)
	output := formatReport(report)
	limit := env.OutputLimit()
	if limit <= 0 {
		limit = tools.DefaultOutputCap
	}
	output = tools.TruncateOnce(output, limit)
	return tools.Result{
		Output:   output,
		External: true,
		IsError:  report.Status == StatusFailed,
		Metadata: map[string]any{
			"agent_id":      report.AgentID,
			"agent_type":    report.AgentType,
			"status":        report.Status,
			"steps":         report.Steps,
			"files_read":    report.FilesRead,
			"files_changed": report.FilesChanged,
			"cost_usd":      report.CostUSD,
			"usage":         report.Usage,
			"duration_ms":   report.Duration.Milliseconds(),
		},
	}, nil
}

func formatReport(report AgentReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sub-agent %s (%s) · %s\n", report.AgentID, report.AgentType, report.Status)
	fmt.Fprintf(&b, "Steps: %d · tokens: %d · cost: $%.6f · duration: %s\n", report.Steps, report.Usage.TotalTokens, report.CostUSD, report.Duration.Round(1e6))
	if report.Error != "" {
		fmt.Fprintf(&b, "Error: %s\n", report.Error)
	}
	b.WriteString("\nSummary:\n")
	b.WriteString(strings.TrimSpace(report.Summary))
	b.WriteString("\n")
	if len(report.FilesRead) > 0 {
		b.WriteString("\nFiles read:\n")
		for _, f := range report.FilesRead {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if len(report.FilesChanged) > 0 {
		b.WriteString("\nFiles changed:\n")
		for _, f := range report.FilesChanged {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if report.Patch != "" {
		b.WriteString("\nPatch:\n")
		b.WriteString(report.Patch)
		if !strings.HasSuffix(report.Patch, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Parent turn context
// ---------------------------------------------------------------------------

type taskContextKey int

const (
	parentToolCallIDKey taskContextKey = iota
	depthKey
)

// WithParentToolCallID records the parent tool call id in ctx.
func WithParentToolCallID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, parentToolCallIDKey, id)
}

// ParentToolCallIDFromContext returns the recorded parent tool call id.
func ParentToolCallIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(parentToolCallIDKey).(string)
	return id
}

// WithDepth records the current agent depth in ctx.
func WithDepth(ctx context.Context, depth int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, depthKey, depth)
}

// DepthFromContext returns the recorded agent depth.
func DepthFromContext(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	depth, _ := ctx.Value(depthKey).(int)
	return depth
}
