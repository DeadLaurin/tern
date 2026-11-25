package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// MarshalCanonicalRequest serializes a request into a byte-for-byte stable
// representation suitable for prefix hashing and cache verification.
//
// The following transformations guarantee determinism:
//   - tools are sorted by name (then description and parameters as tie-breakers)
//   - tool parameter schemas are canonicalized: object keys are sorted
//     recursively and irrelevant whitespace is removed
//   - stream is always true and stream_options always requests usage
//   - thinking is derived from reasoning_effort ("none" disables it, any
//     other value enables it)
//
// No Go map iteration ever contributes ordering to the output.
func MarshalCanonicalRequest(req ChatRequest) ([]byte, error) {
	tools, err := canonicalTools(req.Tools)
	if err != nil {
		return nil, err
	}

	// Use a struct (not a map) so field ordering is fixed.
	payload := struct {
		Model           string        `json:"model"`
		Messages        []Message     `json:"messages"`
		Tools           []ToolDef     `json:"tools,omitempty"`
		MaxTokens       int           `json:"max_tokens,omitempty"`
		Stream          bool          `json:"stream"`
		StreamOptions   streamOptions `json:"stream_options"`
		ReasoningEffort string        `json:"reasoning_effort,omitempty"`
		Thinking        thinking      `json:"thinking"`
	}{
		Model:           req.Model,
		Messages:        req.Messages,
		Tools:           tools,
		MaxTokens:       req.MaxTokens,
		Stream:          true,
		StreamOptions:   streamOptions{IncludeUsage: true},
		ReasoningEffort: req.ReasoningEffort,
		Thinking:        thinking{Type: thinkingType(req.ReasoningEffort)},
	}
	if payload.Messages == nil {
		payload.Messages = []Message{}
	}

	return json.Marshal(payload)
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type thinking struct {
	Type string `json:"type"`
}

func thinkingType(reasoningEffort string) string {
	if reasoningEffort == "none" {
		return "disabled"
	}
	return "enabled"
}

// canonicalTools returns a sorted copy of tools with canonical parameters.
// The input slice and its elements are never mutated.
func canonicalTools(tools []ToolDef) ([]ToolDef, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]ToolDef, len(tools))
	for i, tool := range tools {
		params, err := canonicalizeJSON(tool.Parameters)
		if err != nil {
			return nil, fmt.Errorf("provider: canonicalize parameters for tool %q: %w", tool.Name, err)
		}
		out[i] = ToolDef{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  params,
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Description != out[j].Description {
			return out[i].Description < out[j].Description
		}
		return bytes.Compare(out[i].Parameters, out[j].Parameters) < 0
	})
	return out, nil
}

// canonicalizeJSON parses arbitrary JSON and re-emits it with recursively
// sorted object keys and no insignificant whitespace.
func canonicalizeJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	// Reject trailing garbage so malformed schemas cannot silently hash.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("unexpected trailing JSON data")
	}
	var buf bytes.Buffer
	if err := writeCanonicalJSON(&buf, value); err != nil {
		return nil, err
	}
	return json.RawMessage(buf.Bytes()), nil
}

func writeCanonicalJSON(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(b)
	case json.Number:
		// The decoder already validated the JSON, so the literal is safe.
		buf.WriteString(v.String())
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(key)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonicalJSON(buf, v[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", value)
	}
	return nil
}

// PrepareMessagesForRequest applies DeepSeek's multi-turn reasoning_content
// rule.
//
// Old chain-of-thought from completed turns must not be resent (it breaks
// prefix caching and is unnecessary), but reasoning_content produced during
// tool-call sub-turns inside the active user turn must be preserved verbatim,
// otherwise DeepSeek rejects the request with HTTP 400.
//
// The returned slice is a copy; the input is never mutated.
func PrepareMessagesForRequest(history []Message) []Message {
	if len(history) == 0 {
		return nil
	}
	out := make([]Message, len(history))
	copy(out, history)

	lastUser := -1
	for i := range out {
		if out[i].Role == RoleUser {
			lastUser = i
		}
	}
	if lastUser < 0 {
		// No active user turn: nothing is safe to strip based on the rule.
		return out
	}
	for i := 0; i < lastUser; i++ {
		if out[i].Role == RoleAssistant {
			out[i].ReasoningContent = ""
		}
	}
	return out
}

// PrefixHash returns the SHA-256 hex digest of the canonical request bytes.
func PrefixHash(req ChatRequest) (string, error) {
	b, err := MarshalCanonicalRequest(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// SystemToolsHash returns a SHA-256 hex digest covering only the system
// prompt and tool definitions, which form the stable prefix shared by every
// request in a conversation.
func SystemToolsHash(req ChatRequest) (string, error) {
	tools, err := canonicalTools(req.Tools)
	if err != nil {
		return "", err
	}
	system := make([]Message, 0, len(req.Messages))
	for _, msg := range req.Messages {
		if msg.Role == RoleSystem {
			system = append(system, msg)
		}
	}
	payload := struct {
		Messages []Message `json:"messages"`
		Tools    []ToolDef `json:"tools,omitempty"`
	}{
		Messages: system,
		Tools:    tools,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
