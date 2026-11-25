package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/provider"
)

func testRunOptions(t *testing.T) runOptions {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataDir := filepath.Join(root, "data")
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERN_CONFIG_DIR", configDir)
	t.Setenv("TERN_DATA_DIR", dataDir)
	t.Setenv("DEEPSEEK_API_KEY", "test-secret-key")
	t.Setenv("NO_COLOR", "1")
	return runOptions{
		In:        strings.NewReader(""),
		Out:       &bytes.Buffer{},
		Err:       &bytes.Buffer{},
		StdinTTY:  true,
		StdoutTTY: false,
		WorkDir:   workDir,
		Version:   "test",
	}
}

func TestParseCLIAndConfigRedaction(t *testing.T) {
	opts, err := parseCLI([]string{
		"-p", "hello", "--json", "-c", "--model", "deepseek-v4-pro",
		"--plan", "--max-cost", "1.5", "--max-agents", "2", "--debug-cache",
	}, nil)
	if err != nil {
		t.Fatalf("parseCLI: %v", err)
	}
	if opts.Prompt != "hello" || !opts.PromptSet || !opts.JSON || !opts.Continue {
		t.Fatalf("unexpected flags: %+v", opts)
	}
	if opts.Model != "deepseek-v4-pro" || !opts.ModelSet || !opts.Plan {
		t.Fatalf("model/plan flags not parsed: %+v", opts)
	}
	if opts.MaxCost != 1.5 || !opts.MaxCostSet || opts.MaxAgents != 2 || !opts.AgentsSet || !opts.CacheSet {
		t.Fatalf("numeric flags not parsed: %+v", opts)
	}

	o := testRunOptions(t)
	configDir := os.Getenv("TERN_CONFIG_DIR")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("permission_mode = \"auto-edit\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o.Args = []string{"config"}
	if code := runWithOptions(o); code != 0 {
		t.Fatalf("tern config exit %d, stderr: %s", code, o.Err.(*bytes.Buffer).String())
	}
	out := o.Out.(*bytes.Buffer).String()
	if !strings.Contains(out, `api_key = "[set]"`) {
		t.Fatalf("config output missing redacted key marker:\n%s", out)
	}
	if strings.Contains(out, "test-secret-key") {
		t.Fatalf("config output leaked API key:\n%s", out)
	}
	if !strings.Contains(out, `permission_mode = "auto-edit"`) {
		t.Fatalf("config output missing merged value:\n%s", out)
	}

	versionOpts := testRunOptions(t)
	versionOpts.Args = []string{"--version"}
	if code := runWithOptions(versionOpts); code != 0 {
		t.Fatalf("--version exit %d", code)
	}
	if got := versionOpts.Out.(*bytes.Buffer).String(); got != "tern test\n" {
		t.Fatalf("version output = %q", got)
	}
}

func TestPrintModeWithPipedStdin(t *testing.T) {
	o := testRunOptions(t)
	o.Args = []string{"-p", "say hi"}
	o.In = strings.NewReader("piped context")
	o.StdinTTY = false

	mock := provider.NewMockProvider()
	mock.QueueScript(
		provider.StreamEvent{Type: provider.EventTextDelta, Text: "hello from mock"},
		provider.StreamEvent{Type: provider.EventDone, FinishReason: "stop"},
	)
	o.Provider = mock

	if code := runWithOptions(o); code != 0 {
		t.Fatalf("print mode exit %d, stderr: %s", code, o.Err.(*bytes.Buffer).String())
	}
	out := o.Out.(*bytes.Buffer).String()
	if !strings.Contains(out, "hello from mock") {
		t.Fatalf("print output = %q", out)
	}

	req, ok := mock.LastRequest()
	if !ok {
		t.Fatal("mock provider received no request")
	}
	var userText string
	for _, msg := range req.Messages {
		if msg.Role == provider.RoleUser {
			userText = msg.Content
		}
	}
	if !strings.Contains(userText, "say hi") || !strings.Contains(userText, "piped context") {
		t.Fatalf("combined prompt missing parts: %q", userText)
	}
}

func TestJSONModeEmitsAgentAndSubagentEvents(t *testing.T) {
	o := testRunOptions(t)
	o.Args = []string{"--prompt", "delegate this", "--json", "--permission", "yolo"}

	mock := provider.NewMockProvider()
	mock.QueueScript(
		provider.StreamEvent{
			Type:          provider.EventToolCallDelta,
			ToolCallIndex: 0,
			ToolCallID:    "call_task_1",
			ToolName:      "task",
			ToolArgsDelta: `{"description":"explore the repo","prompt":"find the entry point","agent":"explore"}`,
		},
		provider.StreamEvent{Type: provider.EventDone, FinishReason: "tool_calls"},
	)
	mock.QueueScript(
		provider.StreamEvent{Type: provider.EventTextDelta, Text: "subagent report"},
		provider.StreamEvent{Type: provider.EventDone, FinishReason: "stop"},
	)
	mock.QueueScript(
		provider.StreamEvent{Type: provider.EventTextDelta, Text: "parent final"},
		provider.StreamEvent{Type: provider.EventDone, FinishReason: "stop"},
	)
	o.Provider = mock

	if code := runWithOptions(o); code != 0 {
		t.Fatalf("json mode exit %d, stderr: %s", code, o.Err.(*bytes.Buffer).String())
	}
	lines := nonEmptyLines(o.Out.(*bytes.Buffer).String())
	var sawStart, sawEnd, sawToolStart bool
	for _, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("invalid NDJSON line %q: %v", line, err)
		}
		typ, _ := ev["type"].(string)
		switch typ {
		case "tool_start":
			if ev["tool_name"] == "task" {
				sawToolStart = true
			}
		case "subagent_start":
			sawStart = true
			if ev["agent_id"] == "" || ev["agent_id"] == nil {
				t.Fatalf("subagent_start missing agent_id: %s", line)
			}
			if _, ok := ev["parent_tool_call_id"]; !ok {
				t.Fatalf("subagent_start missing parent_tool_call_id key: %s", line)
			}
		case "subagent_end":
			sawEnd = true
			if ev["agent_id"] == "" || ev["agent_id"] == nil {
				t.Fatalf("subagent_end missing agent_id: %s", line)
			}
			if _, ok := ev["parent_tool_call_id"]; !ok {
				t.Fatalf("subagent_end missing parent_tool_call_id key: %s", line)
			}
		}
	}
	if !sawToolStart || !sawStart || !sawEnd {
		t.Fatalf("missing expected events (tool_start=%v start=%v end=%v):\n%s", sawToolStart, sawStart, sawEnd, strings.Join(lines, "\n"))
	}
}

func TestRPCModePromptStatusQuit(t *testing.T) {
	o := testRunOptions(t)
	o.Args = []string{"--rpc"}
	o.In = strings.NewReader(strings.Join([]string{
		`{"id":"1","method":"prompt","params":{"text":"hello rpc"}}`,
		`{"id":"2","method":"status"}`,
		`{"id":"3","method":"quit"}`,
	}, "\n") + "\n")

	mock := provider.NewMockProvider()
	mock.QueueScript(
		provider.StreamEvent{Type: provider.EventTextDelta, Text: "rpc hello"},
		provider.StreamEvent{Type: provider.EventDone, FinishReason: "stop"},
	)
	o.Provider = mock

	if code := runWithOptions(o); code != 0 {
		t.Fatalf("rpc mode exit %d, stderr: %s", code, o.Err.(*bytes.Buffer).String())
	}
	lines := nonEmptyLines(o.Out.(*bytes.Buffer).String())
	responses := make(map[string]map[string]any)
	sawText := false
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid NDJSON line %q: %v", line, err)
		}
		if rec["type"] == "response" {
			id, _ := rec["id"].(string)
			responses[id] = rec
		}
		if rec["type"] == string(agent.EventTextDelta) && rec["text"] == "rpc hello" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("missing streamed text event:\n%s", strings.Join(lines, "\n"))
	}
	if responses["1"] == nil || responses["2"] == nil || responses["3"] == nil {
		t.Fatalf("missing RPC responses: %+v\n%s", responses, strings.Join(lines, "\n"))
	}
	if responses["1"]["error"] != nil {
		t.Fatalf("prompt failed: %+v", responses["1"])
	}
	status, ok := responses["2"]["result"].(map[string]any)
	if !ok || status["session_id"] == "" || status["model"] == "" {
		t.Fatalf("bad status response: %+v", responses["2"])
	}
	quit, ok := responses["3"]["result"].(map[string]any)
	if !ok || quit["quit"] != true {
		t.Fatalf("bad quit response: %+v", responses["3"])
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func TestSelfUpdateVerifiesChecksum(t *testing.T) {
	asset, archive := testReleaseArchive(t)
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), asset)

	newServer := func(checksumBody string) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"tag_name":"v1.2.3"}`)
		})
		mux.HandleFunc("/dl/o/r/releases/download/v1.2.3/"+asset, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(archive)
		})
		mux.HandleFunc("/dl/o/r/releases/download/v1.2.3/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, checksumBody)
		})
		return httptest.NewServer(mux)
	}

	t.Run("success", func(t *testing.T) {
		server := newServer(checksums)
		defer server.Close()
		target := filepath.Join(t.TempDir(), "tern")
		if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		tag, updated, err := performUpdate(t.Context(), updateOptions{
			Repo:           "o/r",
			CurrentVersion: "dev",
			GOOS:           runtime.GOOS,
			GOARCH:         runtime.GOARCH,
			APIBase:        server.URL + "/api",
			DownloadBase:   server.URL + "/dl",
			TargetPath:     target,
		})
		if err != nil {
			t.Fatalf("performUpdate: %v", err)
		}
		if tag != "v1.2.3" || !updated {
			t.Fatalf("tag=%q updated=%v", tag, updated)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "new-binary" {
			t.Fatalf("target = %q", got)
		}
	})

	t.Run("checksum mismatch", func(t *testing.T) {
		server := newServer(fmt.Sprintf("%064d  %s\n", 0, asset))
		defer server.Close()
		target := filepath.Join(t.TempDir(), "tern")
		if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, _, err := performUpdate(t.Context(), updateOptions{
			Repo:           "o/r",
			CurrentVersion: "dev",
			GOOS:           runtime.GOOS,
			GOARCH:         runtime.GOARCH,
			APIBase:        server.URL + "/api",
			DownloadBase:   server.URL + "/dl",
			TargetPath:     target,
		})
		if err == nil || !strings.Contains(err.Error(), "checksum verification failed") {
			t.Fatalf("expected checksum error, got %v", err)
		}
		got, _ := os.ReadFile(target)
		if string(got) != "old" {
			t.Fatalf("target changed after failed verification: %q", got)
		}
	})
}

func testReleaseArchive(t *testing.T) (string, []byte) {
	t.Helper()
	binaryName := "tern"
	if runtime.GOOS == "windows" {
		binaryName = "tern.exe"
	}
	if runtime.GOOS == "windows" {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, err := zw.Create(binaryName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("new-binary")); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return updateAssetName("1.2.3", runtime.GOOS, runtime.GOARCH), buf.Bytes()
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("new-binary")
	if err := tw.WriteHeader(&tar.Header{
		Name: binaryName,
		Mode: 0o755,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return updateAssetName("1.2.3", runtime.GOOS, runtime.GOARCH), buf.Bytes()
}
