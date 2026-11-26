package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMarshalCanonicalRequestDeterministic(t *testing.T) {
	// Two textually different but semantically identical nested schemas. If
	// canonicalization ever relied on map iteration order these would drift.
	schemas := []string{
		`{"type":"object","properties":{"z":{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"integer"}}},"a":{"type":"array","items":{"type":"string"}}},"required":["a","z"]}`,
		`{"required":["a","z"],"properties":{"a":{"items":{"type":"string"},"type":"array"},"z":{"properties":{"a":{"type":"integer"},"b":{"type":"string"}},"type":"object"}},"type":"object"}`,
	}
	baseTools := []ToolDef{
		{Name: "zeta", Description: "last", Parameters: json.RawMessage(schemas[0])},
		{Name: "alpha", Description: "first", Parameters: json.RawMessage(schemas[1])},
		{Name: "middle", Description: "middle", Parameters: json.RawMessage(schemas[0])},
	}
	messages := []Message{
		{Role: RoleSystem, Content: "be helpful"},
		{Role: RoleUser, Parts: []ContentPart{
			{Type: "text", Text: "look"},
			{Type: "image_url", ImageURL: &ImageURL{URL: "https://example.com/a.png", Detail: "high"}},
		}},
		{Role: RoleAssistant, Content: "sure", ToolCalls: []ToolCall{{
			ID: "call_1", Type: "function",
			Function: ToolCallFunction{Name: "alpha", Arguments: `{"x":1}`},
		}}},
		{Role: RoleTool, ToolCallID: "call_1", Content: "result"},
	}

	var wantBytes []byte
	var wantHash string
	for i := 0; i < 50; i++ {
		tools := make([]ToolDef, len(baseTools))
		for j, tool := range baseTools {
			tools[j] = tool
			// Rotate schema variants so key order also varies by run.
			tools[j].Parameters = json.RawMessage(schemas[(i+j)%len(schemas)])
		}
		// Rotate and sometimes reverse tools to prove sorting is independent
		// of caller order.
		shift := i % len(tools)
		tools = append(append([]ToolDef{}, tools[shift:]...), tools[:shift]...)
		if i%2 == 1 {
			for l, r := 0, len(tools)-1; l < r; l, r = l+1, r-1 {
				tools[l], tools[r] = tools[r], tools[l]
			}
		}

		req := ChatRequest{
			Model:           "deepseek-flash",
			Messages:        messages,
			Tools:           tools,
			ReasoningEffort: "high",
			MaxTokens:       4096,
		}
		got, err := MarshalCanonicalRequest(req)
		if err != nil {
			t.Fatalf("run %d: MarshalCanonicalRequest: %v", i, err)
		}
		sum := sha256.Sum256(got)
		gotHash := hex.EncodeToString(sum[:])
		if i == 0 {
			wantBytes = append([]byte(nil), got...)
			wantHash = gotHash
		} else {
			if !bytes.Equal(got, wantBytes) {
				t.Fatalf("run %d: canonical bytes differ\n first: %s\n  run: %s", i, wantBytes, got)
			}
			if gotHash != wantHash {
				t.Fatalf("run %d: hash %s != %s", i, gotHash, wantHash)
			}
		}

		hash, err := PrefixHash(req)
		if err != nil {
			t.Fatalf("run %d: PrefixHash: %v", i, err)
		}
		if hash != wantHash {
			t.Fatalf("run %d: PrefixHash %s != %s", i, hash, wantHash)
		}
	}

	var decoded struct {
		Stream        bool `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Thinking struct {
			Type string `json:"type"`
		} `json:"thinking"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(wantBytes, &decoded); err != nil {
		t.Fatalf("canonical request is not valid JSON: %v", err)
	}
	if !decoded.Stream || !decoded.StreamOptions.IncludeUsage {
		t.Fatalf("expected stream=true with include_usage, got %s", wantBytes)
	}
	if decoded.Thinking.Type != "enabled" {
		t.Fatalf("expected thinking enabled, got %q", decoded.Thinking.Type)
	}
	if len(decoded.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(decoded.Tools))
	}
	wantNames := []string{"alpha", "middle", "zeta"}
	for i, name := range wantNames {
		if decoded.Tools[i].Function.Name != name {
			t.Fatalf("tool %d = %q, want %q", i, decoded.Tools[i].Function.Name, name)
		}
		if decoded.Tools[i].Type != "function" {
			t.Fatalf("tool %d type = %q, want function", i, decoded.Tools[i].Type)
		}
	}
	// Multimodal user message must serialize content as an array.
	if !bytes.HasPrefix(bytes.TrimSpace(decoded.Messages[1].Content), []byte("[")) {
		t.Fatalf("expected parts content array, got %s", decoded.Messages[1].Content)
	}
	// Plain messages must serialize content as a string.
	if !bytes.HasPrefix(bytes.TrimSpace(decoded.Messages[0].Content), []byte(`"`)) {
		t.Fatalf("expected string content, got %s", decoded.Messages[0].Content)
	}
}

func TestMarshalCanonicalRequestThinkingDisabled(t *testing.T) {
	req := ChatRequest{
		Model:           "deepseek-v4-pro",
		Messages:        []Message{{Role: RoleUser, Content: "hi"}},
		ReasoningEffort: "none",
	}
	b, err := MarshalCanonicalRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		ReasoningEffort string `json:"reasoning_effort"`
		Thinking        struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ReasoningEffort != "none" || decoded.Thinking.Type != "disabled" {
		t.Fatalf("unexpected effort/thinking: %s", b)
	}
}

func TestPrepareMessagesForRequest(t *testing.T) {
	history := []Message{
		{Role: RoleSystem, Content: "system"},
		{Role: RoleUser, Content: "old question"},
		{Role: RoleAssistant, Content: "old answer", ReasoningContent: "old-cot"},
		{Role: RoleUser, Content: "another stale question"},
		{Role: RoleAssistant, Content: "calling tool", ReasoningContent: "loop-cot", ToolCalls: []ToolCall{{
			ID: "call_1", Type: "function",
			Function: ToolCallFunction{Name: "lookup", Arguments: `{}`},
		}}},
		{Role: RoleTool, ToolCallID: "call_1", Content: "tool result"},
		{Role: RoleUser, Content: "current question"},
		{Role: RoleAssistant, Content: "tool followup", ReasoningContent: "current-tool-cot", ToolCalls: []ToolCall{{
			ID: "call_2", Type: "function",
			Function: ToolCallFunction{Name: "lookup", Arguments: `{"q":1}`},
		}}},
	}
	original := make([]Message, len(history))
	copy(original, history)

	got := PrepareMessagesForRequest(history)

	if got[2].ReasoningContent != "" {
		t.Fatalf("prior-turn reasoning not stripped: %q", got[2].ReasoningContent)
	}
	if got[4].ReasoningContent != "" {
		t.Fatalf("prior user-turn tool-loop reasoning not stripped: %q", got[4].ReasoningContent)
	}
	if got[7].ReasoningContent != "current-tool-cot" {
		t.Fatalf("current-turn tool-loop reasoning was not preserved: %q", got[7].ReasoningContent)
	}
	if got[0].Content != "system" || got[6].Content != "current question" {
		t.Fatalf("non-reasoning fields changed: %+v", got)
	}

	// Input must not be mutated.
	for i := range history {
		if history[i].ReasoningContent != original[i].ReasoningContent ||
			history[i].Content != original[i].Content ||
			history[i].ToolCallID != original[i].ToolCallID {
			t.Fatalf("input history mutated at %d: %+v (was %+v)", i, history[i], original[i])
		}
	}
	if len(history[2].ToolCalls) != len(original[2].ToolCalls) || len(history[4].ToolCalls) != len(original[4].ToolCalls) {
		t.Fatal("tool calls mutated")
	}

	// Mutating the result must not leak into the input.
	got[2].ReasoningContent = "changed"
	if history[2].ReasoningContent != "old-cot" {
		t.Fatal("result aliases input slice")
	}
}

func TestParseSSEStream(t *testing.T) {
	fixture := strings.Join([]string{
		": keep-alive",
		"event: message",
		"",
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"Let me "},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"think."}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"content":" world"}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":40,"total_tokens":140,"prompt_cache_hit_tokens":90,"prompt_cache_miss_tokens":10,"completion_tokens_details":{"reasoning_tokens":12}}}`,
		"",
		": keep-alive",
		"data: [DONE]",
		"",
	}, "\n")

	out := make(chan StreamEvent, 64)
	err := ParseSSEStream(context.Background(), strings.NewReader(fixture), out)
	close(out)
	if err != nil {
		t.Fatalf("ParseSSEStream: %v", err)
	}

	var reasoning, content string
	tool := struct {
		id, name, args string
	}{}
	var usage *Usage
	doneCount := 0
	finishReason := ""
	var all []StreamEvent
	for ev := range out {
		all = append(all, ev)
		switch ev.Type {
		case EventReasoningDelta:
			reasoning += ev.Text
		case EventTextDelta:
			content += ev.Text
		case EventToolCallDelta:
			if ev.ToolCallIndex != 0 {
				t.Fatalf("tool index = %d, want 0", ev.ToolCallIndex)
			}
			if ev.ToolCallID != "" {
				tool.id = ev.ToolCallID
			}
			if ev.ToolName != "" {
				tool.name = ev.ToolName
			}
			tool.args += ev.ToolArgsDelta
		case EventUsage:
			usage = ev.Usage
		case EventDone:
			doneCount++
			finishReason = ev.FinishReason
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if reasoning != "Let me think." {
		t.Fatalf("reasoning = %q", reasoning)
	}
	if content != "Hello world" {
		t.Fatalf("content = %q", content)
	}
	if tool.id != "call_abc" || tool.name != "get_weather" || tool.args != `{"city":"Paris"}` {
		t.Fatalf("tool call = %+v", tool)
	}
	if usage == nil {
		t.Fatal("missing usage event")
	}
	if usage.PromptTokens != 100 || usage.CompletionTokens != 40 || usage.TotalTokens != 140 {
		t.Fatalf("usage tokens = %+v", usage)
	}
	if usage.PromptCacheHitTokens != 90 || usage.PromptCacheMissTokens != 10 {
		t.Fatalf("cache usage = %+v", usage)
	}
	if usage.ReasoningTokens != 12 {
		t.Fatalf("reasoning tokens = %d", usage.ReasoningTokens)
	}
	if doneCount != 1 {
		t.Fatalf("done events = %d, want 1", doneCount)
	}
	if finishReason != "tool_calls" {
		t.Fatalf("finish reason = %q", finishReason)
	}
	if len(all) == 0 || all[len(all)-1].Type != EventDone {
		t.Fatalf("expected final event to be done, got %+v", all)
	}
}

func TestDeepSeekRetryRetryAfter(t *testing.T) {
	const successSSE = "" +
		`data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	t.Run("429 then 200", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit"}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(successSSE))
		}))
		defer srv.Close()

		client := testClient(srv.URL)
		events := streamAndCollect(t, client, ChatRequest{
			Model:    "deepseek-flash",
			Messages: []Message{{Role: RoleUser, Content: "hi"}},
		})
		if got := calls.Load(); got != 2 {
			t.Fatalf("calls = %d, want 2", got)
		}
		assertContentAndDone(t, events, "ok", "stop")
	})

	t.Run("500 then 200", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"server_error"}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(successSSE))
		}))
		defer srv.Close()

		events := streamAndCollect(t, testClient(srv.URL), ChatRequest{
			Model:    "deepseek-v4-pro",
			Messages: []Message{{Role: RoleUser, Content: "hi"}},
		})
		if got := calls.Load(); got != 2 {
			t.Fatalf("calls = %d, want 2", got)
		}
		assertContentAndDone(t, events, "ok", "stop")
	})

	t.Run("retry-after HTTP date", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", time.Now().Add(10*time.Millisecond).UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(successSSE))
		}))
		defer srv.Close()

		events := streamAndCollect(t, testClient(srv.URL), ChatRequest{
			Model:    "deepseek-flash",
			Messages: []Message{{Role: RoleUser, Content: "hi"}},
		})
		if got := calls.Load(); got != 2 {
			t.Fatalf("calls = %d, want 2", got)
		}
		assertContentAndDone(t, events, "ok", "stop")
	})
}

func TestDeepSeekNoRetryOnContextTooLong(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 65536 tokens","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	client := testClient(srv.URL)
	_, err := client.Stream(context.Background(), ChatRequest{
		Model:    "deepseek-flash",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected context-too-long error")
	}
	if !errors.Is(err, ErrContextTooLong) {
		t.Fatalf("errors.Is(ErrContextTooLong) = false: %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Retryable {
		t.Fatalf("expected non-retryable APIError, got %#v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (no retry)", got)
	}
}

func TestDeepSeekNoRetryOnUnauthorized(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"authentication_error"}}`))
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).Stream(context.Background(), ChatRequest{
		Model:    "deepseek-flash",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("errors.Is(ErrUnauthorized) = false: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (no retry)", got)
	}
}

func TestDeepSeekContextCancellationMidStream(t *testing.T) {
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer is not a flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"first"}}]}` + "\n\n"))
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := testClient(srv.URL).Stream(ctx, ChatRequest{
		Model:    "deepseek-flash",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		cancel()
		t.Fatalf("Stream: %v", err)
	}

	select {
	case ev, ok := <-ch:
		if !ok {
			cancel()
			t.Fatal("stream closed before first event")
		}
		if ev.Type != EventTextDelta || ev.Text != "first" {
			cancel()
			t.Fatalf("first event = %+v", ev)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for first event")
	}

	cancel()

	// Draining must terminate, proving the parser goroutine and response body
	// were closed on cancellation.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				goto drained
			}
		case <-deadline:
			t.Fatal("stream channel did not close after cancellation")
		}
	}
drained:
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server handler did not observe request cancellation")
	}
}

func TestMockProviderRecordsAndReplays(t *testing.T) {
	mp := NewMockProvider()
	mp.QueueScript(
		StreamEvent{Type: EventReasoningDelta, Text: "thinking"},
		StreamEvent{Type: EventTextDelta, Text: "answer"},
		StreamEvent{Type: EventDone, FinishReason: "stop"},
	)
	req := ChatRequest{
		Model:    "deepseek-flash",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}
	ch, err := mp.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var events []StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) != 3 || events[0].Text != "thinking" || events[1].Text != "answer" || events[2].FinishReason != "stop" {
		t.Fatalf("unexpected events: %+v", events)
	}

	wantBytes, err := MarshalCanonicalRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	gotBytes, ok := mp.LastRequestBytes()
	if !ok || !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("recorded canonical bytes differ\n got: %s\nwant: %s", gotBytes, wantBytes)
	}
	if last, ok := mp.LastRequest(); !ok || last.Model != "deepseek-flash" {
		t.Fatalf("last request not recorded: %+v", last)
	}

	// Raw SSE fixture replay shares the DeepSeek parser.
	mp.QueueSSE(`data: {"choices":[{"index":0,"delta":{"reasoning_content":"r","content":"c"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n")
	ch, err = mp.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	events = events[:0]
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) != 3 {
		t.Fatalf("unexpected replay events: %+v", events)
	}
	if events[0].Type != EventReasoningDelta || events[0].Text != "r" {
		t.Fatalf("replay reasoning = %+v", events[0])
	}
	if events[1].Type != EventTextDelta || events[1].Text != "c" {
		t.Fatalf("replay content = %+v", events[1])
	}
	if events[2].Type != EventDone || events[2].FinishReason != "stop" {
		t.Fatalf("expected final done event, got %+v", events)
	}
	if mp.Calls != 2 || len(mp.RequestBytes) != 2 {
		t.Fatalf("mock did not record both calls: calls=%d bytes=%d", mp.Calls, len(mp.RequestBytes))
	}
}

func TestModelCapabilities(t *testing.T) {
	flash := DefaultModelCapabilities("deepseek-flash")
	if flash.Name != "deepseek-flash" || flash.ContextWindow != 1_000_000 || flash.MaxOutput != 384*1024 {
		t.Fatalf("unexpected flash caps: %+v", flash)
	}
	if !flash.SupportsVision || !flash.SupportsThinking {
		t.Fatalf("flash should support vision and thinking: %+v", flash)
	}
	pro := DefaultModelCapabilities("deepseek-v4-pro")
	if pro.SupportsVision {
		t.Fatalf("pro should not support vision: %+v", pro)
	}
	if pro.ContextWindow != 1_000_000 || pro.MaxOutput != 384*1024 || !pro.SupportsThinking {
		t.Fatalf("unexpected pro caps: %+v", pro)
	}
}

func TestAPIErrorUnwrap(t *testing.T) {
	cases := []struct {
		err    *APIError
		target error
	}{
		{&APIError{StatusCode: 401}, ErrUnauthorized},
		{&APIError{StatusCode: 403}, ErrUnauthorized},
		{&APIError{StatusCode: 429}, ErrRateLimited},
		{&APIError{StatusCode: 400, ContextTooLong: true}, ErrContextTooLong},
	}
	for _, tc := range cases {
		if !errors.Is(tc.err, tc.target) {
			t.Fatalf("APIError %+v does not wrap %v", tc.err, tc.target)
		}
	}
	if (&APIError{StatusCode: 500}).Unwrap() != nil {
		t.Fatal("500 should not wrap a sentinel")
	}
}

func testClient(baseURL string) *DeepSeekClient {
	return &DeepSeekClient{
		APIKey:      "test-key",
		BaseURL:     baseURL,
		HTTPClient:  &http.Client{},
		MaxRetries:  4,
		BaseBackoff: time.Millisecond,
	}
}

func streamAndCollect(t *testing.T, client *DeepSeekClient, req ChatRequest) []StreamEvent {
	t.Helper()
	ch, err := client.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var events []StreamEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-timeout:
			t.Fatal("timed out waiting for stream to close")
		}
	}
}

func assertContentAndDone(t *testing.T, events []StreamEvent, wantContent, wantFinish string) {
	t.Helper()
	var content string
	var done *StreamEvent
	for i := range events {
		ev := events[i]
		switch ev.Type {
		case EventTextDelta:
			content += ev.Text
		case EventDone:
			done = &events[i]
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if content != wantContent {
		t.Fatalf("content = %q, want %q", content, wantContent)
	}
	if done == nil || done.FinishReason != wantFinish {
		t.Fatalf("done event = %+v, want finish %q", done, wantFinish)
	}
}

func TestParseSSEStreamNormalizesUsageDetails(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":250,"prompt_tokens_details":{"cached_tokens":700},"completion_tokens_details":{"reasoning_tokens":100}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	ch := make(chan StreamEvent, 16)
	if err := ParseSSEStream(context.Background(), strings.NewReader(stream), ch); err != nil {
		t.Fatalf("ParseSSEStream: %v", err)
	}
	close(ch)

	var got *Usage
	for ev := range ch {
		if ev.Type == EventUsage && ev.Usage != nil {
			got = ev.Usage
		}
	}
	if got == nil {
		t.Fatal("missing usage event")
	}
	if got.PromptCacheHitTokens != 700 || got.PromptCacheMissTokens != 300 {
		t.Fatalf("cache hit/miss = %d/%d, want 700/300", got.PromptCacheHitTokens, got.PromptCacheMissTokens)
	}
	if got.TotalTokens != 1250 {
		t.Fatalf("total_tokens = %d, want 1250", got.TotalTokens)
	}
	if got.ReasoningTokens != 100 {
		t.Fatalf("reasoning_tokens = %d, want 100", got.ReasoningTokens)
	}
}
