package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors for common API failures. Returned errors wrap these so
// callers can use errors.Is.
var (
	ErrUnauthorized   = errors.New("provider: unauthorized")
	ErrRateLimited    = errors.New("provider: rate limited")
	ErrContextTooLong = errors.New("provider: context too long")
)

// APIError is a typed error returned by the DeepSeek API.
type APIError struct {
	StatusCode int
	Message    string
	Type       string
	Retryable  bool

	// ContextTooLong marks requests rejected because the prompt exceeds the
	// model's context window. Such requests must never be retried.
	ContextTooLong bool
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if e.Type != "" {
		return fmt.Sprintf("provider: deepseek API error %d (%s): %s", e.StatusCode, e.Type, msg)
	}
	return fmt.Sprintf("provider: deepseek API error %d: %s", e.StatusCode, msg)
}

// Unwrap exposes the sentinel matching the underlying failure class.
func (e *APIError) Unwrap() error {
	if e == nil {
		return nil
	}
	switch {
	case e.ContextTooLong:
		return ErrContextTooLong
	case e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden:
		return ErrUnauthorized
	case e.StatusCode == http.StatusTooManyRequests:
		return ErrRateLimited
	}
	return nil
}

const defaultDeepSeekBaseURL = "https://api.deepseek.com"

// DeepSeekClient implements Provider against DeepSeek's OpenAI-compatible
// chat completions API.
type DeepSeekClient struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client

	// MaxRetries is the number of retries after the initial attempt. Zero
	// means the default (4).
	MaxRetries int
	// BaseBackoff is the first retry delay. Zero means the default (500ms).
	BaseBackoff time.Duration
	// DebugCacheLogger, when set, receives diagnostics about prefix hashes
	// and prompt-cache usage.
	DebugCacheLogger func(msg string)
}

// NewDeepSeekClient returns a client with tern's default settings.
func NewDeepSeekClient(apiKey string) *DeepSeekClient {
	return &DeepSeekClient{
		APIKey:      apiKey,
		BaseURL:     defaultDeepSeekBaseURL,
		MaxRetries:  4,
		BaseBackoff: 500 * time.Millisecond,
	}
}

// ModelInfo implements Provider.
func (c *DeepSeekClient) ModelInfo(model string) ModelCapabilities {
	return DefaultModelCapabilities(model)
}

func (c *DeepSeekClient) endpoint() string {
	base := c.BaseURL
	if base == "" {
		base = defaultDeepSeekBaseURL
	}
	return strings.TrimRight(base, "/") + "/chat/completions"
}

func (c *DeepSeekClient) maxRetries() int {
	if c.MaxRetries <= 0 {
		return 4
	}
	return c.MaxRetries
}

func (c *DeepSeekClient) baseBackoff() time.Duration {
	if c.BaseBackoff <= 0 {
		return 500 * time.Millisecond
	}
	return c.BaseBackoff
}

// Stream implements Provider. Messages are prepared with
// PrepareMessagesForRequest before being sent, so stale reasoning_content
// from completed turns is never transmitted.
func (c *DeepSeekClient) Stream(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	prepared := req
	prepared.Messages = PrepareMessagesForRequest(req.Messages)
	prepared.Stream = true

	body, err := MarshalCanonicalRequest(prepared)
	if err != nil {
		return nil, err
	}
	if c.DebugCacheLogger != nil {
		if hash, hashErr := PrefixHash(prepared); hashErr == nil {
			c.DebugCacheLogger(fmt.Sprintf("request model=%s prefix_hash=%s", prepared.Model, hash))
		}
	}

	resp, err := c.doWithRetry(ctx, body)
	if err != nil {
		return nil, err
	}

	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		// ParseSSEStream emits its own error events; the error return is
		// intentionally discarded here because the channel is the API.
		_ = parseSSEStream(ctx, resp.Body, out, c.DebugCacheLogger)
	}()
	return out, nil
}

func (c *DeepSeekClient) doWithRetry(ctx context.Context, body []byte) (*http.Response, error) {
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	endpoint := c.endpoint()
	attempts := c.maxRetries() + 1

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("provider: build request: %w", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")

		resp, err := client.Do(httpReq)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			lastErr = err
			if attempt == attempts-1 {
				break
			}
			if !sleepContext(ctx, c.backoff(attempt, 0)) {
				return nil, ctx.Err()
			}
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		apiErr, retryAfter := c.readAPIError(resp)
		lastErr = apiErr
		if !apiErr.Retryable || attempt == attempts-1 {
			return nil, apiErr
		}
		if !sleepContext(ctx, c.backoff(attempt, retryAfter)) {
			return nil, ctx.Err()
		}
	}

	if lastErr == nil {
		lastErr = errors.New("provider: request failed")
	}
	return nil, fmt.Errorf("provider: request failed after %d attempts: %w", attempts, lastErr)
}

// readAPIError drains and closes the response body, returning a typed error
// plus any Retry-After delay.
func (c *DeepSeekClient) readAPIError(resp *http.Response) (*APIError, time.Duration) {
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		Retryable:  resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &payload); err == nil && payload.Error.Message != "" {
		apiErr.Message = payload.Error.Message
		apiErr.Type = payload.Error.Type
	} else {
		apiErr.Message = strings.TrimSpace(string(data))
		if apiErr.Message == "" {
			apiErr.Message = resp.Status
		}
	}
	if apiErr.Type == "" {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			apiErr.Type = "unauthorized"
		case http.StatusTooManyRequests:
			apiErr.Type = "rate_limit"
		}
	}
	if isContextTooLong(resp.StatusCode, apiErr.Message, apiErr.Type) {
		apiErr.ContextTooLong = true
		apiErr.Retryable = false
	}
	return apiErr, parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
}

func isContextTooLong(status int, message, typ string) bool {
	if status != http.StatusBadRequest && status != http.StatusRequestEntityTooLarge && status != http.StatusUnprocessableEntity {
		return false
	}
	haystack := strings.ToLower(message + " " + typ)
	markers := []string{
		"context length",
		"context_length",
		"context window",
		"context limit",
		"context too long",
		"maximum context",
		"max context",
		"input is too long",
		"prompt is too long",
		"too many tokens",
		"token limit",
		"exceeds the maximum",
	}
	for _, marker := range markers {
		if strings.Contains(haystack, marker) {
			return true
		}
	}
	return false
}

func (c *DeepSeekClient) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	base := c.baseBackoff()
	exp := base
	for i := 0; i < attempt && exp < 30*time.Second; i++ {
		exp *= 2
	}
	if exp > 30*time.Second {
		exp = 30 * time.Second
	}
	half := exp / 2
	if half <= 0 {
		return exp
	}
	// Full-ish jitter in [half, exp) to avoid synchronized retry storms.
	return half + time.Duration(rand.Int64N(int64(half)))
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// parseRetryAfter accepts either a delay in seconds or an HTTP date.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			seconds = 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		d := when.Sub(now)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

// ParseSSEStream parses an OpenAI/DeepSeek SSE stream. It emits reasoning,
// text and tool-call deltas as they arrive, a usage event when the final
// accounting chunk is seen, and exactly one done event. Parse or read
// failures are emitted as EventError and returned.
//
// The caller owns the output channel and must close it.
func ParseSSEStream(ctx context.Context, r io.Reader, out chan<- StreamEvent) error {
	return parseSSEStream(ctx, r, out, nil)
}

func parseSSEStream(ctx context.Context, r io.Reader, out chan<- StreamEvent, logger func(string)) error {
	reader := bufio.NewReaderSize(r, 64<<10)
	finishReason := ""

	for {
		if err := ctx.Err(); err != nil {
			emitEvent(ctx, out, StreamEvent{Type: EventError, Err: err})
			return err
		}

		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")

			switch {
			case strings.HasPrefix(line, ":"):
				// Comment / keep-alive.
			case strings.HasPrefix(line, "data:"):
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "[DONE]" {
					return emitDone(ctx, out, finishReason)
				}
				if data != "" {
					var chunk sseChunk
					if err := json.Unmarshal([]byte(data), &chunk); err != nil {
						perr := fmt.Errorf("provider: decode SSE chunk: %w", err)
						emitEvent(ctx, out, StreamEvent{Type: EventError, Err: perr})
						return perr
					}
					fr, err := handleSSEChunk(ctx, out, chunk, logger)
					if err != nil {
						return err
					}
					if fr != "" {
						finishReason = fr
					}
				}
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return emitDone(ctx, out, finishReason)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				emitEvent(ctx, out, StreamEvent{Type: EventError, Err: ctxErr})
				return ctxErr
			}
			emitEvent(ctx, out, StreamEvent{Type: EventError, Err: readErr})
			return readErr
		}
	}
}

func emitDone(ctx context.Context, out chan<- StreamEvent, finishReason string) error {
	if emitEvent(ctx, out, StreamEvent{Type: EventDone, FinishReason: finishReason}) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

func handleSSEChunk(ctx context.Context, out chan<- StreamEvent, chunk sseChunk, logger func(string)) (string, error) {
	finishReason := ""
	for _, choice := range chunk.Choices {
		delta := choice.Delta
		if delta.ReasoningContent != "" {
			if !emitEvent(ctx, out, StreamEvent{Type: EventReasoningDelta, Text: delta.ReasoningContent}) {
				return finishReason, emitFailure(ctx)
			}
		}
		if delta.Content != "" {
			if !emitEvent(ctx, out, StreamEvent{Type: EventTextDelta, Text: delta.Content}) {
				return finishReason, emitFailure(ctx)
			}
		}
		for _, tc := range delta.ToolCalls {
			ev := StreamEvent{
				Type:          EventToolCallDelta,
				ToolCallIndex: tc.Index,
				ToolCallID:    tc.ID,
				ToolName:      tc.Function.Name,
				ToolArgsDelta: tc.Function.Arguments,
			}
			if !emitEvent(ctx, out, ev) {
				return finishReason, emitFailure(ctx)
			}
		}
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
		}
	}

	if chunk.Usage != nil {
		hit := chunk.Usage.PromptCacheHitTokens
		if hit == 0 && chunk.Usage.PromptTokensDetails.CachedTokens > 0 {
			hit = chunk.Usage.PromptTokensDetails.CachedTokens
		}
		miss := chunk.Usage.PromptCacheMissTokens
		if miss == 0 && chunk.Usage.PromptTokens > 0 {
			miss = chunk.Usage.PromptTokens - hit
			if miss < 0 {
				miss = 0
			}
		}
		total := chunk.Usage.TotalTokens
		if total == 0 && (chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0) {
			total = chunk.Usage.PromptTokens + chunk.Usage.CompletionTokens
		}
		usage := &Usage{
			PromptTokens:          chunk.Usage.PromptTokens,
			CompletionTokens:      chunk.Usage.CompletionTokens,
			TotalTokens:           total,
			PromptCacheHitTokens:  hit,
			PromptCacheMissTokens: miss,
			ReasoningTokens:       chunk.Usage.CompletionTokensDetails.ReasoningTokens,
		}
		if logger != nil {
			logger(fmt.Sprintf(
				"usage prompt_tokens=%d completion_tokens=%d cache_hit=%d cache_miss=%d reasoning_tokens=%d",
				usage.PromptTokens, usage.CompletionTokens, usage.PromptCacheHitTokens, usage.PromptCacheMissTokens, usage.ReasoningTokens,
			))
		}
		if !emitEvent(ctx, out, StreamEvent{Type: EventUsage, Usage: usage}) {
			return finishReason, emitFailure(ctx)
		}
	}
	return finishReason, nil
}

func emitFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

func emitEvent(ctx context.Context, out chan<- StreamEvent, ev StreamEvent) bool {
	if out == nil {
		return false
	}
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// sseChunk mirrors the subset of the OpenAI streaming response we consume.
type sseChunk struct {
	Choices []struct {
		Index        int      `json:"index"`
		Delta        sseDelta `json:"delta"`
		FinishReason string   `json:"finish_reason"`
	} `json:"choices"`
	Usage *sseUsage `json:"usage"`
}

type sseDelta struct {
	ReasoningContent string        `json:"reasoning_content"`
	Content          string        `json:"content"`
	ToolCalls        []sseToolCall `json:"tool_calls"`
}

type sseToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type sseUsage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails   struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}
