package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/ext"
	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
	"github.com/tern-agent/tern/internal/subagent"
	"github.com/tern-agent/tern/internal/tools"
)

func newTestModel(t *testing.T, opts Options) *Model {
	t.Helper()
	dir := t.TempDir()
	if opts.Config == nil {
		opts.Config = config.Default()
	}
	if opts.WorkDir == "" {
		opts.WorkDir = dir
	}
	if opts.GlobalConfigDir == "" {
		opts.GlobalConfigDir = filepath.Join(dir, "config")
	}
	opts.NoColor = true
	m := New(opts)
	m.width = 120
	m.height = 32
	return m
}

func typeText(t *testing.T, m *Model, text string) {
	t.Helper()
	for _, r := range text {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func press(t *testing.T, m *Model, keyType tea.KeyType) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(tea.KeyMsg{Type: keyType})
	return cmd
}

func submitText(t *testing.T, m *Model, text string) tea.Cmd {
	t.Helper()
	typeText(t, m, text)
	return press(t, m, tea.KeyEnter)
}

func TestSlashCommandDispatch(t *testing.T) {
	t.Run("help", func(t *testing.T) {
		m := newTestModel(t, Options{})
		submitText(t, m, "/help")
		view := m.View()
		for _, want := range []string{"/model", "/agents", "/compact", "/quit"} {
			if !strings.Contains(view, want) {
				t.Fatalf("help view missing %q:\n%s", want, view)
			}
		}
	})

	t.Run("model", func(t *testing.T) {
		m := newTestModel(t, Options{})
		submitText(t, m, "/model deepseek-v4-pro")
		if got := m.opts.Config.Model; got != "deepseek-v4-pro" {
			t.Fatalf("model = %q, want deepseek-v4-pro", got)
		}
		if got := m.opts.Agent.Config.Model; got != "deepseek-v4-pro" {
			t.Fatalf("agent model = %q, want deepseek-v4-pro", got)
		}
		if !strings.Contains(m.View(), "Model set to deepseek-v4-pro") {
			t.Fatalf("view does not confirm model change:\n%s", m.View())
		}
	})

	t.Run("model listing", func(t *testing.T) {
		m := newTestModel(t, Options{})
		submitText(t, m, "/model")
		view := m.View()
		if !strings.Contains(view, "deepseek-flash") || !strings.Contains(view, "Current model") {
			t.Fatalf("model listing missing content:\n%s", view)
		}
	})

	t.Run("plan", func(t *testing.T) {
		m := newTestModel(t, Options{})
		submitText(t, m, "/plan")
		if got := m.opts.Agent.Permissions.Mode(); got != permission.ModePlan {
			t.Fatalf("mode = %q, want plan", got)
		}
		submitText(t, m, "/plan")
		if got := m.opts.Agent.Permissions.Mode(); got != permission.ModeAsk {
			t.Fatalf("mode after toggle = %q, want ask", got)
		}
	})

	t.Run("cost", func(t *testing.T) {
		m := newTestModel(t, Options{})
		m.usage.PromptTokens = 1200
		m.usage.CompletionTokens = 300
		m.usage.PromptCacheHitTokens = 900
		m.cache = 0.75
		m.totalCost = 0.0123
		submitText(t, m, "/cost")
		view := m.View()
		for _, want := range []string{"Cost: $0.0123", "1200 prompt", "75%"} {
			if !strings.Contains(view, want) {
				t.Fatalf("cost view missing %q:\n%s", want, view)
			}
		}
	})

	t.Run("context", func(t *testing.T) {
		m := newTestModel(t, Options{})
		m.ctxTokens = 25_000
		m.ctxWindow = 100_000
		submitText(t, m, "/context")
		view := m.View()
		if !strings.Contains(view, "Context: ~25000 / 100000 tokens") {
			t.Fatalf("context view missing usage:\n%s", view)
		}
	})

	t.Run("config", func(t *testing.T) {
		m := newTestModel(t, Options{})
		submitText(t, m, "/config")
		view := m.View()
		if !strings.Contains(view, "api_key") || !strings.Contains(view, "base_url") {
			t.Fatalf("config view missing fields:\n%s", view)
		}
		if strings.Contains(view, m.opts.Config.APIKey) && m.opts.Config.APIKey != "" {
			t.Fatalf("config leaked the API key")
		}
	})

	t.Run("quit", func(t *testing.T) {
		m := newTestModel(t, Options{})
		cmd := submitText(t, m, "/quit")
		if cmd == nil {
			t.Fatal("quit returned nil command")
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("quit command returned %T, want tea.QuitMsg", cmd())
		}
	})

	t.Run("unknown", func(t *testing.T) {
		m := newTestModel(t, Options{})
		submitText(t, m, "/does-not-exist")
		if !strings.Contains(m.View(), "Unknown command") {
			t.Fatalf("unknown command not reported:\n%s", m.View())
		}
	})
}

func TestCtrlCIdleQuitsOnSecondPress(t *testing.T) {
	m := newTestModel(t, Options{})
	if cmd := press(t, m, tea.KeyCtrlC); cmd != nil {
		t.Fatalf("first ctrl+c returned a command, want nil")
	}
	if !strings.Contains(m.View(), "Press Ctrl+C again to quit") {
		t.Fatalf("first ctrl+c did not show quit hint:\n%s", m.View())
	}
	cmd := press(t, m, tea.KeyCtrlC)
	if cmd == nil {
		t.Fatal("second ctrl+c returned nil")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("second ctrl+c returned %T, want tea.QuitMsg", cmd())
	}
}

func TestCtrlCCancelsRunningTurn(t *testing.T) {
	m := newTestModel(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	m.running = true
	m.turnCtx = ctx
	m.cancel = cancel

	if cmd := press(t, m, tea.KeyCtrlC); cmd != nil {
		t.Fatalf("ctrl+c during a turn returned a command, want nil")
	}
	if m.running {
		t.Fatal("turn still marked running after ctrl+c")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("turn context was not cancelled")
	}
	if m.quitArmed {
		t.Fatal("ctrl+c during a turn armed quit")
	}

	// A following ctrl+c when idle should only arm quitting, not exit.
	if cmd := press(t, m, tea.KeyCtrlC); cmd != nil {
		t.Fatalf("ctrl+c after cancellation returned a command, want nil")
	}
}

func TestFileAndCommandCompletion(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"main.go":              "package main\n",
		"docs/readme.md":       "# docs\n",
		"node_modules/skip.js": "// skip\n",
		".git/config":          "[core]\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := newTestModel(t, Options{WorkDir: dir})

	typeText(t, m, "@ma")
	press(t, m, tea.KeyTab)
	if got := m.ed.Value(); !strings.Contains(got, "@main.go") {
		t.Fatalf("@file completion = %q, want to contain @main.go", got)
	}

	m.ed.Reset()
	typeText(t, m, "/he")
	press(t, m, tea.KeyTab)
	if got := m.ed.Value(); !strings.Contains(got, "/help") {
		t.Fatalf("/command completion = %q, want to contain /help", got)
	}
}

func TestSubagentEventsExpandAndCollapse(t *testing.T) {
	m := newTestModel(t, Options{})
	m.Update(subagentEventMsg{event: subagent.Event{
		Type:        subagent.EventSubagentStart,
		AgentID:     "agent-1",
		AgentType:   subagent.AgentExplore,
		Description: "find the parser",
		Status:      subagent.StatusRunning,
	}})
	m.Update(subagentEventMsg{event: subagent.Event{
		Type:        subagent.EventSubagentDelta,
		AgentID:     "agent-1",
		AgentType:   subagent.AgentExplore,
		Description: "find the parser",
		Status:      subagent.StatusRunning,
		Detail:      "reading parser.go",
	}})
	m.Update(tickMsg(time.Now()))
	if len(m.agentOrder) != 1 {
		t.Fatalf("agent count = %d, want 1", len(m.agentOrder))
	}
	if !strings.Contains(m.View(), "explore") {
		t.Fatalf("agent block not rendered:\n%s", m.View())
	}

	press(t, m, tea.KeyTab)
	if !m.agentsOpen {
		t.Fatal("Tab did not expand the focused sub-agent")
	}
	if !strings.Contains(m.View(), "reading parser.go") {
		t.Fatalf("expanded agent missing live output:\n%s", m.View())
	}

	press(t, m, tea.KeyEsc)
	if m.agentsOpen {
		t.Fatal("Esc did not collapse the sub-agent view")
	}

	m.Update(subagentEventMsg{event: subagent.Event{
		Type:        subagent.EventSubagentEnd,
		AgentID:     "agent-1",
		AgentType:   subagent.AgentExplore,
		Description: "find the parser",
		Status:      subagent.StatusDone,
		Summary:     "found it",
	}})
	view := m.View()
	if !strings.Contains(view, "explore") {
		t.Fatalf("finished agent disappeared:\n%s", view)
	}
}

func TestAgentsPanel(t *testing.T) {
	m := newTestModel(t, Options{})
	m.Update(subagentEventMsg{event: subagent.Event{
		Type:        subagent.EventSubagentStart,
		AgentID:     "agent-1",
		AgentType:   subagent.AgentResearch,
		Description: "look up docs",
		Status:      subagent.StatusRunning,
	}})
	submitText(t, m, "/agents")
	if !m.agentsPanel {
		t.Fatal("/agents did not open the panel")
	}
	view := m.View()
	if !strings.Contains(view, "Sub-agents") || !strings.Contains(view, "agent-1") {
		t.Fatalf("agents panel missing content:\n%s", view)
	}
	press(t, m, tea.KeyEsc)
	if m.agentsPanel {
		t.Fatal("Esc did not close the agents panel")
	}
}

func TestApprovalPromptAllowAndDeny(t *testing.T) {
	t.Run("parent allow", func(t *testing.T) {
		m := newTestModel(t, Options{})
		response := make(chan approvalResult, 1)
		req := &agent.ApprovalRequest{ToolName: "bash", Command: "rm -rf build", Reason: "mutating command"}
		m.Update(approvalRequestMsg{request: &approvalState{parent: req, response: response}})
		view := m.View()
		for _, want := range []string{"Permission Required", "bash", "rm -rf build", "mutating command"} {
			if !strings.Contains(view, want) {
				t.Fatalf("approval view missing %q:\n%s", want, view)
			}
		}
		press(t, m, tea.KeyEnter)
		select {
		case result := <-response:
			if !result.allow {
				t.Fatal("approval was denied, want allow")
			}
		case <-time.After(time.Second):
			t.Fatal("approval response was not delivered")
		}
		if m.pendingApproval != nil {
			t.Fatal("approval prompt was not cleared")
		}
	})

	t.Run("subagent deny", func(t *testing.T) {
		m := newTestModel(t, Options{})
		response := make(chan approvalResult, 1)
		req := &subagent.ApprovalRequest{
			AgentID:         "agent-2",
			AgentType:       subagent.AgentGeneral,
			TaskDescription: "refactor the parser",
			ToolName:        "edit",
			Detail:          "parser.go",
			DiffPreview:     "--- a/parser.go\n+++ b/parser.go\n+new line",
			Reason:          "writes files",
		}
		m.Update(approvalRequestMsg{request: &approvalState{sub: req, response: response}})
		view := m.View()
		for _, want := range []string{"general", "refactor the parser", "edit", "parser.go", "+new line"} {
			if !strings.Contains(view, want) {
				t.Fatalf("subagent approval view missing %q:\n%s", want, view)
			}
		}
		press(t, m, tea.KeyRunes)
		// The first KeyRunes press in this helper has no runes; send 'n'.
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		select {
		case result := <-response:
			if result.allow {
				t.Fatal("approval was allowed, want deny")
			}
		case <-time.After(time.Second):
			t.Fatal("approval response was not delivered")
		}
	})
}

func TestAskUserPrompt(t *testing.T) {
	m := newTestModel(t, Options{})
	response := make(chan string, 1)
	req := &askState{
		question: "Which package?",
		options:  []string{"tui", "agent"},
		response: response,
	}
	m.Update(askUserMsg{request: req})
	if !strings.Contains(m.View(), "Which package?") {
		t.Fatalf("ask prompt missing question:\n%s", m.View())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case answer := <-response:
		if answer != "agent" {
			t.Fatalf("answer = %q, want agent", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("ask response was not delivered")
	}
}

func TestNoColorRenderingHasNoANSI(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	dir := t.TempDir()
	m := newTestModel(t, Options{WorkDir: dir})
	m.addInfo("plain info")
	m.addError("plain error")
	m.Update(agentEventMsg{event: agent.Event{Type: agent.EventTextDelta, Text: "**bold** and `code`\n\n```go\nfunc main() {}\n```"}})
	m.Update(tickMsg(time.Now()))
	view := m.View()
	if strings.Contains(view, "\x1b[") {
		t.Fatalf("NO_COLOR view contains ANSI escapes: %q", view)
	}
	for _, r := range view {
		if r > 127 {
			t.Fatalf("NO_COLOR view contains non-ASCII rune %q in %q", r, view)
		}
	}

	md := RenderMarkdown("# Title\n\n**bold** and *italic* and `code`\n\n```go\nfunc main() { return }\n```", 80, true)
	if strings.Contains(md, "\x1b[") {
		t.Fatalf("plain markdown contains ANSI escapes: %q", md)
	}
	if !strings.Contains(md, "func main()") {
		t.Fatalf("markdown lost code: %q", md)
	}
}

func TestMarkdownHighlightingAndStructure(t *testing.T) {
	rendered := RenderMarkdownWithOptions("# Heading\n\n- one\n- two\n\n> quote\n\n```go\n// comment\nfunc hello() { return \"world\" }\n```", MarkdownOptions{Width: 80})
	for _, want := range []string{"Heading", "one", "quote", "func hello", "world"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered markdown missing %q:\n%s", want, rendered)
		}
	}
}

func TestReasoningAndToolToggle(t *testing.T) {
	m := newTestModel(t, Options{})
	m.Update(agentEventMsg{event: agent.Event{Type: agent.EventReasoningDelta, Text: "thinking about it"}})
	m.Update(agentEventMsg{event: agent.Event{
		Type:       agent.EventToolStart,
		ToolName:   "bash",
		ToolCallID: "call-1",
		Args:       []byte(`{"command":"go test ./..."}`),
	}})
	m.Update(tickMsg(time.Now()))
	view := m.View()
	if !strings.Contains(view, "^T expand") {
		t.Fatalf("collapsed reasoning not shown:\n%s", view)
	}
	if !strings.Contains(view, "go test ./...") {
		t.Fatalf("tool summary missing command:\n%s", view)
	}
	if strings.Contains(view, "thinking about it") {
		t.Fatalf("reasoning text visible while collapsed:\n%s", view)
	}

	press(t, m, tea.KeyCtrlT)
	view = m.View()
	if !strings.Contains(view, "thinking about it") {
		t.Fatalf("reasoning not visible after Ctrl+T:\n%s", view)
	}
}

func TestCustomSlashCommandDispatch(t *testing.T) {
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, ".tern", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmdDir, "explain.md"), []byte("Explain $ARGUMENTS in detail.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{WorkDir: dir})
	if len(m.commands) != 1 || m.commands[0].Name != "explain" {
		t.Fatalf("custom commands not loaded: %+v", m.commands)
	}
	// The command expands into a prompt; execution is delegated to a turn.
	m.baseCtx = context.Background()
	submitText(t, m, "/explain parser")
	if len(m.entries) == 0 {
		t.Fatal("custom command did not append a prompt")
	}
	if got := m.entries[0].text; got != "Explain parser in detail." {
		t.Fatalf("expanded prompt = %q", got)
	}
}

func TestStatusBarFormat(t *testing.T) {
	m := newTestModel(t, Options{GitBranch: "main"})
	view := m.View()
	// Idle zero segments (tok/s, ctx, cache, cost) are omitted for calmness.
	want := "deepseek-flash (high) | ask | main"
	if !strings.Contains(view, want) {
		t.Fatalf("status bar missing %q:\n%s", want, view)
	}
	if strings.Contains(view, "0.0 tok/s") || strings.Contains(view, "$0.000") {
		t.Fatalf("status bar should omit idle zero segments:\n%s", view)
	}
}

func TestPastedFileAndImagePaths(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(image, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{WorkDir: dir})

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(source), Paste: true})
	if got := m.ed.Value(); got != "@main.go" {
		t.Fatalf("pasted source path = %q, want @main.go", got)
	}

	m.ed.Reset()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(image), Paste: true})
	if got := m.ed.Value(); !strings.Contains(got, "[image: shot.png]") {
		t.Fatalf("pasted image marker = %q", got)
	}
	parts := m.ed.TakeAttachments()
	if len(parts) != 1 || parts[0].ImageURL == nil {
		t.Fatalf("image attachment not created: %+v", parts)
	}
	if !strings.HasPrefix(parts[0].ImageURL.URL, "data:image/png;base64,") {
		t.Fatalf("image data URL = %q", parts[0].ImageURL.URL)
	}
}

func TestReverseHistorySearch(t *testing.T) {
	m := newTestModel(t, Options{})
	m.ed.AddHistory("/help")
	m.ed.AddHistory("/model")
	m.ed.AddHistory("/cost")

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("mo")})
	if got := m.ed.Value(); got != "/model" {
		t.Fatalf("reverse search value = %q, want /model", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.ed.SearchActive() {
		t.Fatal("reverse search still active after enter")
	}
	if got := m.ed.Value(); got != "/model" {
		t.Fatalf("accepted search value = %q, want /model", got)
	}
}

// typeWithSpace types text using tea.KeySpace for spaces, mirroring Bubble Tea
// v1's key encoding for the physical spacebar.
func typeWithSpace(t *testing.T, m *Model, text string) {
	t.Helper()
	for _, r := range text {
		if r == ' ' {
			m.Update(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestSpacebarInEditorAndHistorySearch(t *testing.T) {
	m := newTestModel(t, Options{})
	typeWithSpace(t, m, "hello world")
	if got := m.ed.Value(); got != "hello world" {
		t.Fatalf("editor value = %q, want %q", got, "hello world")
	}

	m.ed.Reset()
	m.ed.AddHistory("deploy staging now")
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	typeWithSpace(t, m, "staging now")
	if got := m.ed.SearchQuery(); got != "staging now" {
		t.Fatalf("search query = %q, want %q", got, "staging now")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.ed.Value(); got != "deploy staging now" {
		t.Fatalf("accepted search value = %q", got)
	}
}

func TestSpacebarInAskPrompt(t *testing.T) {
	m := newTestModel(t, Options{})
	response := make(chan string, 1)
	m.Update(askUserMsg{request: &askState{question: "Name?", response: response}})
	typeWithSpace(t, m, "hello world")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case got := <-response:
		if got != "hello world" {
			t.Fatalf("ask answer = %q, want %q", got, "hello world")
		}
	case <-time.After(time.Second):
		t.Fatal("ask response not delivered")
	}
}

func TestSpacebarInSessionFilter(t *testing.T) {
	store, dir := newSessionStore(t)
	m := newTestModel(t, Options{SessionStore: store, WorkDir: dir})
	submitText(t, m, "/sessions")
	if m.activeModal != modalSessions {
		t.Fatal("/sessions did not open the session modal")
	}
	typeWithSpace(t, m, "my session")
	if got := m.modal.filter; got != "my session" {
		t.Fatalf("session filter = %q, want %q", got, "my session")
	}
	if !strings.Contains(m.View(), "my session") {
		t.Fatalf("filter not rendered:\n%s", m.View())
	}
}

func TestLiveTokensPerSecondIndicator(t *testing.T) {
	m := newTestModel(t, Options{})
	m.running = true
	m.turnStart = time.Now().Add(-2 * time.Second)
	m.speedStart = time.Now().Add(-2 * time.Second)
	m.Update(agentEventMsg{event: agent.Event{Type: agent.EventTextDelta, Text: strings.Repeat("word ", 200)}})
	m.spinnerFrame = 0
	view := m.View()
	if !strings.Contains(view, "tok/s") {
		t.Fatalf("live view missing tok/s indicator:\n%s", view)
	}
	if m.liveSpeed <= 0 {
		t.Fatalf("liveSpeed = %v, want > 0", m.liveSpeed)
	}
	m.Update(agentEventMsg{event: agent.Event{Type: agent.EventTurnEnd, TokensPerSec: 54}})
	if !strings.Contains(m.View(), "54 tok/s") {
		t.Fatalf("status bar missing exact tok/s value:\n%s", m.View())
	}
}

func TestModelSelectorChangesModelAndEffort(t *testing.T) {
	m := newTestModel(t, Options{})
	m.opts.Config.ReasoningEffort = "high"
	submitText(t, m, "/model")
	if m.activeModal != modalModel {
		t.Fatal("/model did not open the model selector")
	}
	view := m.View()
	for _, want := range []string{"Current model", "deepseek-flash", "deepseek-v4-pro", "1M", "vision", "off", "peak"} {
		if !strings.Contains(view, want) {
			t.Fatalf("model selector missing %q:\n%s", want, view)
		}
	}
	press(t, m, tea.KeyDown)
	press(t, m, tea.KeyRight)
	press(t, m, tea.KeyEnter)
	if m.activeModal != modalNone {
		t.Fatal("enter did not close the model selector")
	}
	if got := m.opts.Config.Model; got != "deepseek-v4-pro" {
		t.Fatalf("config model = %q, want deepseek-v4-pro", got)
	}
	if got := m.opts.Agent.Config.Model; got != "deepseek-v4-pro" {
		t.Fatalf("agent model = %q, want deepseek-v4-pro", got)
	}
	if got := m.opts.Config.ReasoningEffort; got != "max" {
		t.Fatalf("config effort = %q, want max", got)
	}
	if got := m.opts.Agent.ReasoningEffort(); got != "max" {
		t.Fatalf("agent effort = %q, want max", got)
	}
}

func TestModelShortcutStillWorks(t *testing.T) {
	m := newTestModel(t, Options{})
	submitText(t, m, "/model deepseek-v4-pro")
	if got := m.opts.Config.Model; got != "deepseek-v4-pro" {
		t.Fatalf("model = %q", got)
	}
	if m.activeModal != modalNone {
		t.Fatal("direct /model shortcut should not open the selector")
	}
}

func TestCtrlPOpensModelSelector(t *testing.T) {
	m := newTestModel(t, Options{})
	press(t, m, tea.KeyCtrlP)
	if m.activeModal != modalModel {
		t.Fatal("Ctrl+P did not open the model selector")
	}
	press(t, m, tea.KeyEsc)
	if m.activeModal != modalNone {
		t.Fatal("Esc did not close the model selector")
	}
}

func newSessionStore(t *testing.T) (*session.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func TestSessionsModalSwitchAndCreate(t *testing.T) {
	store, dir := newSessionStore(t)
	first, err := store.Create(session.SessionMeta{Name: "first session", Model: "deepseek-flash", WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.AppendMessage(providerMessage("user", "hello from first")); err != nil {
		t.Fatal(err)
	}
	if err := first.AppendMessage(providerMessage("assistant", "first reply")); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t, Options{SessionStore: store, WorkDir: dir})
	submitText(t, m, "/sessions")
	if m.activeModal != modalSessions {
		t.Fatal("/sessions did not open the session manager")
	}
	view := m.View()
	if !strings.Contains(view, "first session") || !strings.Contains(view, "Sessions") {
		t.Fatalf("session manager missing content:\n%s", view)
	}
	press(t, m, tea.KeyEnter)
	if m.opts.Session == nil || m.opts.Session.ID() != first.ID() {
		t.Fatalf("active session = %v, want %s", m.opts.Session, first.ID())
	}
	if m.activeModal != modalNone {
		t.Fatal("opening a session did not close the modal")
	}
	if !strings.Contains(m.View(), "hello from first") || !strings.Contains(m.View(), "first reply") {
		t.Fatalf("transcript was not rebuilt from replay:\n%s", m.View())
	}

	submitText(t, m, "/sessions")
	pressRune(t, m, "n")
	if m.opts.Session == nil || m.opts.Session.ID() == first.ID() {
		t.Fatal("n did not create and switch to a fresh session")
	}
	for _, entry := range m.entries {
		if entry.kind == entryUser {
			t.Fatalf("fresh session transcript contains a user message: %+v", entry)
		}
	}
}

func TestSessionsModalRenameForkDeleteAndFilter(t *testing.T) {
	store, dir := newSessionStore(t)
	first, err := store.Create(session.SessionMeta{Name: "alpha session", Model: "deepseek-flash", WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{SessionStore: store, WorkDir: dir})
	submitText(t, m, "/sessions")

	// Filter includes a space via the spacebar.
	typeWithSpace(t, m, "alpha")
	if got := m.modal.filter; got != "alpha" {
		t.Fatalf("filter = %q", got)
	}
	// Clear the filter and rename the selected session.
	for range []rune("alpha") {
		press(t, m, tea.KeyBackspace)
	}
	pressRune(t, m, "r")
	typeWithSpace(t, m, "renamed session")
	press(t, m, tea.KeyEnter)
	opened, err := store.Open(first.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got := opened.Name(); got != "renamed session" {
		t.Fatalf("renamed session = %q", got)
	}

	// Fork the selected session and switch to the fork. The rename flow keeps
	// the manager open, so no extra /sessions is needed.
	pressRune(t, m, "f")
	if m.opts.Session == nil || m.opts.Session.ID() == first.ID() {
		t.Fatal("f did not switch to the fork")
	}
	forkID := m.opts.Session.ID()

	// A non-active session can be deleted after confirmation.
	submitText(t, m, "/sessions")
	typeWithSpace(t, m, "/renamed")
	if len(m.filteredSessions()) < 2 {
		t.Fatalf("expected parent and fork in filter, got %d", len(m.filteredSessions()))
	}
	for range []rune("renamed") {
		press(t, m, tea.KeyBackspace)
	}
	// Select the row that is not active.
	for _, item := range m.filteredSessions() {
		if item.ID != forkID {
			m.modal.index = indexOfSession(m.filteredSessions(), item.ID)
			break
		}
	}
	pressRune(t, m, "d")
	if !m.modal.confirmDelete {
		t.Fatal("d did not arm delete confirmation")
	}
	pressRune(t, m, "y")
	if _, err := store.Open(first.ID()); err == nil {
		t.Fatal("session still exists after confirmed delete")
	}
}

func TestSessionsModalPreventsDeletingActiveSession(t *testing.T) {
	store, dir := newSessionStore(t)
	sess, err := store.Create(session.SessionMeta{Name: "active", Model: "deepseek-flash", WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{SessionStore: store, Session: sess, WorkDir: dir})
	submitText(t, m, "/sessions")
	pressRune(t, m, "d")
	if m.modal.confirmDelete {
		t.Fatal("active session should not be armed for deletion")
	}
	if !strings.Contains(m.View(), "Cannot delete the active session") {
		t.Fatalf("missing active-session protection notice:\n%s", m.View())
	}
}

func TestStatsModalRenderingAndResume(t *testing.T) {
	store, dir := newSessionStore(t)
	sess, err := store.Create(session.SessionMeta{Name: "stats session", Model: "deepseek-flash", WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(providerMessage("user", "measure me")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(sessionUsage(1000, 500, 200, 100), 0.002, ""); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{SessionStore: store, WorkDir: dir})
	submitText(t, m, "/stats")
	if m.activeModal != modalStats {
		t.Fatal("/stats did not open the usage modal")
	}
	view := m.View()
	for _, want := range []string{"Usage & activity", "Sessions", "Turns", "Tokens", "Cache", "Activity", "Speed", "Models", "Recent", "stats session"} {
		if !strings.Contains(view, want) {
			t.Fatalf("stats view missing %q:\n%s", want, view)
		}
	}
	for _, want := range []string{"█", "░", "▁"} {
		if !strings.Contains(view, want) {
			t.Fatalf("stats view missing visual gauge/sparkline %q:\n%s", want, view)
		}
	}
	if lines := strings.Count(view, "\n"); lines > m.height {
		t.Fatalf("stats view taller than the terminal (%d lines):\n%s", lines, view)
	}
	press(t, m, tea.KeyEnter)
	if m.opts.Session == nil || m.opts.Session.ID() != sess.ID() {
		t.Fatal("enter in stats did not resume the selected session")
	}
}

func TestSkillsPanelAndTemplateCreation(t *testing.T) {
	dir := t.TempDir()
	global := t.TempDir()
	skillDir := filepath.Join(dir, ".tern", "skills")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(skillDir, "review.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: review\ndescription: Review code carefully\n---\n\nCheck edge cases.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{WorkDir: dir, GlobalConfigDir: global})
	submitText(t, m, "/skills")
	if m.activeModal != modalSkills {
		t.Fatal("/skills did not open the skills panel")
	}
	view := m.View()
	if !strings.Contains(view, "review") || !strings.Contains(view, "Check edge cases") || !strings.Contains(view, ".tern/skills") {
		t.Fatalf("skills panel missing content:\n%s", view)
	}
	press(t, m, tea.KeyEnter)
	if m.activeModal != modalNone {
		t.Fatal("enter did not close the skills panel")
	}
	if got := m.ed.Value(); !strings.Contains(got, "Check edge cases") {
		t.Fatalf("skill body not loaded into editor: %q", got)
	}

	m.ed.Reset()
	submitText(t, m, "/skills")
	pressRune(t, m, "c")
	example := filepath.Join(skillDir, "example.md")
	if _, err := os.Stat(example); err != nil {
		t.Fatalf("starter skill not created: %v", err)
	}
}

func TestExtPanelEmptyAndInstalled(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		m := newTestModel(t, Options{})
		submitText(t, m, "/ext")
		if m.activeModal != modalExt {
			t.Fatal("/ext did not open the extensions panel")
		}
		view := m.View()
		for _, want := range []string{"~/.config/tern/extensions/", "./.tern/extensions/", "tern ext install <path-or-git-url>"} {
			if !strings.Contains(view, want) {
				t.Fatalf("empty extensions view missing %q:\n%s", want, view)
			}
		}
		press(t, m, tea.KeyEsc)
		if m.activeModal != modalNone {
			t.Fatal("esc did not close the extensions panel")
		}
	})

	t.Run("project extension", func(t *testing.T) {
		dir := t.TempDir()
		extDir := filepath.Join(dir, ".tern", "extensions", "demo")
		if err := os.MkdirAll(extDir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := "name = \"demo\"\nversion = \"1.2.3\"\ndescription = \"Demo extension for tests\"\n\n[[tools]]\nname = \"hello\"\ncommand = \"echo\"\n"
		if err := os.WriteFile(filepath.Join(extDir, "extension.toml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		m := newTestModel(t, Options{WorkDir: dir})
		submitText(t, m, "/ext")
		if m.activeModal != modalExt {
			t.Fatal("/ext did not open the extensions panel")
		}
		view := m.View()
		for _, want := range []string{"demo", "1.2.3", "project", "enabled", "1 tool(s)", "Demo extension for tests"} {
			if !strings.Contains(view, want) {
				t.Fatalf("extensions view missing %q:\n%s", want, view)
			}
		}
		press(t, m, tea.KeyEsc)
		if m.activeModal != modalNone {
			t.Fatal("esc did not close the extensions panel")
		}
	})
}

func TestWelcomeShowsRepoSummary(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"main.go", "util.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := newTestModel(t, Options{WorkDir: dir})
	// Indexing runs asynchronously via Init so New never blocks startup.
	if m.repoSummary != "" {
		t.Fatalf("expected repo summary to load asynchronously, got %q", m.repoSummary)
	}
	if cmd := m.Init(); cmd != nil {
		if batch, ok := cmd().(tea.BatchMsg); ok {
			for _, sub := range batch {
				if sub == nil {
					continue
				}
				if msg, ok := sub().(repoSummaryMsg); ok {
					m.Update(msg)
				}
			}
		}
	}
	if !strings.Contains(m.repoSummary, "indexed") {
		t.Fatalf("repo summary not computed: %q", m.repoSummary)
	}
	welcome := m.renderWelcome()
	if !strings.Contains(welcome, m.repoSummary) {
		t.Fatalf("welcome view missing repo summary %q:\n%s", m.repoSummary, welcome)
	}
	if !strings.Contains(welcome, ".gitignore respected") {
		t.Fatalf("welcome repo summary missing ignore note:\n%s", welcome)
	}
}

func TestMCPPanelEmptyAndConfigured(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		m := newTestModel(t, Options{})
		submitText(t, m, "/mcp")
		if m.activeModal != modalMCP {
			t.Fatal("/mcp did not open the MCP panel")
		}
		view := m.View()
		for _, want := range []string{"No MCP servers configured", "[[servers]]", "mcp.toml", ".tern/mcp.toml"} {
			if !strings.Contains(view, want) {
				t.Fatalf("empty MCP view missing %q:\n%s", want, view)
			}
		}
	})

	t.Run("configured", func(t *testing.T) {
		manager := ext.NewManager(ext.ServerConfig{Name: "files", Transport: ext.TransportStdio, Command: "true"})
		m := newTestModel(t, Options{MCP: manager})
		submitText(t, m, "/mcp")
		view := m.View()
		if !strings.Contains(view, "files") || !strings.Contains(view, "stdio") {
			t.Fatalf("configured MCP view missing server:\n%s", view)
		}
	})
}

func TestNoColorModalOutputIsANSIFree(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	store, dir := newSessionStore(t)
	if _, err := store.Create(session.SessionMeta{Name: "plain session", Model: "deepseek-flash", WorkDir: dir}); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{SessionStore: store, WorkDir: dir})
	for _, command := range []string{"/model", "/sessions", "/stats", "/skills", "/mcp"} {
		submitText(t, m, command)
		view := m.View()
		if strings.Contains(view, "\x1b[") {
			t.Fatalf("%s view contains ANSI escapes: %q", command, view)
		}
		press(t, m, tea.KeyEsc)
		if m.activeModal != modalNone {
			t.Fatalf("%s did not close on Esc", command)
		}
	}
}

func pressRune(t *testing.T, m *Model, text string) {
	t.Helper()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
}

func indexOfSession(items []modalSession, id string) int {
	for i, item := range items {
		if item.ID == id {
			return i
		}
	}
	return 0
}

func providerMessage(role, content string) provider.Message {
	return provider.Message{Role: provider.Role(role), Content: content}
}

func sessionUsage(prompt, completion, hit, miss int) provider.Usage {
	return provider.Usage{
		PromptTokens:          prompt,
		CompletionTokens:      completion,
		TotalTokens:           prompt + completion,
		PromptCacheHitTokens:  hit,
		PromptCacheMissTokens: miss,
	}
}

func TestShiftTabCyclesReasoningEffort(t *testing.T) {
	m := newTestModel(t, Options{})
	m.opts.Config.ReasoningEffort = "high"
	m.opts.Agent.Config.ReasoningEffort = "high"

	press(t, m, tea.KeyShiftTab)
	if got := m.opts.Config.ReasoningEffort; got != "max" {
		t.Fatalf("effort after shift+tab = %q, want max", got)
	}
	if got := m.opts.Agent.ReasoningEffort(); got != "max" {
		t.Fatalf("agent effort after shift+tab = %q, want max", got)
	}
	if !strings.Contains(m.View(), "Reasoning effort: max") {
		t.Fatalf("status bar missing effort update:\n%s", m.View())
	}

	press(t, m, tea.KeyShiftTab)
	if got := m.opts.Config.ReasoningEffort; got != "none" {
		t.Fatalf("effort after wrapping = %q, want none", got)
	}
}

func TestEffortCommandSetsLiveEffort(t *testing.T) {
	m := newTestModel(t, Options{})
	submitText(t, m, "/effort low")
	if got := m.opts.Config.ReasoningEffort; got != "low" {
		t.Fatalf("config effort = %q, want low", got)
	}
	if got := m.opts.Agent.ReasoningEffort(); got != "low" {
		t.Fatalf("agent effort = %q, want low", got)
	}
	if !strings.Contains(m.View(), "Reasoning effort set to low") {
		t.Fatalf("effort confirmation missing:\n%s", m.View())
	}

	submitText(t, m, "/thinking max")
	if got := m.opts.Agent.ReasoningEffort(); got != "max" {
		t.Fatalf("/thinking effort = %q, want max", got)
	}

	submitText(t, m, "/effort nope")
	if !strings.Contains(m.View(), "expected one of") {
		t.Fatalf("invalid effort not reported:\n%s", m.View())
	}
}

func TestModelModalEffortNumberKeysAndEnter(t *testing.T) {
	m := newTestModel(t, Options{})
	m.opts.Config.ReasoningEffort = "high"
	press(t, m, tea.KeyCtrlP)
	if m.activeModal != modalModel {
		t.Fatal("model selector did not open")
	}
	if !strings.Contains(m.View(), "Reasoning:") {
		t.Fatalf("effort pill row missing:\n%s", m.View())
	}
	pressRune(t, m, "1")
	if m.modal.effort != "none" {
		t.Fatalf("number key effort = %q, want none", m.modal.effort)
	}
	press(t, m, tea.KeyEnter)
	if m.activeModal != modalNone {
		t.Fatal("enter did not close the model selector")
	}
	if got := m.opts.Config.ReasoningEffort; got != "none" {
		t.Fatalf("effort after enter = %q, want none", got)
	}

	// Arrow keys pick the next level and Enter saves it.
	press(t, m, tea.KeyCtrlP)
	press(t, m, tea.KeyRight)
	press(t, m, tea.KeyEnter)
	if got := m.opts.Agent.ReasoningEffort(); got != "low" {
		t.Fatalf("effort after arrow = %q, want low", got)
	}
}

func TestHelpModalHighlightAndDispatch(t *testing.T) {
	m := newTestModel(t, Options{})
	submitText(t, m, "/help")
	if m.activeModal != modalHelp {
		t.Fatal("/help did not open the interactive help modal")
	}
	view := m.View()
	for _, want := range []string{"Commands", "effort", "model", "Shift+Tab"} {
		if !strings.Contains(view, want) {
			t.Fatalf("help modal missing %q:\n%s", want, view)
		}
	}
	if m.modal.index != 0 {
		t.Fatalf("help index = %d, want 0", m.modal.index)
	}
	press(t, m, tea.KeyDown)
	if m.modal.index != 1 {
		t.Fatalf("help index after down = %d, want 1", m.modal.index)
	}
	press(t, m, tea.KeyUp)
	if m.modal.index != 0 {
		t.Fatalf("help index after up = %d, want 0", m.modal.index)
	}

	// Enter on an argument-taking command pre-fills the editor.
	m.modal.index = indexOfHelpItem(t, m, "effort")
	press(t, m, tea.KeyEnter)
	if m.activeModal != modalNone {
		t.Fatal("help did not close after enter")
	}
	if got := m.ed.Value(); got != "/effort " {
		t.Fatalf("pre-filled editor = %q, want /effort ", got)
	}

	// Enter on a zero-arg command runs it immediately.
	m.ed.Reset()
	submitText(t, m, "/help")
	m.modal.index = indexOfHelpItem(t, m, "model")
	press(t, m, tea.KeyEnter)
	if m.activeModal != modalModel {
		t.Fatal("enter on /model did not open the model selector")
	}
}

func indexOfHelpItem(t *testing.T, m *Model, name string) int {
	t.Helper()
	for i, item := range m.modal.helpItems {
		if item.name == name {
			return i
		}
	}
	t.Fatalf("help item %q not found", name)
	return 0
}

func TestStatsSparklineAndGaugeHelpers(t *testing.T) {
	gauge := barGauge(0.5, 20)
	if len([]rune(gauge)) != 20 {
		t.Fatalf("gauge length = %d, want 20", len([]rune(gauge)))
	}
	if !strings.Contains(gauge, "█") || !strings.Contains(gauge, "░") {
		t.Fatalf("gauge missing filled/empty cells: %q", gauge)
	}
	line := sparkline([]float64{0, 1, 2, 3, 4, 5, 6, 7}, 12)
	if !strings.ContainsAny(line, string(sparkRunes)) {
		t.Fatalf("sparkline has no spark glyphs: %q", line)
	}
	if len([]rune(line)) != 12 {
		t.Fatalf("sparkline length = %d, want 12", len([]rune(line)))
	}
}

func TestApprovalQueueIsFIFO(t *testing.T) {
	m := newTestModel(t, Options{})
	first := make(chan approvalResult, 1)
	second := make(chan approvalResult, 1)
	third := make(chan approvalResult, 1)
	m.Update(approvalRequestMsg{request: &approvalState{
		parent:   &agent.ApprovalRequest{ToolName: "bash", Command: "one"},
		response: first,
	}})
	m.Update(approvalRequestMsg{request: &approvalState{
		parent:   &agent.ApprovalRequest{ToolName: "bash", Command: "two"},
		response: second,
	}})
	m.Update(approvalRequestMsg{request: &approvalState{
		parent:   &agent.ApprovalRequest{ToolName: "edit", Command: "three"},
		response: third,
	}})
	if len(m.approvalQueue) != 2 {
		t.Fatalf("approval queue length = %d, want 2", len(m.approvalQueue))
	}
	if !strings.Contains(m.View(), "1 of 3") {
		t.Fatalf("approval header missing queue position:\n%s", m.View())
	}
	if got := m.pendingApproval.parent.Command; got != "one" {
		t.Fatalf("pending approval = %q, want one", got)
	}

	pressRune(t, m, "y")
	select {
	case result := <-first:
		if !result.allow {
			t.Fatal("first approval was denied, want allow")
		}
	default:
		t.Fatal("first approval response missing")
	}
	if m.pendingApproval == nil || m.pendingApproval.parent.Command != "two" {
		t.Fatalf("next approval = %+v, want two", m.pendingApproval)
	}
	if len(m.approvalQueue) != 1 {
		t.Fatalf("approval queue length = %d, want 1", len(m.approvalQueue))
	}

	pressRune(t, m, "n")
	select {
	case result := <-second:
		if result.allow {
			t.Fatal("second approval was allowed, want deny")
		}
	default:
		t.Fatal("second approval response missing")
	}
	if m.pendingApproval == nil || m.pendingApproval.parent.Command != "three" {
		t.Fatalf("third approval not promoted: %+v", m.pendingApproval)
	}
	pressRune(t, m, "y")
	select {
	case result := <-third:
		if !result.allow {
			t.Fatal("third approval was denied, want allow")
		}
	default:
		t.Fatal("third approval response missing")
	}
	if m.pendingApproval != nil || len(m.approvalQueue) != 0 {
		t.Fatal("approval queue did not drain")
	}
}

func TestApprovalAllowAllForTurn(t *testing.T) {
	m := newTestModel(t, Options{})
	m.running = true
	channels := make([]chan approvalResult, 3)
	for i := range channels {
		channels[i] = make(chan approvalResult, 1)
		req := &agent.ApprovalRequest{ToolName: "bash", Command: "cmd"}
		m.Update(approvalRequestMsg{request: &approvalState{parent: req, response: channels[i]}})
	}
	if len(m.approvalQueue) != 2 {
		t.Fatalf("approval queue length = %d, want 2", len(m.approvalQueue))
	}
	pressRune(t, m, "a")
	if !m.turnAutoApprove {
		t.Fatal("pressing a did not enable allow-all for the turn")
	}
	for i, ch := range channels {
		select {
		case result := <-ch:
			if !result.allow {
				t.Fatalf("approval %d was denied, want allow", i)
			}
		default:
			t.Fatalf("approval %d response missing", i)
		}
	}
	if m.pendingApproval != nil || len(m.approvalQueue) != 0 {
		t.Fatal("allow-all did not drain the queue")
	}

	// Subsequent requests in the same turn are approved without prompting.
	next := make(chan approvalResult, 1)
	m.Update(approvalRequestMsg{request: &approvalState{
		parent:   &agent.ApprovalRequest{ToolName: "bash"},
		response: next,
	}})
	select {
	case result := <-next:
		if !result.allow {
			t.Fatal("subsequent approval was denied, want allow")
		}
	case <-time.After(time.Second):
		t.Fatal("subsequent approval was not auto-approved")
	}
	if m.pendingApproval != nil {
		t.Fatal("auto-approved request should not be displayed")
	}

	// The flag is cleared at the end of the turn.
	m.Update(turnDoneMsg{})
	if m.turnAutoApprove {
		t.Fatal("turnAutoApprove was not cleared at turn end")
	}
}

func leadingSpaces(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func lineContaining(view, needle string) string {
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func TestLayoutColumnAlignment(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		m := newTestModel(t, Options{})
		m.width = 140
		m.height = 30
		// Hide the inspector so its model label cannot shadow the status bar
		// in the single-column alignment check.
		m.sidebarHidden = true
		typeText(t, m, "hello")
		view := m.View()
		editorLine := lineContaining(view, "hello")
		statusLine := lineContaining(view, "deepseek")
		if editorLine == "" || statusLine == "" {
			t.Fatalf("missing editor or status line:\n%s", view)
		}
		if leadingSpaces(editorLine) != leadingSpaces(statusLine) {
			t.Fatalf("editor and status left edges differ: editor=%d status=%d\n%s",
				leadingSpaces(editorLine), leadingSpaces(statusLine), view)
		}
		if leadingSpaces(editorLine) == 0 {
			t.Fatalf("wide layout was not centered:\n%s", view)
		}
	})

	t.Run("boxed", func(t *testing.T) {
		m := newTestModel(t, Options{})
		m.st = newStyles(false)
		m.width = 140
		m.height = 30
		m.sidebarHidden = true
		typeText(t, m, "hello")
		view := m.View()
		boxLine := lineContaining(view, "╭")
		statusLine := lineContaining(view, "· ask")
		if boxLine == "" || statusLine == "" {
			t.Fatalf("missing boxed editor or status line:\n%s", view)
		}
		if leadingSpaces(boxLine) != leadingSpaces(statusLine) {
			t.Fatalf("boxed editor and status left edges differ: editor=%d status=%d\n%s",
				leadingSpaces(boxLine), leadingSpaces(statusLine), view)
		}
	})
}

func TestPermissionAndQuestionOverlaysAreCentered(t *testing.T) {
	t.Run("permission", func(t *testing.T) {
		m := newTestModel(t, Options{})
		response := make(chan approvalResult, 1)
		m.Update(approvalRequestMsg{request: &approvalState{
			parent:   &agent.ApprovalRequest{ToolName: "bash", Command: "make test"},
			response: response,
		}})
		view := m.View()
		line := lineContaining(view, "Permission Required")
		if line == "" {
			t.Fatalf("permission overlay missing:\n%s", view)
		}
		if leadingSpaces(line) == 0 {
			t.Fatalf("permission overlay not horizontally centered:\n%s", view)
		}
		lines := strings.Split(view, "\n")
		row := 0
		for i, candidate := range lines {
			if strings.Contains(candidate, "Permission Required") {
				row = i
				break
			}
		}
		if row < m.height/4 || row > m.height*3/4 {
			t.Fatalf("permission overlay not vertically centered: row=%d height=%d", row, m.height)
		}
	})

	t.Run("question", func(t *testing.T) {
		m := newTestModel(t, Options{})
		response := make(chan string, 1)
		m.Update(askUserMsg{request: &askState{
			question: "Which package?",
			options:  []string{"tui", "agent"},
			response: response,
		}})
		view := m.View()
		line := lineContaining(view, "Which package?")
		if line == "" {
			t.Fatalf("question overlay missing:\n%s", view)
		}
		if leadingSpaces(line) == 0 {
			t.Fatalf("question overlay not centered:\n%s", view)
		}
	})
}

func TestYoloToggleShortcutAndCommand(t *testing.T) {
	m := newTestModel(t, Options{})
	if got := m.opts.Agent.Permissions.Mode(); got != permission.ModeAsk {
		t.Fatalf("initial mode = %q, want ask", got)
	}
	press(t, m, tea.KeyCtrlY)
	if got := m.opts.Agent.Permissions.Mode(); got != permission.ModeYolo {
		t.Fatalf("mode after ctrl+y = %q, want yolo", got)
	}
	if got := m.opts.Config.PermissionMode; got != permission.ModeYolo.String() {
		t.Fatalf("config mode after ctrl+y = %q", got)
	}
	if view := m.View(); !strings.Contains(view, "YOLO") {
		t.Fatalf("status bar missing YOLO indicator:\n%s", view)
	}
	press(t, m, tea.KeyCtrlY)
	if got := m.opts.Agent.Permissions.Mode(); got != permission.ModeAsk {
		t.Fatalf("mode after second ctrl+y = %q, want ask", got)
	}

	submitText(t, m, "/yolo")
	if got := m.opts.Agent.Permissions.Mode(); got != permission.ModeYolo {
		t.Fatalf("mode after /yolo = %q, want yolo", got)
	}
	submitText(t, m, "/yolo")
	if got := m.opts.Agent.Permissions.Mode(); got != permission.ModeAsk {
		t.Fatalf("mode after second /yolo = %q, want ask", got)
	}
}

func TestThinkingThreeStateCycle(t *testing.T) {
	m := newTestModel(t, Options{})
	m.st = newStyles(false)
	m.Update(agentEventMsg{event: agent.Event{Type: agent.EventReasoningDelta, Text: "secret reasoning"}})
	m.Update(tickMsg(time.Now()))
	if m.thinking != thinkingCollapsed {
		t.Fatalf("initial thinking mode = %v, want collapsed", m.thinking)
	}
	view := m.View()
	if !strings.Contains(view, "Thought") || !strings.Contains(view, "^T expand") {
		t.Fatalf("collapsed summary missing:\n%s", view)
	}
	if strings.Contains(view, "secret reasoning") {
		t.Fatalf("reasoning leaked while collapsed:\n%s", view)
	}

	press(t, m, tea.KeyCtrlT)
	if m.thinking != thinkingExpanded {
		t.Fatalf("thinking mode = %v, want expanded", m.thinking)
	}
	view = m.View()
	if !strings.Contains(view, "secret reasoning") || !strings.Contains(view, "│") {
		t.Fatalf("expanded reasoning missing:\n%s", view)
	}

	press(t, m, tea.KeyCtrlT)
	if m.thinking != thinkingHidden {
		t.Fatalf("thinking mode = %v, want hidden", m.thinking)
	}
	view = m.View()
	if strings.Contains(view, "secret reasoning") || strings.Contains(view, "Thought") {
		t.Fatalf("reasoning still visible while hidden:\n%s", view)
	}

	press(t, m, tea.KeyCtrlT)
	if m.thinking != thinkingCollapsed {
		t.Fatalf("thinking mode = %v, want collapsed again", m.thinking)
	}
}

func TestScrollingDoesNotSnapBackToBottom(t *testing.T) {
	m := newTestModel(t, Options{})
	m.height = 24
	for i := 0; i < 60; i++ {
		m.addInfo("history line " + strings.Repeat("x", 40))
	}
	m.View()
	if !m.followTail {
		t.Fatal("initial view should follow the tail")
	}
	press(t, m, tea.KeyPgUp)
	if m.followTail {
		t.Fatal("PgUp did not disable followTail")
	}
	afterPgUp := m.viewport.YOffset
	if afterPgUp <= 0 {
		t.Fatalf("PgUp did not scroll up: offset=%d", afterPgUp)
	}

	m.Update(tickMsg(time.Now()))
	m.Update(agentEventMsg{event: agent.Event{Type: agent.EventTextDelta, Text: "streaming while scrolled up"}})
	m.View()
	if m.followTail {
		t.Fatal("tick/streaming re-enabled followTail while scrolled up")
	}
	if m.viewport.YOffset != afterPgUp {
		t.Fatalf("scroll offset jumped after tick/stream: got %d want %d", m.viewport.YOffset, afterPgUp)
	}
	if m.viewport.AtBottom() {
		t.Fatal("viewport snapped back to the bottom")
	}
	if !strings.Contains(m.View(), "scrolled up") {
		t.Fatalf("scrolled-up indicator missing:\n%s", m.View())
	}
}

func TestMouseWheelScrolling(t *testing.T) {
	m := newTestModel(t, Options{})
	m.height = 24
	for i := 0; i < 60; i++ {
		m.addInfo("history line " + strings.Repeat("x", 40))
	}
	m.View()
	if !m.followTail {
		t.Fatal("initial view should follow the tail")
	}
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.followTail {
		t.Fatal("wheel up did not disable followTail")
	}
	if m.viewport.YOffset <= 0 {
		t.Fatalf("wheel up did not scroll: offset=%d", m.viewport.YOffset)
	}
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if !m.followTail {
		t.Fatal("wheel down to the bottom did not re-enable followTail")
	}
}

func TestTextOnlyTokensPerSecondTracking(t *testing.T) {
	m := newTestModel(t, Options{})
	m.running = true
	m.turnStart = time.Now().Add(-time.Second)
	text := strings.Repeat("word ", 100)

	m.Update(agentEventMsg{event: agent.Event{Type: agent.EventTextDelta, Text: text}})
	textSpeed := m.liveSpeed
	if textSpeed <= 0 {
		t.Fatalf("text delta did not produce a live speed: %v", textSpeed)
	}
	runesAfterText := m.outputRunes

	m.Update(agentEventMsg{event: agent.Event{Type: agent.EventReasoningDelta, Text: strings.Repeat("reasoning ", 500)}})
	if m.liveSpeed != textSpeed {
		t.Fatalf("reasoning delta changed live speed: got %v want %v", m.liveSpeed, textSpeed)
	}
	if m.outputRunes != runesAfterText {
		t.Fatalf("reasoning delta counted output runes: got %d want %d", m.outputRunes, runesAfterText)
	}

	m.Update(agentEventMsg{event: agent.Event{Type: agent.EventToolStart, ToolName: "bash", ToolCallID: "c1"}})
	if m.liveSpeed != textSpeed {
		t.Fatalf("tool start changed live speed: got %v want %v", m.liveSpeed, textSpeed)
	}
	if m.outputRunes != 0 {
		t.Fatalf("tool start did not freeze the text window: runes=%d", m.outputRunes)
	}

	m.Update(agentEventMsg{event: agent.Event{Type: agent.EventUsage, TokensPerSec: 77}})
	if m.liveSpeed != 77 {
		t.Fatalf("usage tok/s = %v, want 77", m.liveSpeed)
	}
	m.Update(agentEventMsg{event: agent.Event{Type: agent.EventTurnEnd, TokensPerSec: 0}})
	if m.liveSpeed != 77 {
		t.Fatalf("zero tok/s usage erased text speed: got %v want 77", m.liveSpeed)
	}
}

func TestTwoPaneSidebarRenderingAndToggle(t *testing.T) {
	m := newTestModel(t, Options{GitBranch: "main"})
	m.width = 140
	m.height = 40

	view := m.View()
	for _, want := range []string{"CONTEXT", "STATS", "SUB-AGENTS", "Ctx", "Cache", "main"} {
		if !strings.Contains(view, want) {
			t.Fatalf("wide two-pane view missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "|") {
		t.Fatalf("wide view missing the pane divider:\n%s", view)
	}
	if m.sidebarHidden {
		t.Fatal("sidebar should be visible by default")
	}

	press(t, m, tea.KeyCtrlB)
	if !m.sidebarHidden {
		t.Fatal("Ctrl+B did not hide the sidebar")
	}
	view = m.View()
	if strings.Contains(view, "CONTEXT & STATS") || strings.Contains(view, "SUB-AGENTS · idle") {
		t.Fatalf("hidden sidebar still rendered:\n%s", view)
	}

	press(t, m, tea.KeyCtrlB)
	if m.sidebarHidden {
		t.Fatal("Ctrl+B did not restore the sidebar")
	}
	if !strings.Contains(m.View(), "CONTEXT & STATS") {
		t.Fatalf("restored sidebar missing:\n%s", m.View())
	}
}

func TestNarrowTerminalCollapsesToSinglePane(t *testing.T) {
	m := newTestModel(t, Options{GitBranch: "main"})
	m.width = 80
	m.height = 24
	typeText(t, m, "hello narrow")

	view := m.View()
	if strings.Contains(view, "CONTEXT & STATS") {
		t.Fatalf("narrow layout rendered the sidebar:\n%s", view)
	}
	if leadingSpaces(lineContaining(view, "hello narrow")) != 0 {
		t.Fatalf("narrow layout should not center a wide column:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if displayWidth(line) > m.width {
			t.Fatalf("narrow view line overflows %d columns: %q", m.width, line)
		}
	}
}

func TestSubagentsLiveInSidebarAndFocusCycles(t *testing.T) {
	m := newTestModel(t, Options{})
	m.width = 140
	m.height = 40
	for _, event := range []subagent.Event{
		{Type: subagent.EventSubagentStart, AgentID: "agent-1", AgentType: subagent.AgentExplore, Description: "find the parser", Status: subagent.StatusRunning},
		{Type: subagent.EventSubagentStart, AgentID: "agent-2", AgentType: subagent.AgentReview, Description: "review the patch", Status: subagent.StatusRunning},
		{Type: subagent.EventSubagentDelta, AgentID: "agent-1", AgentType: subagent.AgentExplore, Description: "find the parser", Status: subagent.StatusRunning, Detail: "reading parser.go"},
	} {
		m.Update(subagentEventMsg{event: event})
	}
	m.Update(tickMsg(time.Now()))

	view := m.View()
	if !strings.Contains(view, "SUB-AGENTS") || !strings.Contains(view, "find the parser") {
		t.Fatalf("sidebar missing live sub-agents:\n%s", view)
	}
	agentLine := lineContaining(view, "find the parser")
	// The welcome emblem means the left pane can begin with non-space art on
	// the same row, so compare the text's column to the pane edge directly.
	if strings.Index(agentLine, "find the parser") <= m.leftPaneWidth() {
		t.Fatalf("sub-agent rendered in the chat pane instead of the sidebar:\n%s", view)
	}

	press(t, m, tea.KeyTab)
	if !m.agentsOpen {
		t.Fatal("Tab did not focus the sidebar sub-agent list")
	}
	view = m.View()
	if !strings.Contains(view, "reading parser.go") {
		t.Fatalf("focused sidebar agent missing live output:\n%s", view)
	}
	if !strings.Contains(view, "c cancel") || !strings.Contains(view, "Esc chat") {
		t.Fatalf("focused sidebar missing cancel/unfocus hints:\n%s", view)
	}

	first := m.agentFocus
	press(t, m, tea.KeyDown)
	if m.agentFocus == first {
		t.Fatal("down did not cycle the focused sub-agent")
	}
	press(t, m, tea.KeyEsc)
	if m.agentsOpen {
		t.Fatal("Esc did not return focus to the chat")
	}
}

func TestTodosRenderInSidebar(t *testing.T) {
	store := tools.NewTodoStore()
	if _, err := store.Replace([]tools.TodoItem{
		{ID: "1", Content: "read the files", Status: tools.TodoCompleted},
		{ID: "2", Content: "write the code", Status: tools.TodoInProgress},
		{ID: "3", Content: "ship it", Status: tools.TodoPending},
	}); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{Todos: store})
	m.width = 140
	m.height = 40

	view := m.View()
	for _, want := range []string{"TODOS (1/3)", "read the files", "write the code", "ship it"} {
		if !strings.Contains(view, want) {
			t.Fatalf("sidebar todos missing %q:\n%s", want, view)
		}
	}
}

func newSteerableModel(t *testing.T) (*Model, *agent.Agent) {
	t.Helper()
	ag := agent.New(agent.Options{Config: config.Default()})
	m := newTestModel(t, Options{Agent: ag})
	m.baseCtx = context.Background()
	return m, ag
}

func TestMessageQueueingAndAutoDispatch(t *testing.T) {
	m, _ := newSteerableModel(t)
	m.running = true

	submitText(t, m, "first queued prompt")
	if m.ed.Value() != "" {
		t.Fatalf("editor not cleared after queueing: %q", m.ed.Value())
	}
	submitText(t, m, "second queued prompt")
	if len(m.queued) != 2 {
		t.Fatalf("queue length = %d, want 2", len(m.queued))
	}
	if m.queued[0].text != "first queued prompt" || m.queued[1].text != "second queued prompt" {
		t.Fatalf("queue is not FIFO: %+v", m.queued)
	}
	if strings.Contains(m.View(), "A turn is already running") {
		t.Fatalf("queueing still rejected with the old warning:\n%s", m.View())
	}

	view := m.View()
	for _, want := range []string{"Queued (2)", "first queued prompt", "^S steer", "/queue clear"} {
		if !strings.Contains(view, want) {
			t.Fatalf("queue strip missing %q:\n%s", want, view)
		}
	}

	// A completed turn dispatches the first queued message automatically.
	m.Update(turnDoneMsg{result: &agent.TurnResult{StopReason: agent.StopComplete}})
	if len(m.queued) != 1 {
		t.Fatalf("queue length after dispatch = %d, want 1", len(m.queued))
	}
	if m.queued[0].text != "second queued prompt" {
		t.Fatalf("wrong message left in queue: %+v", m.queued)
	}
	if !m.running {
		t.Fatal("auto-dispatched turn did not start")
	}
	if !strings.Contains(m.View(), "Queued (1)") {
		t.Fatalf("queue strip not updated after dispatch:\n%s", m.View())
	}
}

func TestCtrlSSteersAndPromotesQueuedMessage(t *testing.T) {
	m, ag := newSteerableModel(t)
	m.running = true

	// Direct steer from editor text.
	typeText(t, m, "focus on tests")
	press(t, m, tea.KeyCtrlS)
	if got := ag.PendingSteers(); got != 1 {
		t.Fatalf("pending steers = %d, want 1", got)
	}
	if m.ed.Value() != "" {
		t.Fatalf("editor not cleared after steering: %q", m.ed.Value())
	}
	if !strings.Contains(m.View(), "Steered: focus on tests") {
		t.Fatalf("steering not shown in transcript:\n%s", m.View())
	}

	// Empty editor promotes the first queued message instead.
	m.queueMessage("queued steer", nil)
	press(t, m, tea.KeyCtrlS)
	if len(m.queued) != 0 {
		t.Fatalf("queue not drained by promotion: %+v", m.queued)
	}
	if got := ag.PendingSteers(); got != 2 {
		t.Fatalf("pending steers after promotion = %d, want 2", got)
	}
	if !strings.Contains(m.View(), "Steered: queued steer") {
		t.Fatalf("promoted steer not shown:\n%s", m.View())
	}

	// The agent's matching steer event must not duplicate the local entry.
	m.Update(agentEventMsg{event: agent.Event{Type: agent.EventSteer, Text: "queued steer"}})
	if n := strings.Count(m.View(), "Steered: queued steer"); n != 1 {
		t.Fatalf("steer entry rendered %d times, want 1:\n%s", n, m.View())
	}
}

func TestQueueClearAndCancelPreservesQueue(t *testing.T) {
	m, _ := newSteerableModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.running = true
	m.turnCtx = ctx
	m.cancel = cancel

	m.queueMessage("one", nil)
	m.queueMessage("two", nil)
	submitText(t, m, "/queue clear")
	if len(m.queued) != 0 {
		t.Fatalf("/queue clear left %d item(s)", len(m.queued))
	}
	if !strings.Contains(m.View(), "Cleared 2 queued") {
		t.Fatalf("/queue clear confirmation missing:\n%s", m.View())
	}

	// Ctrl+C must cancel without auto-starting the surviving queue.
	m.queueMessage("keep me", nil)
	press(t, m, tea.KeyCtrlC)
	if m.running {
		t.Fatal("turn still running after Ctrl+C")
	}
	if len(m.queued) != 1 || m.queued[0].text != "keep me" {
		t.Fatalf("cancel did not preserve the queue: %+v", m.queued)
	}
	cancelledView := m.View()
	if !strings.Contains(cancelledView, "1 queued") || !strings.Contains(cancelledView, "/queue run") {
		t.Fatalf("cancel message missing queue hint:\n%s", cancelledView)
	}

	// A late cancellation result must not dispatch the held item either.
	m.Update(turnDoneMsg{err: context.Canceled})
	if len(m.queued) != 1 {
		t.Fatalf("cancelled turn auto-dispatched the queue: %+v", m.queued)
	}
	if m.running {
		t.Fatal("cancelled turn restarted from the queue")
	}
}

func TestWelcomeTernEmblemAndNoColor(t *testing.T) {
	m := newTestModel(t, Options{})
	view := m.View()
	for _, line := range ternEmblem {
		if !strings.Contains(view, strings.TrimSpace(line)) {
			t.Fatalf("welcome view missing emblem line %q:\n%s", line, view)
		}
	}
	if strings.Contains(view, "\x1b[") {
		t.Fatalf("welcome view contains ANSI under NO_COLOR:\n%q", view)
	}

	// Tiny terminals omit the emblem rather than clipping it.
	m.width = 30
	m.height = 15
	m.dirty = true
	small := m.View()
	if strings.Contains(small, "_..---'") {
		t.Fatalf("emblem rendered on a tiny terminal:\n%s", small)
	}

	// NO_COLOR still strips styles even when color was explicitly requested.
	t.Setenv("NO_COLOR", "1")
	plain := newTestModel(t, Options{})
	plain.st = newStyles(noColor(false))
	styled := plain.View()
	if strings.Contains(styled, "\x1b[") {
		t.Fatalf("NO_COLOR view contains ANSI sequences:\n%q", styled)
	}
	for _, line := range ternEmblem {
		if !strings.Contains(styled, strings.TrimSpace(line)) {
			t.Fatalf("NO_COLOR welcome missing emblem line %q:\n%s", line, styled)
		}
	}
}

func TestAgentPeekModalFromSidebarAndAgentsPanel(t *testing.T) {
	store, dir := newSessionStore(t)
	parent, err := store.Create(session.SessionMeta{Name: "parent", Model: "deepseek-flash", WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	child, err := parent.CreateChild("call-1", "agent-1", subagent.AgentExplore, "find the parser", "", "deepseek-flash")
	if err != nil {
		t.Fatal(err)
	}
	if err := child.AppendMessage(provider.Message{Role: provider.RoleUser, Content: "inspect the parser and report"}); err != nil {
		t.Fatal(err)
	}
	if err := child.AppendMessage(provider.Message{
		Role:             provider.RoleAssistant,
		ReasoningContent: "weighing the parser options",
		Content:          "looked at parser.go",
		ToolCalls: []provider.ToolCall{{
			ID: "call-1", Type: "function",
			Function: provider.ToolCallFunction{Name: "read_file", Arguments: `{"path":"parser.go"}`},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := child.AppendMessage(provider.Message{Role: provider.RoleTool, ToolCallID: "call-1", Name: "read_file", Content: "package parser\nfunc Parse() {}"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := child.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: "filler transcript line"}); err != nil {
			t.Fatal(err)
		}
	}
	second, err := parent.CreateChild("call-2", "agent-2", subagent.AgentReview, "review the patch", "", "deepseek-flash")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.AppendMessage(provider.Message{Role: provider.RoleUser, Content: "second prompt body"}); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t, Options{SessionStore: store, Session: parent, WorkDir: dir})
	for _, event := range []subagent.Event{
		{Type: subagent.EventSubagentStart, AgentID: "agent-1", AgentType: subagent.AgentExplore, Description: "find the parser", Status: subagent.StatusRunning},
		{Type: subagent.EventSubagentStart, AgentID: "agent-2", AgentType: subagent.AgentReview, Description: "review the patch", Status: subagent.StatusRunning},
	} {
		m.Update(subagentEventMsg{event: event})
	}
	m.agentLines["agent-1"] = &strings.Builder{}
	m.agentLines["agent-1"].WriteString("live streaming chunk")

	press(t, m, tea.KeyTab)
	if !m.agentsOpen {
		t.Fatal("Tab did not focus sub-agents")
	}
	press(t, m, tea.KeyEnter)
	if m.activeModal != modalAgentPeek {
		t.Fatalf("Enter on focused sub-agent opened %v, want modalAgentPeek", m.activeModal)
	}
	if m.modal.peekAgentID != "agent-1" {
		t.Fatalf("peeked agent = %q, want agent-1", m.modal.peekAgentID)
	}
	view := m.View()
	for _, want := range []string{"Prompt", "inspect the parser and report", "looked at parser.go", "read_file", "Thought"} {
		if !strings.Contains(view, want) {
			t.Fatalf("peek view missing %q:\n%s", want, view)
		}
	}

	before := m.modal.peekScroll
	press(t, m, tea.KeyDown)
	if m.modal.peekScroll <= before {
		t.Fatalf("down did not scroll peek (before=%d after=%d)", before, m.modal.peekScroll)
	}
	press(t, m, tea.KeyPgDown)
	if m.modal.peekScroll <= before {
		t.Fatal("pgdown did not advance the peek")
	}
	press(t, m, tea.KeyEnd)
	if !strings.Contains(m.View(), "live streaming chunk") {
		t.Fatalf("peek view missing live streaming output:\n%s", m.View())
	}

	press(t, m, tea.KeyTab)
	if m.modal.peekAgentID != "agent-2" {
		t.Fatalf("tab switch peeked %q, want agent-2", m.modal.peekAgentID)
	}
	if !strings.Contains(m.View(), "second prompt body") {
		t.Fatalf("switched peek missing second transcript:\n%s", m.View())
	}
	press(t, m, tea.KeyRight)
	if m.modal.peekAgentID != "agent-1" {
		t.Fatalf("right switch wrapped peeked %q, want agent-1", m.modal.peekAgentID)
	}

	press(t, m, tea.KeyEsc)
	if m.activeModal != modalNone {
		t.Fatal("Esc did not close the peek modal")
	}

	// The /agents panel also opens the peek, and /agents <index> jumps directly.
	submitText(t, m, "/agents")
	if !m.agentsPanel {
		t.Fatal("/agents did not open the panel")
	}
	press(t, m, tea.KeyEnter)
	if m.activeModal != modalAgentPeek || m.modal.peekAgentID != "agent-1" {
		t.Fatalf("/agents Enter did not peek agent-1 (modal=%v id=%q)", m.activeModal, m.modal.peekAgentID)
	}
	press(t, m, tea.KeyEsc)

	submitText(t, m, "/agents 2")
	if m.activeModal != modalAgentPeek || m.modal.peekAgentID != "agent-2" {
		t.Fatalf("/agents 2 did not peek agent-2 (modal=%v id=%q)", m.activeModal, m.modal.peekAgentID)
	}
}

func TestPersistentChatbarHistory(t *testing.T) {
	dir := t.TempDir()
	first := newTestModel(t, Options{GlobalConfigDir: dir})
	submitText(t, first, "/cost")
	first.running = true // keep the prompt queued instead of starting a turn
	submitText(t, first, "first line\nsecond line")

	historyFile := filepath.Join(dir, "history.jsonl")
	data, err := os.ReadFile(historyFile)
	if err != nil {
		t.Fatalf("history file was not written: %v", err)
	}
	if got := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; got != 2 {
		t.Fatalf("history file lines = %d, want 2:\n%s", got, data)
	}

	second := newTestModel(t, Options{GlobalConfigDir: dir})
	if got := second.ed.History(); len(got) != 2 {
		t.Fatalf("loaded history = %#v, want 2 entries", got)
	}
	press(t, second, tea.KeyUp)
	if got := second.ed.Value(); got != "first line\nsecond line" {
		t.Fatalf("first up = %q, want the multi-line prompt", got)
	}
	press(t, second, tea.KeyUp)
	if got := second.ed.Value(); got != "/cost" {
		t.Fatalf("second up = %q, want /cost (zsh-style multi-line stepping)", got)
	}
	press(t, second, tea.KeyDown)
	if got := second.ed.Value(); got != "first line\nsecond line" {
		t.Fatalf("down = %q, want the multi-line prompt", got)
	}
	press(t, second, tea.KeyDown)
	if got := second.ed.Value(); got != "" {
		t.Fatalf("final down = %q, want the empty draft", got)
	}
}

func clickAt(m *Model, x, y int, button tea.MouseButton) tea.Cmd {
	_, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: button, X: x, Y: y})
	return cmd
}

func statusHitFor(m *Model, action mouseAction) (statusHit, bool) {
	for _, hit := range m.statusHits {
		if hit.action == action {
			return hit, true
		}
	}
	return statusHit{}, false
}

func TestMouseClickSubagentSidebarOpensPeek(t *testing.T) {
	m := newTestModel(t, Options{})
	m.width = 140
	m.height = 40
	for _, event := range []subagent.Event{
		{Type: subagent.EventSubagentStart, AgentID: "agent-1", AgentType: subagent.AgentExplore, Description: "find the parser", Status: subagent.StatusRunning},
		{Type: subagent.EventSubagentStart, AgentID: "agent-2", AgentType: subagent.AgentReview, Description: "review the patch", Status: subagent.StatusRunning},
	} {
		m.Update(subagentEventMsg{event: event})
	}
	m.Update(tickMsg(time.Now()))
	m.View()

	row := -1
	for y, id := range m.sidebarAgentRows {
		if id == "agent-2" {
			row = y
			break
		}
	}
	if row < 0 {
		t.Fatalf("no sidebar hit row for agent-2: %+v", m.sidebarAgentRows)
	}
	clickAt(m, m.sidebarX+3, row, tea.MouseButtonLeft)
	if m.activeModal != modalAgentPeek {
		t.Fatalf("left click opened %v, want modalAgentPeek", m.activeModal)
	}
	if m.modal.peekAgentID != "agent-2" {
		t.Fatalf("peeked agent = %q, want agent-2", m.modal.peekAgentID)
	}
	if !strings.Contains(m.View(), "review the patch") {
		t.Fatalf("peek view missing clicked agent:\n%s", m.View())
	}
}

func TestRightClickMessageCopiesToClipboard(t *testing.T) {
	m := newTestModel(t, Options{})
	var copied string
	m.clipboardFn = func(text string) error {
		copied = text
		return nil
	}
	user := &transcriptEntry{kind: entryUser, text: "copy this user prompt"}
	assistant := &transcriptEntry{kind: entryAssistant, text: "copy this assistant reply"}
	m.entries = append(m.entries, user, assistant)
	m.dirty = true
	m.View()

	var span entrySpan
	found := false
	for _, candidate := range m.entrySpans {
		if candidate.entry == user {
			span = candidate
			found = true
			break
		}
	}
	if !found {
		t.Fatal("user entry span was not recorded")
	}
	y := span.start - m.viewport.YOffset
	clickAt(m, m.viewportX+2, y, tea.MouseButtonRight)
	if copied != user.text {
		t.Fatalf("clipboard got %q, want %q", copied, user.text)
	}
	if !strings.HasPrefix(m.statusMsg, "Copied") || !strings.Contains(m.statusMsg, "chars") {
		t.Fatalf("statusMsg = %q, want a Copied confirmation", m.statusMsg)
	}

	// Assistant messages copy too.
	copied = ""
	m.View()
	y = -1
	for _, candidate := range m.entrySpans {
		if candidate.entry == assistant {
			y = candidate.start - m.viewport.YOffset
			break
		}
	}
	if y < 0 {
		t.Fatal("assistant entry span was not recorded")
	}
	clickAt(m, m.viewportX+2, y, tea.MouseButtonRight)
	if copied != assistant.text {
		t.Fatalf("clipboard got %q, want %q", copied, assistant.text)
	}
}

func TestLeftClickThinkingEntryToggles(t *testing.T) {
	m := newTestModel(t, Options{})
	thinking := &transcriptEntry{kind: entryReasoning, text: "weighing the options carefully"}
	m.entries = append(m.entries, thinking)
	m.thinking = thinkingCollapsed
	m.dirty = true
	m.View()

	row := -1
	for _, span := range m.entrySpans {
		if span.entry == thinking {
			row = span.start - m.viewport.YOffset
			break
		}
	}
	if row < 0 {
		t.Fatal("reasoning entry span was not recorded")
	}
	clickAt(m, m.viewportX+1, row, tea.MouseButtonLeft)
	if m.thinking != thinkingExpanded {
		t.Fatalf("click did not expand thinking: mode=%v", m.thinking)
	}
	m.View()
	row = -1
	for _, span := range m.entrySpans {
		if span.entry == thinking {
			row = span.start - m.viewport.YOffset
			break
		}
	}
	if row < 0 {
		t.Fatal("expanded reasoning entry span was not recorded")
	}
	clickAt(m, m.viewportX+1, row, tea.MouseButtonLeft)
	if m.thinking != thinkingCollapsed {
		t.Fatalf("second click did not collapse thinking: mode=%v", m.thinking)
	}
}

func TestLeftClickStatusSegments(t *testing.T) {
	m := newTestModel(t, Options{GitBranch: "main"})
	m.width = 120
	m.height = 32

	t.Run("model", func(t *testing.T) {
		m.View()
		hit, ok := statusHitFor(m, actionStatusModel)
		if !ok {
			t.Fatal("model status hit was not recorded")
		}
		clickAt(m, m.statusX+hit.start, m.statusY, tea.MouseButtonLeft)
		if m.activeModal != modalModel {
			t.Fatalf("model click opened %v, want modalModel", m.activeModal)
		}
		m.closeModal()
	})

	t.Run("yolo", func(t *testing.T) {
		m.View()
		hit, ok := statusHitFor(m, actionStatusYolo)
		if !ok {
			t.Fatal("yolo status hit was not recorded")
		}
		clickAt(m, m.statusX+hit.start, m.statusY, tea.MouseButtonLeft)
		if m.opts.Agent.Permissions.Mode() != permission.ModeYolo {
			t.Fatalf("yolo click left mode = %v", m.opts.Agent.Permissions.Mode())
		}
	})

	t.Run("panel", func(t *testing.T) {
		m.sidebarHidden = false
		m.View()
		hit, ok := statusHitFor(m, actionStatusPanel)
		if !ok {
			t.Fatal("panel status hit was not recorded")
		}
		clickAt(m, m.statusX+hit.start, m.statusY, tea.MouseButtonLeft)
		if !m.sidebarHidden {
			t.Fatal("panel click did not hide the sidebar")
		}
	})
}

func TestLeftClickApprovalButtonsAndHelpRow(t *testing.T) {
	t.Run("approval allow and deny", func(t *testing.T) {
		for _, action := range []mouseAction{actionApprovalAllow, actionApprovalDeny} {
			m := newTestModel(t, Options{})
			response := make(chan approvalResult, 1)
			m.Update(approvalRequestMsg{request: &approvalState{
				parent:   &agent.ApprovalRequest{ToolName: "bash", Command: "rm -rf build"},
				response: response,
			}})
			m.View()
			found := false
			for _, hit := range m.overlayHits {
				if hit.action != action {
					continue
				}
				found = true
				clickAt(m, m.overlayLeft+hit.start, m.overlayTop+hit.line, tea.MouseButtonLeft)
				break
			}
			if !found {
				t.Fatalf("approval hit %v was not recorded: %+v", action, m.overlayHits)
			}
			select {
			case result := <-response:
				wantAllow := action == actionApprovalAllow
				if result.allow != wantAllow {
					t.Fatalf("approval result allow=%v, want %v", result.allow, wantAllow)
				}
			case <-time.After(time.Second):
				t.Fatal("approval click did not resolve the request")
			}
			if m.pendingApproval != nil {
				t.Fatal("approval prompt was not cleared")
			}
		}
	})

	t.Run("help row", func(t *testing.T) {
		m := newTestModel(t, Options{})
		submitText(t, m, "/help")
		m.View()
		if len(m.modalAbsRows) == 0 {
			t.Fatal("help modal did not record clickable rows")
		}
		target := 1
		y := -1
		for row, index := range m.modalAbsRows {
			if index == target {
				y = row
				break
			}
		}
		if y < 0 {
			t.Fatalf("help row %d missing from hit map: %+v", target, m.modalAbsRows)
		}
		clickAt(m, m.width/2, y, tea.MouseButtonLeft)
		if m.modal.index != target {
			t.Fatalf("help click selected row %d, want %d", m.modal.index, target)
		}
	})
}

func longApprovalDiff() string {
	var b strings.Builder
	b.WriteString("--- a/internal/parser.go\n")
	b.WriteString("+++ b/internal/parser.go\n")
	b.WriteString("@@ -1,3 +1,20 @@\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "+added line %02d\n", i)
	}
	for i := 0; i < 4; i++ {
		fmt.Fprintf(&b, "-removed line %02d\n", i)
	}
	return b.String()
}

func TestApprovalDiffStatsAndExpansion(t *testing.T) {
	m := newTestModel(t, Options{})
	response := make(chan approvalResult, 1)
	m.Update(approvalRequestMsg{request: &approvalState{
		parent:   &agent.ApprovalRequest{ToolName: "edit", Path: "parser.go", Diff: longApprovalDiff()},
		response: response,
	}})

	view := m.View()
	for _, want := range []string{"+20", "-4", "[d] Full diff", "more diff lines"} {
		if !strings.Contains(view, want) {
			t.Fatalf("compact approval view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "added line 19") {
		t.Fatalf("compact preview should hide far diff lines:\n%s", view)
	}

	pressRune(t, m, "d")
	if m.pendingApproval == nil || !m.pendingApproval.diffExpanded {
		t.Fatal("'d' did not expand the diff")
	}
	view = m.View()
	for _, want := range []string{"added line 19", "-removed line 03", "[d] Compact diff"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expanded approval view missing %q:\n%s", want, view)
		}
	}

	pressRune(t, m, "d")
	if m.pendingApproval.diffExpanded {
		t.Fatal("second 'd' did not collapse the diff")
	}

	// Clicking the [d] button must toggle exactly like the keyboard shortcut.
	m.View()
	found := false
	for _, hit := range m.overlayHits {
		if hit.action != actionApprovalDiffToggle {
			continue
		}
		found = true
		clickAt(m, m.overlayLeft+hit.start, m.overlayTop+hit.line, tea.MouseButtonLeft)
		break
	}
	if !found {
		t.Fatalf("diff toggle hit was not recorded: %+v", m.overlayHits)
	}
	if !m.pendingApproval.diffExpanded {
		t.Fatal("clicking [d] Full diff did not expand the diff")
	}
	if m.pendingApproval == nil {
		t.Fatal("diff toggle unexpectedly resolved the approval")
	}
}

func TestApprovalDiffStatsAbsentWithoutDiff(t *testing.T) {
	m := newTestModel(t, Options{})
	m.Update(approvalRequestMsg{request: &approvalState{
		parent:   &agent.ApprovalRequest{ToolName: "bash", Command: "go test ./..."},
		response: make(chan approvalResult, 1),
	}})
	view := m.View()
	if strings.Contains(view, "[d]") {
		t.Fatalf("approval without diff should not show a diff toggle:\n%s", view)
	}
	if strings.Contains(view, "+0") || strings.Contains(view, "-0") {
		t.Fatalf("approval without diff should not show stats:\n%s", view)
	}
}

func TestThemeCommandSwitchesPalettes(t *testing.T) {
	m := newTestModel(t, Options{})
	if m.theme != "dark" {
		t.Fatalf("default theme = %q, want dark", m.theme)
	}

	submitText(t, m, "/theme light")
	if m.theme != "light" {
		t.Fatalf("theme after /theme light = %q", m.theme)
	}
	if !m.st.plain {
		t.Fatal("NO_COLOR model should keep plain styles after switching theme")
	}
	if view := m.View(); strings.Contains(view, "\x1b[") {
		t.Fatalf("light theme emitted ANSI under NO_COLOR:\n%q", view)
	}

	submitText(t, m, "/theme dark")
	if m.theme != "dark" {
		t.Fatalf("theme after /theme dark = %q", m.theme)
	}

	// No argument toggles in both directions.
	submitText(t, m, "/theme")
	if m.theme != "light" {
		t.Fatalf("theme after bare /theme = %q, want light", m.theme)
	}
	submitText(t, m, "/theme")
	if m.theme != "dark" {
		t.Fatalf("theme after second bare /theme = %q, want dark", m.theme)
	}

	// Invalid arguments leave the active palette untouched.
	submitText(t, m, "/theme neon")
	if m.theme != "dark" {
		t.Fatalf("invalid /theme changed the theme to %q", m.theme)
	}
}

func TestThemePalettesAreSurfaceFree(t *testing.T) {
	light := paletteForTheme("light")
	if light.accent != "#16150F" || light.muted != "#4C4940" || light.subtle != "#767063" ||
		light.border != "#B8B2A6" || light.sage != "#2D6A4F" || light.amber != "#9A6700" || light.coral != "#B42318" {
		t.Fatalf("unexpected light palette: %+v", light)
	}
	for _, theme := range []string{"dark", "light"} {
		st := newStylesForTheme(false, theme)
		for name, rendered := range map[string]string{
			"surface":     st.paint(st.surface, "x"),
			"panel":       st.paint(st.panel, "x"),
			"modalBorder": st.paint(st.modalBorder, "x"),
			"selected":    st.paint(st.selected, "x"),
		} {
			if strings.Contains(rendered, "48;") || strings.Contains(rendered, "48:") {
				t.Fatalf("%s theme %s painted a background: %q", theme, name, rendered)
			}
		}
	}
}

func TestThemeHonorsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, theme := range []string{"dark", "light"} {
		st := newStylesForTheme(noColor(false), theme)
		if !st.plain {
			t.Fatalf("%s theme ignored NO_COLOR", theme)
		}
		for _, out := range []string{
			st.paint(st.bold, "text"),
			st.paint(st.error, "boom"),
			st.paint(st.modalBorder, "frame"),
		} {
			if strings.Contains(out, "\x1b[") {
				t.Fatalf("%s theme emitted ANSI under NO_COLOR: %q", theme, out)
			}
		}
	}
}

func TestStreamingTinyDeltasAccumulateContextTokensAndResetCache(t *testing.T) {
	m := newTestModel(t, Options{})
	m.width = 120
	m.height = 40

	// 40 tiny 2-byte SSE deltas = 80 bytes = 20 estimated tokens.
	// Previously len("ab")/4 == 0 dropped every single delta.
	for i := 0; i < 40; i++ {
		m.Update(agentEventMsg{event: agent.Event{Type: agent.EventTextDelta, Text: "ab"}})
	}
	if m.ctxTokens != 20 {
		t.Fatalf("ctxTokens after 40x2-byte deltas = %d, want 20", m.ctxTokens)
	}

	// Authoritative usage snaps ctxTokens and sets cache hit rate.
	m.Update(agentEventMsg{event: agent.Event{
		Type:          agent.EventUsage,
		Usage:         &provider.Usage{PromptTokens: 4000, CompletionTokens: 200, PromptCacheHitTokens: 3000, PromptCacheMissTokens: 1000},
		ContextTokens: 4200,
		CacheHitRate:  0.75,
	}})
	if m.ctxTokens != 4200 {
		t.Fatalf("ctxTokens after usage = %d, want 4200", m.ctxTokens)
	}
	if m.cache != 0.75 {
		t.Fatalf("cache after usage = %v, want 0.75", m.cache)
	}

	// Zero cache hit rate on a subsequent usage event must update m.cache to 0.
	m.Update(agentEventMsg{event: agent.Event{
		Type:          agent.EventUsage,
		Usage:         &provider.Usage{PromptTokens: 4500, CompletionTokens: 100, PromptCacheHitTokens: 0, PromptCacheMissTokens: 4500},
		ContextTokens: 4600,
		CacheHitRate:  0,
	}})
	if m.cache != 0 {
		t.Fatalf("cache after 0-hit usage = %v, want 0", m.cache)
	}

	// Compaction resets cache and snaps ctxTokens to compacted size.
	m.cache = 0.8
	m.Update(agentEventMsg{event: agent.Event{
		Type:          agent.EventCompact,
		ContextTokens: 900,
	}})
	if m.ctxTokens != 900 || m.cache != 0 {
		t.Fatalf("after compact: ctxTokens=%d cache=%v, want 900 and 0", m.ctxTokens, m.cache)
	}
}

func TestLiveSlashAutocompleteAndGhostCompletion(t *testing.T) {
	m := newTestModel(t, Options{Version: "v0.3.1"})
	m.width = 120
	m.height = 40

	// Typing "/" immediately shows the matching command list and ghost text.
	typeText(t, m, "/")
	if len(m.slashMenu) == 0 {
		t.Fatal("expected slash menu to open when typing '/'")
	}
	if m.ed.Ghost() == "" {
		t.Fatal("expected inline ghost completion when typing '/'")
	}

	// Filtering to "/st" narrows to stats & steer and updates ghost text.
	typeText(t, m, "st")
	if len(m.slashMenu) != 2 || m.slashMenu[0].name != "stats" || m.slashMenu[1].name != "steer" {
		t.Fatalf("slashMenu for /st = %+v, want [stats, steer]", m.slashMenu)
	}
	if m.ed.Ghost() != "ats" {
		t.Fatalf("ghost for /st = %q, want %q", m.ed.Ghost(), "ats")
	}
	view := m.View()
	if !strings.Contains(view, "/stats") || !strings.Contains(view, "/steer") {
		t.Fatalf("view missing live slash menu items:\n%s", view)
	}

	// Down arrow moves selection to /steer and updates ghost text.
	press(t, m, tea.KeyDown)
	if m.slashIndex != 1 || m.ed.Ghost() != "eer" {
		t.Fatalf("after Down: index=%d ghost=%q, want 1 and %q", m.slashIndex, m.ed.Ghost(), "eer")
	}

	// Tab completes the selected command and closes the menu.
	press(t, m, tea.KeyTab)
	if got := m.ed.Value(); got != "/steer " {
		t.Fatalf("after Tab: value=%q, want %q", got, "/steer ")
	}
	if len(m.slashMenu) != 0 || m.ed.Ghost() != "" {
		t.Fatalf("slash menu not cleared after Tab: len=%d ghost=%q", len(m.slashMenu), m.ed.Ghost())
	}

	// Esc dismisses the menu without clearing the editor text.
	m.ed.Reset()
	typeText(t, m, "/mo")
	if len(m.slashMenu) == 0 {
		t.Fatal("expected slash menu for /mo")
	}
	press(t, m, tea.KeyEsc)
	if len(m.slashMenu) != 0 || m.ed.Ghost() != "" {
		t.Fatalf("Esc did not dismiss slash menu: len=%d ghost=%q", len(m.slashMenu), m.ed.Ghost())
	}
	if m.ed.Value() != "/mo" {
		t.Fatalf("Esc altered editor text: %q", m.ed.Value())
	}
}

func TestVersionIndicatorAndUpdateBadge(t *testing.T) {
	m := newTestModel(t, Options{Version: "v0.3.1"})
	m.width = 120
	m.height = 40

	view := m.View()
	if !strings.Contains(view, "v0.3.1") {
		t.Fatalf("view missing version indicator:\n%s", view)
	}

	// Delivering an async update check shows the update indicator in welcome and sidebar.
	m.Update(updateCheckMsg{latest: "v0.3.2", available: true})
	view = m.View()
	if !strings.Contains(view, "v0.3.2") || !strings.Contains(view, "tern update") {
		t.Fatalf("view missing update available indicator:\n%s", view)
	}

	// /ver + Enter completes to /version and prints version + update info.
	typeText(t, m, "/ver")
	press(t, m, tea.KeyEnter)
	view = m.View()
	if !strings.Contains(view, "tern v0.3.1") || !strings.Contains(view, "update available: v0.3.2") {
		t.Fatalf("/version output missing version/update info:\n%s", view)
	}
}
