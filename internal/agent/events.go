package agent

import (
	"encoding/json"

	"github.com/tern-agent/tern/internal/provider"
)

// EventType discriminates agent events emitted through Agent.OnEvent.
type EventType string

// Event types emitted by the agent loop. They are the wire format for the
// CLI's --json mode and for --rpc integrations.
const (
	EventTurnStart      EventType = "turn_start"
	EventReasoningDelta EventType = "reasoning_delta"
	EventTextDelta      EventType = "text_delta"
	EventToolStart      EventType = "tool_start"
	EventToolEnd        EventType = "tool_end"
	EventUsage          EventType = "usage"
	EventCompact        EventType = "compact"
	EventTurnEnd        EventType = "turn_end"
	EventError          EventType = "error"
	EventCacheDebug     EventType = "cache_debug"
)

// Event is a single observable agent event.
//
// One struct carries every event kind so JSON consumers get a stable envelope
// with a "type" discriminator. Fields that do not apply to an event are
// omitted. The struct is safe to marshal to JSON directly.
type Event struct {
	Type EventType `json:"type"`

	// Turn and Step locate the event in the agent loop.
	Turn int `json:"turn,omitempty"`
	Step int `json:"step,omitempty"`

	// Text is the streamed reasoning or assistant text.
	Text string `json:"text,omitempty"`

	// Tool fields.
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Output     string          `json:"output,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	External   bool            `json:"external,omitempty"`
	// DurationMS is kept for source compatibility; emit copies it into
	// DurationMs, which is the field marshaled to JSON.
	DurationMS int64 `json:"-"`
	DurationMs int64 `json:"duration_ms,omitempty"`

	// Usage and accounting fields.
	Usage        *provider.Usage `json:"usage,omitempty"`
	CostUSD      float64         `json:"cost_usd,omitempty"`
	TotalCostUSD float64         `json:"total_cost_usd,omitempty"`
	CacheHitRate float64         `json:"cache_hit_rate,omitempty"`
	TokensPerSec float64         `json:"tokens_per_sec,omitempty"`

	// Cache verification fields (cache_debug).
	PrefixHash      string `json:"prefix_hash,omitempty"`
	SystemToolsHash string `json:"system_tools_hash,omitempty"`

	// Compact carries details for a compaction event.
	Compact *CompactInfo `json:"compact,omitempty"`

	// Reason is a short machine-readable explanation (stop reason, denial).
	Reason string `json:"reason,omitempty"`
	// Message is human-readable detail, also used for error events.
	Message string `json:"message,omitempty"`
	// Diff is an optional unified diff preview attached to approvals.
	Diff string `json:"diff,omitempty"`
}

// EventTypeValue returns the event's discriminator. It lets callers treat
// Event as an interface-like value when writing small dispatchers.
func (e Event) EventTypeValue() EventType { return e.Type }

// CompactInfo describes a context compaction in a compact event.
type CompactInfo struct {
	Reason             string `json:"reason,omitempty"`
	SummaryChars       int    `json:"summary_chars"`
	MessagesBefore     int    `json:"messages_before"`
	MessagesAfter      int    `json:"messages_after"`
	MessagesSummarized int    `json:"messages_summarized"`
	KeptTail           int    `json:"kept_tail"`
	CachePrefixReset   bool   `json:"cache_prefix_reset"`
}
