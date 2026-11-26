package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
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

func TestStatsCommandTextAndJSON(t *testing.T) {
	o := testRunOptions(t)
	store, err := session.NewStore(os.Getenv("TERN_DATA_DIR"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	sess, err := store.Create(session.SessionMeta{
		ID:      "session-stats-text",
		Name:    "Stats Session",
		Model:   "mock-model",
		WorkDir: o.WorkDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleUser, Content: "hello stats"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := sess.AppendUsageWithDuration(provider.Usage{
		PromptTokens:          100,
		CompletionTokens:      50,
		TotalTokens:           150,
		PromptCacheHitTokens:  60,
		PromptCacheMissTokens: 40,
	}, 0.5, "hash", 10*time.Second); err != nil {
		t.Fatalf("AppendUsageWithDuration: %v", err)
	}

	o.Args = []string{"stats"}
	if code := runWithOptions(o); code != 0 {
		t.Fatalf("tern stats exit %d, stderr: %s", code, o.Err.(*bytes.Buffer).String())
	}
	text := o.Out.(*bytes.Buffer).String()
	for _, want := range []string{
		"tern stats",
		"Sessions",
		"mock-model",
		"Stats Session",
		"150",
		"60.0%",
		"10.0s",
		"5.0 tok/s",
		"$0.5000",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stats output missing %q:\n%s", want, text)
		}
	}

	jsonOut := &bytes.Buffer{}
	jsonErr := &bytes.Buffer{}
	jsonOpts := o
	jsonOpts.Args = []string{"stats", "--json"}
	jsonOpts.Out = jsonOut
	jsonOpts.Err = jsonErr
	if code := runWithOptions(jsonOpts); code != 0 {
		t.Fatalf("tern stats --json exit %d, stderr: %s", code, jsonErr.String())
	}
	var got session.AggregateStats
	if err := json.Unmarshal(jsonOut.Bytes(), &got); err != nil {
		t.Fatalf("stats JSON is invalid: %v\n%s", err, jsonOut.String())
	}
	if got.SessionCount != 1 || got.TurnCount != 1 || got.MessageCount != 1 {
		t.Fatalf("unexpected JSON counts: %+v", got)
	}
	if got.TotalUsage.TotalTokens != 150 || got.TotalUsage.PromptCacheHitTokens != 60 {
		t.Fatalf("unexpected JSON usage: %+v", got.TotalUsage)
	}
	if got.TotalCostUSD != 0.5 || got.TotalDuration != 10*time.Second {
		t.Fatalf("unexpected JSON totals: cost=%v duration=%v", got.TotalCostUSD, got.TotalDuration)
	}
	if len(got.Models) != 1 || got.Models[0].Model != "mock-model" {
		t.Fatalf("unexpected JSON models: %+v", got.Models)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].Name != "Stats Session" {
		t.Fatalf("unexpected JSON sessions: %+v", got.Sessions)
	}
}

func TestProjectLocalMCPAndHookConfigs(t *testing.T) {
	o := testRunOptions(t)
	configDir := os.Getenv("TERN_CONFIG_DIR")
	projectDir := filepath.Join(o.WorkDir, ".tern")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeFile := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(configDir, "hooks.toml"), `
[[hooks]]
event = "session_start"
command = "global-hook"
`)
	writeFile(filepath.Join(projectDir, "hooks.toml"), `
[[hooks]]
event = "before_tool"
command = "project-hook"
matcher = "bash"
`)
	writeFile(filepath.Join(configDir, "mcp.toml"), `
[[servers]]
name = "shared"
command = "global-server"

[[servers]]
name = "global-only"
url = "https://global.example/mcp"
`)
	writeFile(filepath.Join(projectDir, "mcp.toml"), `
[[servers]]
name = "shared"
command = "project-server"

[[servers]]
name = "project-only"
command = "project-only-server"
`)

	hooks, hookWarnings := loadHookConfigs(configDir, o.WorkDir)
	if len(hookWarnings) != 0 {
		t.Fatalf("unexpected hook warnings: %v", hookWarnings)
	}
	if len(hooks) != 2 {
		t.Fatalf("hooks = %+v, want 2 (global + project)", hooks)
	}
	if hooks[0].Command != "global-hook" || hooks[0].Event != "session_start" {
		t.Fatalf("global hook not loaded first: %+v", hooks[0])
	}
	if hooks[1].Command != "project-hook" || hooks[1].Event != "before_tool" || hooks[1].Matcher != "bash" {
		t.Fatalf("project hook not loaded: %+v", hooks[1])
	}

	servers, mcpWarnings := loadMCPConfigs(configDir, o.WorkDir)
	if len(mcpWarnings) != 0 {
		t.Fatalf("unexpected mcp warnings: %v", mcpWarnings)
	}
	if len(servers) != 3 {
		t.Fatalf("servers = %+v, want 3", servers)
	}
	byName := make(map[string]int, len(servers))
	for i, s := range servers {
		byName[s.Name] = i
	}
	if i, ok := byName["shared"]; !ok || servers[i].Command != "project-server" {
		t.Fatalf("project server did not override global: %+v", servers)
	}
	if i, ok := byName["global-only"]; !ok || servers[i].URL != "https://global.example/mcp" {
		t.Fatalf("global-only server missing: %+v", servers)
	}
	if i, ok := byName["project-only"]; !ok || servers[i].Command != "project-only-server" {
		t.Fatalf("project-only server missing: %+v", servers)
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

func TestUpdateCheckAgainstMockRelease(t *testing.T) {
	asset, archive := testReleaseArchive(t)
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), asset)

	var downloads int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v1.2.3"}`)
	})
	mux.HandleFunc("/dl/o/r/releases/download/v1.2.3/"+asset, func(w http.ResponseWriter, r *http.Request) {
		downloads++
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/dl/o/r/releases/download/v1.2.3/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		downloads++
		_, _ = io.WriteString(w, checksums)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("TERN_UPDATE_REPO", "o/r")

	oldAPI, oldDownload := githubAPIBase, githubDownloadBase
	githubAPIBase = server.URL + "/api"
	githubDownloadBase = server.URL + "/dl"
	t.Cleanup(func() { githubAPIBase, githubDownloadBase = oldAPI, oldDownload })

	oldVersion := version
	t.Cleanup(func() { version = oldVersion })

	version = "dev"
	o := testRunOptions(t)
	o.Args = []string{"update", "--check"}
	if code := runWithOptions(o); code != 0 {
		t.Fatalf("update --check exit %d, stderr: %s", code, o.Err.(*bytes.Buffer).String())
	}
	if out := o.Out.(*bytes.Buffer).String(); !strings.Contains(out, "Update available: v1.2.3") {
		t.Fatalf("update --check output missing availability: %s", out)
	}

	version = "v1.2.3"
	upToDate := testRunOptions(t)
	upToDate.Args = []string{"update", "--check"}
	if code := runWithOptions(upToDate); code != 0 {
		t.Fatalf("update --check (latest) exit %d, stderr: %s", code, upToDate.Err.(*bytes.Buffer).String())
	}
	if out := upToDate.Out.(*bytes.Buffer).String(); !strings.Contains(out, "tern is already up to date (v1.2.3)") {
		t.Fatalf("update --check (latest) output = %s", out)
	}
	if downloads != 0 {
		t.Fatalf("update --check downloaded %d files, want 0", downloads)
	}
}

func TestUpdateThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink update behavior is covered on Unix")
	}
	asset, archive := testReleaseArchive(t)
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), asset)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v1.2.3"}`)
	})
	mux.HandleFunc("/dl/o/r/releases/download/v1.2.3/"+asset, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/dl/o/r/releases/download/v1.2.3/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, checksums)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("TERN_UPDATE_REPO", "o/r")

	oldAPI, oldDownload := githubAPIBase, githubDownloadBase
	githubAPIBase = server.URL + "/api"
	githubDownloadBase = server.URL + "/dl"
	t.Cleanup(func() { githubAPIBase, githubDownloadBase = oldAPI, oldDownload })

	oldVersion := version
	version = "dev"
	t.Cleanup(func() { version = oldVersion })

	dir := t.TempDir()
	target := filepath.Join(dir, "tern-real")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "tern")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	oldExecutable := executablePath
	executablePath = func() (string, error) { return link, nil }
	t.Cleanup(func() { executablePath = oldExecutable })

	o := testRunOptions(t)
	o.Args = []string{"update"}
	if code := runWithOptions(o); code != 0 {
		t.Fatalf("update exit %d, stderr: %s", code, o.Err.(*bytes.Buffer).String())
	}
	if out := o.Out.(*bytes.Buffer).String(); !strings.Contains(out, "Updated tern to v1.2.3") {
		t.Fatalf("update output = %s", out)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was replaced by a regular file: %s", link)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != resolvedTarget {
		t.Fatalf("symlink now points to %s, want %s", resolved, resolvedTarget)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-binary" {
		t.Fatalf("target binary = %q, want new-binary", got)
	}
}

func TestUninstallDryRunYesAndPurge(t *testing.T) {
	setup := func(t *testing.T) (runOptions, string, string, string) {
		t.Helper()
		o := testRunOptions(t)
		configDir := os.Getenv("TERN_CONFIG_DIR")
		dataDir := os.Getenv("TERN_DATA_DIR")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("model = \"test\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "session.json"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(t.TempDir(), "tern")
		if err := os.WriteFile(binary, []byte("fake-binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		oldExecutable := executablePath
		executablePath = func() (string, error) { return binary, nil }
		t.Cleanup(func() { executablePath = oldExecutable })
		return o, binary, configDir, dataDir
	}

	t.Run("dry run", func(t *testing.T) {
		o, binary, configDir, dataDir := setup(t)
		o.Args = []string{"uninstall", "--dry-run", "--purge"}
		if code := runWithOptions(o); code != 0 {
			t.Fatalf("uninstall --dry-run exit %d, stderr: %s", code, o.Err.(*bytes.Buffer).String())
		}
		out := o.Out.(*bytes.Buffer).String()
		for _, want := range []string{binary, configDir, dataDir, "Dry run: no changes were made."} {
			if !strings.Contains(out, want) {
				t.Fatalf("dry-run output missing %q:\n%s", want, out)
			}
		}
		for _, path := range []string{binary, filepath.Join(configDir, "config.toml"), filepath.Join(dataDir, "session.json")} {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("dry run removed %s: %v", path, err)
			}
		}
	})

	t.Run("yes keeps config", func(t *testing.T) {
		o, binary, configDir, dataDir := setup(t)
		o.Args = []string{"uninstall", "--yes"}
		if code := runWithOptions(o); code != 0 {
			t.Fatalf("uninstall --yes exit %d, stderr: %s", code, o.Err.(*bytes.Buffer).String())
		}
		if _, err := os.Stat(binary); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("binary still exists: %v", err)
		}
		if _, err := os.Stat(filepath.Join(configDir, "config.toml")); err != nil {
			t.Fatalf("config was removed without --purge: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dataDir, "session.json")); err != nil {
			t.Fatalf("sessions were removed without --purge: %v", err)
		}
		out := o.Out.(*bytes.Buffer).String()
		if !strings.Contains(out, "Config and sessions were kept") || !strings.Contains(out, configDir) || !strings.Contains(out, dataDir) {
			t.Fatalf("missing kept-data note:\n%s", out)
		}
	})

	t.Run("yes purge", func(t *testing.T) {
		o, binary, configDir, dataDir := setup(t)
		o.Args = []string{"uninstall", "--yes", "--purge"}
		if code := runWithOptions(o); code != 0 {
			t.Fatalf("uninstall --yes --purge exit %d, stderr: %s", code, o.Err.(*bytes.Buffer).String())
		}
		for _, path := range []string{binary, configDir, dataDir} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s still exists: %v", path, err)
			}
		}
	})

	t.Run("non-tty requires yes", func(t *testing.T) {
		o, binary, _, _ := setup(t)
		o.Args = []string{"uninstall"}
		o.StdinTTY = false
		if code := runWithOptions(o); code != 1 {
			t.Fatalf("uninstall without --yes exit %d, want 1", code)
		}
		if !strings.Contains(o.Err.(*bytes.Buffer).String(), "--yes") {
			t.Fatalf("stderr should advise --yes: %s", o.Err.(*bytes.Buffer).String())
		}
		if _, err := os.Stat(binary); err != nil {
			t.Fatalf("binary was removed without confirmation: %v", err)
		}
	})
}
