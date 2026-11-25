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
	"github.com/tern-agent/tern/internal/permission"
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
		for _, want := range []string{"Approval required", "bash", "rm -rf build", "mutating command"} {
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
	if !strings.Contains(view, "Ctrl+T to expand") {
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
	want := "deepseek-flash | ask | ctx 0% | cache 0% | 0 agents | $0.000 | main"
	if !strings.Contains(view, want) {
		t.Fatalf("status bar missing %q:\n%s", want, view)
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
