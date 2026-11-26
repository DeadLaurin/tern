// Package acp implements the Agent Client Protocol (ACP) for tern.
//
// ACP is JSON-RPC 2.0 carried as newline-delimited JSON over stdio. An ACP
// host (T3 Code and others) starts `tern acp`, performs an initialize
// handshake, creates or loads a session and then drives the agent with
// session/prompt requests while streaming session/update notifications.
//
// The package deliberately depends only on the agent, permission, provider and
// session packages so that it can be embedded by any front end.
package acp

import (
	"encoding/json"
)

// JSONRPCVersion is the only protocol version this package emits.
const JSONRPCVersion = "2.0"

// ProtocolVersion is the ACP protocol version advertised by initialize.
const ProtocolVersion = 1

// JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	CodeRequestCancel  = -32800
)

// rpcIncoming is the union of a JSON-RPC request, notification and response.
type rpcIncoming struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCRequest is a JSON-RPC 2.0 request or notification.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  any             `json:"params,omitempty"`
}

// RPCResponse is a JSON-RPC 2.0 response.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCNotification is a JSON-RPC 2.0 notification.
type RPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error implements error.
func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentInfo         agentInfo         `json:"agentInfo"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
}

type agentInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version,omitempty"`
}

type agentCapabilities struct {
	LoadSession        bool               `json:"loadSession"`
	PromptCapabilities promptCapabilities `json:"promptCapabilities"`
}

type promptCapabilities struct {
	Image           bool `json:"image"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// ---------------------------------------------------------------------------
// session state
// ---------------------------------------------------------------------------

type newSessionParams struct {
	Cwd        string `json:"cwd"`
	McpServers any    `json:"mcpServers,omitempty"`
}

type loadSessionParams struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd,omitempty"`
}

type modelInfo struct {
	ModelID     string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type modelsState struct {
	CurrentModelID  string      `json:"currentModelId"`
	AvailableModels []modelInfo `json:"availableModels"`
}

type modeInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type modesState struct {
	CurrentModeID  string     `json:"currentModeId"`
	AvailableModes []modeInfo `json:"availableModes"`
}

type sessionState struct {
	SessionID string      `json:"sessionId"`
	Models    modelsState `json:"models"`
	Modes     modesState  `json:"modes"`
}

type setModelParams struct {
	SessionID       string `json:"sessionId"`
	ModelID         string `json:"modelId"`
	ConfigID        string `json:"configId,omitempty"`
	Effort          string `json:"effort,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

type setModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

// ---------------------------------------------------------------------------
// session/prompt
// ---------------------------------------------------------------------------

type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type contentBlock struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	Data     string         `json:"data,omitempty"`
	MimeType string         `json:"mimeType,omitempty"`
	URI      string         `json:"uri,omitempty"`
	Resource *resourceBlock `json:"resource,omitempty"`
}

type resourceBlock struct {
	URI      string `json:"uri"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// promptUsage is the ACP usage block returned by session/prompt.
type promptUsage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	TotalTokens      int `json:"totalTokens"`
	CachedReadTokens int `json:"cachedReadTokens"`
	ThoughtTokens    int `json:"thoughtTokens"`
}

// promptMeta carries tern-specific accounting beyond the ACP usage block.
type promptMeta struct {
	CostUSD       float64 `json:"costUsd"`
	TotalCostUSD  float64 `json:"totalCostUsd"`
	ContextTokens int     `json:"contextTokens"`
	ContextWindow int     `json:"contextWindow"`
	CacheHitRate  float64 `json:"cacheHitRate"`
	TokensPerSec  float64 `json:"tokensPerSec"`
	Steps         int     `json:"steps"`
	ToolCalls     int     `json:"toolCalls"`
	DurationMs    int64   `json:"durationMs"`
}

type promptResult struct {
	StopReason string      `json:"stopReason"`
	Usage      promptUsage `json:"usage"`
	Meta       promptMeta  `json:"_meta"`
}

// ---------------------------------------------------------------------------
// session/update notifications
// ---------------------------------------------------------------------------

type textChunkUpdate struct {
	SessionID     string       `json:"sessionId"`
	SessionUpdate string       `json:"sessionUpdate"`
	Content       contentBlock `json:"content"`
}

type toolCallContent struct {
	Type    string       `json:"type"`
	Content contentBlock `json:"content"`
}

type toolCallUpdate struct {
	SessionID     string            `json:"sessionId"`
	SessionUpdate string            `json:"sessionUpdate"`
	ToolCallID    string            `json:"toolCallId"`
	Title         string            `json:"title,omitempty"`
	Kind          string            `json:"kind,omitempty"`
	Status        string            `json:"status,omitempty"`
	RawInput      any               `json:"rawInput,omitempty"`
	RawOutput     any               `json:"rawOutput,omitempty"`
	Content       []toolCallContent `json:"content,omitempty"`
}

type usageCost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

type usageMeta struct {
	InputTokens      int     `json:"inputTokens"`
	OutputTokens     int     `json:"outputTokens"`
	TotalTokens      int     `json:"totalTokens"`
	CachedReadTokens int     `json:"cachedReadTokens"`
	CacheMissTokens  int     `json:"cacheMissTokens"`
	ReasoningTokens  int     `json:"reasoningTokens"`
	CacheHitRate     float64 `json:"cacheHitRate"`
	TokensPerSec     float64 `json:"tokensPerSec"`
	TurnCostUSD      float64 `json:"turnCostUsd"`
	TotalCostUSD     float64 `json:"totalCostUsd"`
}

type usageUpdate struct {
	SessionID     string    `json:"sessionId"`
	SessionUpdate string    `json:"sessionUpdate"`
	Used          int       `json:"used"`
	Size          int       `json:"size"`
	Cost          usageCost `json:"cost"`
	Meta          usageMeta `json:"_meta"`
}

// ---------------------------------------------------------------------------
// session/request_permission
// ---------------------------------------------------------------------------

type permissionToolCall struct {
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title"`
	Kind       string `json:"kind,omitempty"`
	Status     string `json:"status,omitempty"`
	RawInput   any    `json:"rawInput,omitempty"`
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type permissionRequestParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  permissionToolCall `json:"toolCall"`
	Options   []permissionOption `json:"options"`
}

type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

type permissionResult struct {
	Outcome permissionOutcome `json:"outcome"`
}

// ---------------------------------------------------------------------------
// session/stats (tern extension)
// ---------------------------------------------------------------------------

type sessionStatsResult struct {
	SessionID     string  `json:"sessionId"`
	Usage         any     `json:"usage"`
	TotalCostUSD  float64 `json:"totalCostUsd"`
	CacheHitRate  float64 `json:"cacheHitRate"`
	ContextTokens int     `json:"contextTokens"`
	ContextWindow int     `json:"contextWindow"`
	Store         any     `json:"store,omitempty"`
}
