package subagent

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tern-agent/tern/internal/provider"
)

// StepView is one rendered tool step inside a sub-agent block.
type StepView struct {
	Tool   string
	Detail string
	Status string
}

// AgentView is the render model for one sub-agent block.
type AgentView struct {
	ID          string
	AgentType   string
	Description string
	Status      string
	Step        int
	CurrentTool string
	Steps       []StepView
	Usage       provider.Usage
	CostUSD     float64
	Elapsed     time.Duration
	Error       string
	Summary     string
}

// RenderAgent renders a single sub-agent block. When expanded is false only a
// one-line summary is produced.
func RenderAgent(v AgentView, expanded bool) string {
	return renderView(v, expanded)
}

// Render renders a single sub-agent block using the block's status to decide
// whether detail is useful: terminal failures and approvals stay expanded;
// successful agents collapse.
func Render(v AgentView) string {
	expanded := v.Status != StatusDone
	return renderView(v, expanded)
}

// RenderGroup renders a stacked group of sub-agent blocks. Each block is
// separated by one blank line. Mixed expanded/collapsed states are supported
// through expandedByID.
func RenderGroup(views []AgentView, expanded bool) string {
	blocks := make([]string, 0, len(views))
	for _, v := range views {
		blocks = append(blocks, renderView(v, expanded))
	}
	return strings.Join(blocks, "\n\n")
}

// RenderBlocks is an alias for RenderGroup.
func RenderBlocks(views []AgentView, expanded bool) string {
	return RenderGroup(views, expanded)
}

func renderView(v AgentView, expanded bool) string {
	agentType := strings.TrimSpace(v.AgentType)
	if agentType == "" {
		agentType = "agent"
	}
	description := strings.TrimSpace(v.Description)
	if description == "" {
		description = "sub-agent"
	}
	header := fmt.Sprintf("▸ %s · %q", agentType, description)
	status := normalizeRenderStatus(v.Status)

	if !expanded {
		parts := []string{header}
		if v.Step > 0 || len(v.Steps) > 0 {
			parts = append(parts, fmt.Sprintf("%d steps", effectiveSteps(v)))
		}
		parts = append(parts, status)
		if s := statusSuffix(v); s != "" {
			parts = append(parts, s)
		}
		return strings.Join(parts, " · ")
	}

	var b strings.Builder
	b.WriteString(header)
	if right := statusRight(v, status); right != "" {
		pad := 56 - displayWidth(header) - displayWidth(right)
		if pad < 2 {
			pad = 2
		}
		b.WriteString(strings.Repeat(" ", pad))
		b.WriteString(right)
	}
	b.WriteString("\n")

	steps := v.Steps
	if status == StatusWaitingApproval && v.CurrentTool != "" && !hasTool(steps, v.CurrentTool) {
		steps = append(steps, StepView{Tool: v.CurrentTool, Detail: "approval required", Status: StatusWaitingApproval})
	}
	if len(steps) == 0 && v.CurrentTool != "" {
		steps = []StepView{{Tool: v.CurrentTool, Status: status}}
	}
	for i, step := range steps {
		branch := "├"
		if i == len(steps)-1 {
			branch = "└"
		}
		tool := strings.TrimSpace(step.Tool)
		detail := strings.TrimSpace(step.Detail)
		label := tool
		if detail != "" {
			label += " " + detail
		}
		stepStatus := strings.TrimSpace(step.Status)
		if stepStatus == "" {
			stepStatus = StatusDone
		}
		switch stepStatus {
		case StatusRunning:
			stepStatus = "running…"
		case StatusWaitingApproval:
			stepStatus = "waiting…"
		}
		padding := 36 - displayWidth(label)
		if padding < 2 {
			padding = 2
		}
		fmt.Fprintf(&b, "  %s %s%s%s\n", branch, label, strings.Repeat(" ", padding), stepStatus)
	}

	if status == StatusFailed && v.Error != "" {
		fmt.Fprintf(&b, "  ! %s\n", oneLine(v.Error))
	} else if status == StatusCancelled && v.Summary != "" {
		fmt.Fprintf(&b, "  ⊘ %s\n", oneLine(v.Summary))
	}

	fmt.Fprintf(&b, "  %s\n", statsLine(v))
	return strings.TrimRight(b.String(), "\n")
}

func statusRight(v AgentView, status string) string {
	switch status {
	case StatusQueued:
		return "queued"
	case StatusRunning:
		return "⠋ " + formatElapsed(v.Elapsed)
	case StatusWaitingApproval:
		if v.Elapsed > 0 {
			return "⚠ approval · " + formatElapsed(v.Elapsed)
		}
		return "⚠ approval"
	case StatusDone:
		return "✓ " + formatElapsed(v.Elapsed)
	case StatusFailed:
		return "✗ failed"
	case StatusCancelled:
		return "⊘ cancelled"
	case StatusLimitReached:
		return "! limit reached"
	default:
		return status
	}
}

func statusSuffix(v AgentView) string {
	if v.Elapsed <= 0 {
		return ""
	}
	switch normalizeRenderStatus(v.Status) {
	case StatusQueued:
		return "queued"
	case StatusRunning, StatusWaitingApproval:
		return formatElapsed(v.Elapsed)
	default:
		return formatElapsed(v.Elapsed)
	}
}

func normalizeRenderStatus(status string) string {
	switch strings.TrimSpace(status) {
	case StatusQueued, StatusRunning, StatusWaitingApproval, StatusDone, StatusFailed, StatusCancelled, StatusLimitReached:
		return strings.TrimSpace(status)
	case "":
		return StatusRunning
	default:
		return StatusRunning
	}
}

func statsLine(v AgentView) string {
	steps := effectiveSteps(v)
	tokens := v.Usage.TotalTokens
	if tokens == 0 {
		tokens = v.Usage.PromptTokens + v.Usage.CompletionTokens
	}
	parts := []string{
		fmt.Sprintf("%d steps", steps),
		formatTokens(tokens) + " tok",
	}
	if cache := cachePercent(v.Usage); cache >= 0 {
		parts = append(parts, fmt.Sprintf("cache %d%%", cache))
	}
	parts = append(parts, formatCost(v.CostUSD))
	return strings.Join(parts, " · ")
}

func effectiveSteps(v AgentView) int {
	if v.Step > len(v.Steps) {
		return v.Step
	}
	return len(v.Steps)
}

func hasTool(steps []StepView, tool string) bool {
	for _, step := range steps {
		if step.Tool == tool {
			return true
		}
	}
	return false
}

func cachePercent(u provider.Usage) int {
	total := u.PromptCacheHitTokens + u.PromptCacheMissTokens
	if total <= 0 {
		return -1
	}
	return int(float64(u.PromptCacheHitTokens)/float64(total)*100 + 0.5)
}

func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000_000)) + "M"
	case n >= 1_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000)) + "k"
	default:
		return fmt.Sprintf("%d", n)
	}
}

func trimZero(s string) string {
	s = strings.TrimSuffix(s, ".0")
	return s
}

func formatCost(cost float64) string {
	if cost <= 0 {
		return "$0.000"
	}
	if cost < 0.01 {
		s := strings.TrimRight(fmt.Sprintf("%.4f", cost), "0")
		s = strings.TrimSuffix(s, ".")
		// Keep at least three decimal places for consistency.
		for len(strings.TrimPrefix(s, "0.")) < 3 {
			s += "0"
		}
		return "$" + s
	}
	if cost < 1 {
		return "$" + strings.TrimRight(fmt.Sprintf("%.3f", cost), "0")
	}
	return "$" + fmt.Sprintf("%.2f", cost)
}

func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	minutes := int(d / time.Minute)
	seconds := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%02ds", minutes, seconds)
}

func displayWidth(s string) int {
	// The renderer only emits ASCII decoration plus wide runes from the fixed
	// symbol set; rune count is a sufficiently accurate alignment estimate.
	return len([]rune(s))
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// NoColor reports whether colored output is disabled. The renderer currently
// emits plain text only, which makes NO_COLOR trivially correct; this helper
// exists so callers and tests can assert the policy.
func NoColor() bool {
	_, set := os.LookupEnv("NO_COLOR")
	return set
}
