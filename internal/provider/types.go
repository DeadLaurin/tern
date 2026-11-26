// Package provider defines the model-provider abstraction used by tern.
//
// The core implementation targets DeepSeek's OpenAI-compatible chat
// completions API, but the types are deliberately provider agnostic so that
// additional backends (or a deterministic mock) can be plugged in.
package provider

import (
	"context"
	"encoding/json"
	"strings"
)

// Provider is the minimal interface a model backend must implement.
type Provider interface {
	// Stream sends a chat request and returns a channel of incremental
	// stream events. The channel is closed once the response is complete or
	// the context is cancelled. An error is returned when the request could
	// not be started at all.
	Stream(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)

	// ModelInfo reports static capabilities for a model name.
	ModelInfo(model string) ModelCapabilities
}

// Default capability values shared by the DeepSeek models tern supports.
const (
	DefaultContextWindow = 1_000_000
	DefaultMaxOutput     = 384 * 1024
)

// ModelCapabilities describes the static limits and features of a model.
type ModelCapabilities struct {
	Name             string
	ContextWindow    int
	MaxOutput        int
	SupportsVision   bool
	SupportsThinking bool
}

// DefaultModelCapabilities returns the capability set for a known model,
// falling back to a conservative DeepSeek-compatible description for unknown
// names.
func DefaultModelCapabilities(model string) ModelCapabilities {
	caps := ModelCapabilities{
		Name:             model,
		ContextWindow:    DefaultContextWindow,
		MaxOutput:        DefaultMaxOutput,
		SupportsVision:   false,
		SupportsThinking: true,
	}
	lower := strings.ToLower(model)
	switch {
	case lower == "deepseek-v4-pro":
		caps.SupportsVision = false
	case lower == "deepseek-flash":
		caps.SupportsVision = true
	case strings.Contains(lower, "claude-"):
		// Anthropic Claude models (also seen as openrouter/anthropic/claude-*).
		caps.ContextWindow = 200_000
		caps.SupportsVision = true
	case strings.Contains(lower, "gemini-"):
		// Google Gemini models support a ~1M token context.
		caps.ContextWindow = 1_000_000
		caps.SupportsVision = true
	case strings.Contains(lower, "gpt-4"),
		strings.Contains(lower, "gpt-5"),
		strings.Contains(lower, "o3"),
		strings.Contains(lower, "o4"):
		// OpenAI multimodal model families.
		caps.SupportsVision = true
	}
	return caps
}

// Role is a chat message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is a single chat message.
//
// When Parts is non-empty (multimodal user messages) the serialized content
// field is a JSON array of content parts. Otherwise content is a plain JSON
// string taken from Content.
type Message struct {
	Role Role

	// Content is plain text content.
	Content string

	// Parts carries multimodal content (for example text plus an image).
	// When non-empty it takes precedence over Content.
	Parts []ContentPart

	// ReasoningContent is DeepSeek's thinking chain-of-thought. It must be
	// echoed back only for tool-call sub-turns inside the current user turn.
	ReasoningContent string

	// ToolCalls is set on assistant messages that invoke tools.
	ToolCalls []ToolCall

	// ToolCallID is set on role=tool messages.
	ToolCallID string

	// Name is an optional participant name.
	Name string
}

type messageJSON struct {
	Role             Role            `json:"role"`
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
}

// MarshalJSON serializes the message in OpenAI/DeepSeek chat format.
func (m Message) MarshalJSON() ([]byte, error) {
	content, err := m.marshalContent()
	if err != nil {
		return nil, err
	}
	return json.Marshal(messageJSON{
		Role:             m.Role,
		Content:          content,
		ReasoningContent: m.ReasoningContent,
		ToolCalls:        m.ToolCalls,
		ToolCallID:       m.ToolCallID,
		Name:             m.Name,
	})
}

func (m Message) marshalContent() (json.RawMessage, error) {
	if len(m.Parts) > 0 {
		b, err := json.Marshal(m.Parts)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(b), nil
	}
	b, err := json.Marshal(m.Content)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// UnmarshalJSON accepts both string and array content forms.
func (m *Message) UnmarshalJSON(b []byte) error {
	var raw struct {
		Role             Role            `json:"role"`
		Content          json.RawMessage `json:"content"`
		ReasoningContent string          `json:"reasoning_content"`
		ToolCalls        []ToolCall      `json:"tool_calls"`
		ToolCallID       string          `json:"tool_call_id"`
		Name             string          `json:"name"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.ReasoningContent = raw.ReasoningContent
	m.ToolCalls = raw.ToolCalls
	m.ToolCallID = raw.ToolCallID
	m.Name = raw.Name
	m.Content = ""
	m.Parts = nil
	if len(raw.Content) == 0 || string(raw.Content) == "null" {
		return nil
	}
	if raw.Content[0] == '"' {
		return json.Unmarshal(raw.Content, &m.Content)
	}
	if raw.Content[0] == '[' {
		return json.Unmarshal(raw.Content, &m.Parts)
	}
	// Unknown content shape; keep it as raw text rather than failing.
	m.Content = string(raw.Content)
	return nil
}

// ContentPart is one element of a multimodal content array.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL points at a remote image.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// ToolDef describes a callable tool. It serializes in OpenAI's
// {"type":"function", ...} shape.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type toolDefJSON struct {
	Type     string           `json:"type"`
	Function toolFunctionJSON `json:"function"`
}

type toolFunctionJSON struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// MarshalJSON serializes the tool definition in OpenAI function format.
func (t ToolDef) MarshalJSON() ([]byte, error) {
	params := t.Parameters
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	return json.Marshal(toolDefJSON{
		Type: "function",
		Function: toolFunctionJSON{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		},
	})
}

// UnmarshalJSON accepts the OpenAI function tool shape.
func (t *ToolDef) UnmarshalJSON(b []byte) error {
	var raw toolDefJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	t.Name = raw.Function.Name
	t.Description = raw.Function.Description
	t.Parameters = raw.Function.Parameters
	return nil
}

// ToolCall is a function call requested by the assistant.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the tool name and its JSON-encoded arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatRequest is a single chat completion request.
type ChatRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Tools           []ToolDef `json:"tools,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	MaxTokens       int       `json:"max_tokens,omitempty"`
	Stream          bool      `json:"stream,omitempty"`
}

// Usage reports token accounting for a response.
type Usage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens,omitempty"`
	ReasoningTokens       int `json:"reasoning_tokens,omitempty"`
}

// EventType discriminates StreamEvent payloads.
type EventType string

const (
	EventReasoningDelta EventType = "reasoning_delta"
	EventTextDelta      EventType = "text_delta"
	EventToolCallDelta  EventType = "tool_call_delta"
	EventUsage          EventType = "usage"
	EventDone           EventType = "done"
	EventError          EventType = "error"
)

// StreamEvent is one incremental response event.
type StreamEvent struct {
	Type          EventType
	Text          string
	ToolCallIndex int
	ToolCallID    string
	ToolName      string
	ToolArgsDelta string
	FinishReason  string
	Usage         *Usage
	Err           error
}
