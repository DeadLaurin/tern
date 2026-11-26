package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMarshalRequestForProviderThinking(t *testing.T) {
	req := ChatRequest{
		Model:           "some-model",
		Messages:        []Message{{Role: RoleUser, Content: "hi"}},
		ReasoningEffort: "none",
	}

	deepseek, err := MarshalRequestForProvider(req, "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(deepseek, []byte(`"thinking"`)) {
		t.Fatalf("deepseek payload missing thinking: %s", deepseek)
	}
	if !bytes.Contains(deepseek, []byte(`"reasoning_effort":"none"`)) {
		t.Fatalf("deepseek payload should keep reasoning_effort=none: %s", deepseek)
	}

	// Empty provider name keeps DeepSeek behavior.
	empty, err := MarshalRequestForProvider(req, "")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := MarshalCanonicalRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(empty, canonical) {
		t.Fatalf("empty provider name should match canonical request\n got: %s\nwant: %s", empty, canonical)
	}

	for _, name := range []string{"openrouter", "openai", "groq", "xai", "mistral", "together", "ollama", "lmstudio", "custom"} {
		got, err := MarshalRequestForProvider(req, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bytes.Contains(got, []byte(`"thinking"`)) {
			t.Fatalf("%s payload must omit thinking: %s", name, got)
		}
		if bytes.Contains(got, []byte(`"reasoning_effort"`)) {
			t.Fatalf("%s payload must omit reasoning_effort=none: %s", name, got)
		}
		var decoded struct {
			Stream        bool           `json:"stream"`
			StreamOptions map[string]any `json:"stream_options"`
		}
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !decoded.Stream {
			t.Fatalf("%s payload must set stream=true: %s", name, got)
		}
		if v, ok := decoded.StreamOptions["include_usage"].(bool); !ok || !v {
			t.Fatalf("%s payload must include stream_options.include_usage: %s", name, got)
		}
	}

	// A non-none reasoning effort is forwarded for OpenAI-compatible providers.
	req.ReasoningEffort = "high"
	got, err := MarshalRequestForProvider(req, "openrouter")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"reasoning_effort":"high"`)) {
		t.Fatalf("openrouter payload should keep reasoning_effort=high: %s", got)
	}

	// Tools are sorted deterministically for every provider.
	withTools := ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools: []ToolDef{
			{Name: "zeta", Parameters: json.RawMessage(`{"b":1,"a":2}`)},
			{Name: "alpha", Parameters: json.RawMessage(`{"y":1,"x":2}`)},
		},
	}
	a, err := MarshalRequestForProvider(withTools, "openrouter")
	if err != nil {
		t.Fatal(err)
	}
	b, err := MarshalRequestForProvider(withTools, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("tool ordering should not depend on provider name\n a: %s\n b: %s", a, b)
	}
	if bytes.Index(a, []byte(`"alpha"`)) > bytes.Index(a, []byte(`"zeta"`)) {
		t.Fatalf("tools not sorted: %s", a)
	}
}

func TestParseSSEStreamOpenRouterReasoning(t *testing.T) {
	stream := `data: {"choices":[{"index":0,"delta":{"reasoning":"step one"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"answer"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	out := make(chan StreamEvent, 16)
	done := make(chan error, 1)
	go func() {
		err := ParseSSEStream(context.Background(), strings.NewReader(stream), out)
		close(out)
		done <- err
	}()

	var events []StreamEvent
	for ev := range out {
		events = append(events, ev)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ParseSSEStream: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ParseSSEStream did not return")
	}

	if len(events) != 3 {
		t.Fatalf("unexpected events: %+v", events)
	}
	if events[0].Type != EventReasoningDelta || events[0].Text != "step one" {
		t.Fatalf("reasoning event = %+v", events[0])
	}
	if events[1].Type != EventTextDelta || events[1].Text != "answer" {
		t.Fatalf("text event = %+v", events[1])
	}
	if events[2].Type != EventDone || events[2].FinishReason != "stop" {
		t.Fatalf("done event = %+v", events[2])
	}
}

func TestNewOpenAICompatClientHeaders(t *testing.T) {
	type captured struct {
		auth     string
		referer  string
		title    string
		hasAuth  bool
		provider string
	}
	got := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasAuth := r.Header["Authorization"]
		got <- captured{
			auth:    r.Header.Get("Authorization"),
			referer: r.Header.Get("HTTP-Referer"),
			title:   r.Header.Get("X-Title"),
			hasAuth: hasAuth,
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewOpenAICompatClient("openrouter", srv.URL, "", map[string]string{
		"HTTP-Referer": "https://tern.example",
		"X-Title":      "tern",
	})
	if client.ProviderName != "openrouter" {
		t.Fatalf("ProviderName = %q", client.ProviderName)
	}
	ch, err := client.Stream(context.Background(), ChatRequest{
		Model:    "anthropic/claude-3.5-sonnet",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}

	select {
	case c := <-got:
		if c.hasAuth || c.auth != "" {
			t.Fatalf("empty api key must not send Authorization, got %q", c.auth)
		}
		if c.referer != "https://tern.example" {
			t.Fatalf("HTTP-Referer = %q", c.referer)
		}
		if c.title != "tern" {
			t.Fatalf("X-Title = %q", c.title)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not receive request")
	}

	// With an API key the Bearer header is present again.
	got2 := make(chan string, 1)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got2 <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv2.Close()
	client2 := NewOpenAICompatClient("openai", srv2.URL, "sk-test", nil)
	ch, err = client2.Stream(context.Background(), ChatRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	select {
	case auth := <-got2:
		if auth != "Bearer sk-test" {
			t.Fatalf("Authorization = %q", auth)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not receive request")
	}
}

func TestDefaultModelCapabilitiesMultiProvider(t *testing.T) {
	cases := []struct {
		model    string
		context  int
		vision   bool
		thinking bool
	}{
		{"anthropic/claude-3.5-sonnet", 200_000, true, true},
		{"claude-opus-4", 200_000, true, true},
		{"google/gemini-2.5-pro", 1_000_000, true, true},
		{"gemini-2.0-flash", 1_000_000, true, true},
		{"openai/gpt-4o", DefaultContextWindow, true, true},
		{"gpt-5-mini", DefaultContextWindow, true, true},
		{"o3-mini", DefaultContextWindow, true, true},
		{"o4-mini", DefaultContextWindow, true, true},
		{"deepseek-flash", 1_000_000, true, true},
		{"deepseek-v4-pro", 1_000_000, false, true},
	}
	for _, tc := range cases {
		caps := DefaultModelCapabilities(tc.model)
		if caps.Name != tc.model {
			t.Fatalf("%s: Name = %q", tc.model, caps.Name)
		}
		if caps.ContextWindow != tc.context {
			t.Fatalf("%s: ContextWindow = %d, want %d", tc.model, caps.ContextWindow, tc.context)
		}
		if caps.SupportsVision != tc.vision {
			t.Fatalf("%s: SupportsVision = %v, want %v", tc.model, caps.SupportsVision, tc.vision)
		}
		if caps.SupportsThinking != tc.thinking {
			t.Fatalf("%s: SupportsThinking = %v, want %v", tc.model, caps.SupportsThinking, tc.thinking)
		}
		if caps.MaxOutput != DefaultMaxOutput {
			t.Fatalf("%s: MaxOutput = %d, want %d", tc.model, caps.MaxOutput, DefaultMaxOutput)
		}
	}
}

func TestAPIErrorProviderName(t *testing.T) {
	defaultErr := &APIError{StatusCode: 400, Message: "bad"}
	if !strings.Contains(defaultErr.Error(), "deepseek API error") {
		t.Fatalf("default error = %q", defaultErr.Error())
	}
	dsErr := &APIError{StatusCode: 400, Message: "bad", Provider: "deepseek"}
	if !strings.Contains(dsErr.Error(), "deepseek API error") {
		t.Fatalf("deepseek error = %q", dsErr.Error())
	}
	orErr := &APIError{StatusCode: 400, Message: "bad", Provider: "openrouter"}
	if !strings.Contains(orErr.Error(), "openrouter API error") {
		t.Fatalf("openrouter error = %q", orErr.Error())
	}
}
