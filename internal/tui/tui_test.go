package tui

import (
	"context"
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
	if lines := strings.Count(view, "\n"); lines > 22 {
		t.Fatalf("stats view too tall (%d lines):\n%s", lines, view)
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
