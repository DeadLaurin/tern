package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/tern-agent/tern/internal/agent"
	terncontext "github.com/tern-agent/tern/internal/context"
	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/subagent"
	"github.com/tern-agent/tern/internal/tools"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	tickInterval     = 60 * time.Millisecond
	quitTimeout      = 3 * time.Second
	maxEditorRows    = 8
	compactDiffRows  = 12
	expandedDiffRows = 32
	yoloLabel        = "YOLO !"
)

// thinkingMode controls how reasoning entries are rendered.
type thinkingMode int

const (
	thinkingCollapsed thinkingMode = iota
	thinkingExpanded
	thinkingHidden
)

type entryKind int

const (
	entryInfo entryKind = iota
	entryUser
	entryAssistant
	entryReasoning
	entryTool
	entryError
)

type transcriptEntry struct {
	kind     entryKind
	text     string
	toolName string
	toolID   string
	args     string
	started  time.Time
	output   string
	isError  bool
	elapsed  time.Duration
	finished bool
}

type agentBlock struct {
	view subagent.AgentView
}

// queuedMessage is a prompt held back while a turn is running. Attachments are
// captured at submit time so pasting an image and pressing Enter never loses
// the image while the queue waits.
type queuedMessage struct {
	text        string
	attachments []provider.ContentPart
}

// steerer is the small capability the TUI needs from the agent to inject a
// mid-turn steering message. Using an interface keeps the TUI decoupled from
// the concrete agent implementation.
type steerer interface {
	Steer(string) bool
}

type approvalState struct {
	parent   *agent.ApprovalRequest
	sub      *subagent.ApprovalRequest
	response chan approvalResult
	// diffExpanded switches the approval card from a compact 12-line diff
	// preview to a wider 32-line view. It starts collapsed and is toggled by
	// pressing 'd' or clicking the [d] button.
	diffExpanded bool
}

type approvalResult struct {
	allow bool
	err   error
}

type askState struct {
	question string
	options  []string
	selected int
	input    []rune
	response chan string
}

// Model is the Bubble Tea model for the TUI.
type Model struct {
	opts    Options
	st      styles
	bus     *eventBus
	program *tea.Program
	baseCtx context.Context

	width  int
	height int

	// theme is the active palette name ("dark" or "light"). It is kept on the
	// model so /theme can toggle between the two without a restart.
	theme string

	viewport viewport.Model
	ed       editor

	entries      []*transcriptEntry
	curAssistant *transcriptEntry
	curReasoning *transcriptEntry
	toolEntries  map[string]*transcriptEntry

	pendingText      string
	pendingReasoning string

	agents      map[string]*agentBlock
	agentOrder  []string
	agentLines  map[string]*strings.Builder
	agentFocus  int
	agentsOpen  bool
	agentsPanel bool
	panelIndex  int

	// sidebarHidden collapses the right-hand inspector so the chat can use
	// the full terminal width. It is toggled with Ctrl+B.
	sidebarHidden bool
	sessionStart  time.Time

	thinking       thinkingMode
	showToolDetail bool

	// previousPermissionMode is remembered when YOLO is enabled so toggling it
	// off restores the mode the user was in before.
	previousPermissionMode permission.Mode

	running   bool
	turnCtx   context.Context
	cancel    context.CancelFunc
	turnStart time.Time

	// queued holds prompts submitted while a turn is running. They are
	// dispatched FIFO as turns complete. holdQueue suppresses exactly one
	// auto-dispatch when the user cancels a turn with Ctrl+C so queued work is
	// preserved until they explicitly run or clear it.
	queued    []queuedMessage
	holdQueue bool
	// pendingSteers remembers steering messages shown locally so the matching
	// agent "steer" event is not rendered twice.
	pendingSteers []string

	spinnerFrame int
	usingTools   bool

	// Live output throughput. Numeric deltas drive an estimate until the
	// provider reports an exact tokens_per_sec value.
	outputRunes int
	speedStart  time.Time
	liveSpeed   float64
	lastSpeed   float64

	usage         provider.Usage
	totalCost     float64
	cache         float64
	ctxTokens     int
	baseCtxTokens int
	streamChars   int
	ctxWindow     int
	historyChars  int

	quitArmed bool
	quitAt    time.Time
	statusMsg string

	pendingApproval *approvalState
	// approvalQueue holds requests that arrived while another approval was on
	// screen. It is strictly FIFO so concurrent sub-agents never overwrite or
	// jump ahead of one another.
	approvalQueue []*approvalState
	// turnAutoApprove is set by pressing 'a' in the approval prompt. It lasts
	// until the current turn ends and silently approves every remaining and
	// subsequent request for that turn.
	turnAutoApprove bool
	pendingAsk      *askState

	commands []terncontext.SlashCommand

	// historyPath is the persisted terminal-style chatbar history. It stays
	// empty when no config dir was supplied so tests and embedded use never
	// touch the user's home directory.
	historyPath string

	files []string

	// repoSummary is the quiet one-line repository index summary shown in the
	// welcome view. It is computed once at construction and omitted when empty.
	repoSummary string

	dirty      bool
	content    string
	lastWidth  int
	followTail bool

	// Modal state. activeModal is the enum the key router and view use;
	// modal carries the selected rows, filters and transient prompts.
	activeModal modalKind
	modal       modalState

	// clipboardFn is the clipboard writer. It defaults to copyToClipboard and
	// is injectable so tests stay hermetic.
	clipboardFn func(string) error

	// Mouse geometry recorded by the last paint. Views are shallow strings, so
	// each view records where its clickable rows landed before returning.
	entrySpans         []entrySpan
	sidebarAgentRows   map[int]string
	sidebarX           int
	viewportX          int
	statusHits         []statusHit
	statusX            int
	statusY            int
	modalRows          []modalRowHit
	modalAbsRows       map[int]int
	overlayHits        []overlayHit
	overlayTop         int
	overlayLeft        int
	agentsPanelRows    map[int]int
	agentsPanelAbsRows map[int]int
}

type repoSummaryMsg struct {
	summary string
}

// Init starts the UI ticker and kicks off the non-blocking repository index so
// frame 1 renders immediately on any repository size.
func (m *Model) Init() tea.Cmd {
	tick := tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
	workDir := m.opts.WorkDir
	if workDir == "" {
		return tick
	}
	return tea.Batch(tick, func() tea.Msg {
		if index, err := terncontext.IndexRepo(workDir, 80); err == nil && index != nil {
			return repoSummaryMsg{summary: index.Summary()}
		}
		return nil
	})
}

// Update handles one message. It never performs blocking agent work on the
// event loop.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case repoSummaryMsg:
		if msg.summary != "" {
			m.repoSummary = msg.summary
			m.dirty = true
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.dirty = true
		return m, nil

	case tickMsg:
		m.spinnerFrame++
		m.flushPending()
		m.syncAgents()
		if m.activeModal == modalAgentPeek {
			// Peeking reads the child transcript live, so keep repainting while
			// the sub-agent streams new output.
			m.dirty = true
		}
		if m.quitArmed && time.Since(m.quitAt) > quitTimeout {
			m.quitArmed = false
			if strings.Contains(m.statusMsg, "again to quit") {
				m.statusMsg = "Ready"
			}
		}
		if m.running {
			m.statusMsg = m.runningStatus()
		}
		if m.running || m.activeAgentCount() > 0 {
			m.dirty = true
		}
		return m, tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })

	case agentEventMsg:
		m.handleAgentEvent(msg.event)
		return m, nil

	case subagentEventMsg:
		m.handleSubagentEvent(msg.event)
		return m, nil

	case turnDoneMsg:
		m.handleTurnDone(msg)
		return m, nil

	case cmdResultMsg:
		if msg.err != nil {
			m.addError(msg.text + ": " + msg.err.Error())
		} else if !msg.quiet && strings.TrimSpace(msg.text) != "" {
			m.addInfo(msg.text)
		}
		return m, nil

	case approvalRequestMsg:
		m.flushPending()
		m.enqueueApproval(msg.request)
		return m, nil

	case askUserMsg:
		m.flushPending()
		m.pendingAsk = msg.request
		m.statusMsg = "Waiting for input"
		m.dirty = true
		m.followTail = true
		return m, nil

	case editorExternalMsg:
		if msg.err != nil {
			m.addError("external editor: " + msg.err.Error())
			return m, nil
		}
		m.ed.SetValue(strings.TrimRight(msg.content, "\n"))
		m.statusMsg = "Loaded prompt from external editor"
		return m, nil

	case tea.MouseMsg:
		if m.activeModal == modalAgentPeek {
			switch {
			case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelUp,
				msg.Type == tea.MouseWheelUp:
				m.scrollAgentPeek(-3)
				return m, nil
			case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelDown,
				msg.Type == tea.MouseWheelDown:
				m.scrollAgentPeek(3)
				return m, nil
			}
		}
		switch {
		case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelUp,
			msg.Type == tea.MouseWheelUp:
			m.viewport.ScrollUp(3)
			m.followTail = m.viewport.AtBottom()
			return m, nil
		case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelDown,
			msg.Type == tea.MouseWheelDown:
			m.viewport.ScrollDown(3)
			m.followTail = m.viewport.AtBottom()
			return m, nil
		}
		if msg.Action == tea.MouseActionPress {
			switch msg.Button {
			case tea.MouseButtonLeft:
				if m.handleLeftClick(msg) {
					return m, nil
				}
			case tea.MouseButtonRight:
				if m.activeModal == modalNone && !m.agentsPanel && m.handleTranscriptMouse(msg) {
					return m, nil
				}
			}
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		m.followTail = m.viewport.AtBottom()
		return m, cmd

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	// Any other key disarms a pending quit.
	if m.quitArmed && !(msg.Type == tea.KeyCtrlC) {
		m.quitArmed = false
		if strings.Contains(m.statusMsg, "again to quit") {
			m.statusMsg = "Ready"
		}
	}

	if m.pendingApproval != nil {
		return m.handleApprovalKey(msg)
	}
	if m.pendingAsk != nil {
		return m.handleAskKey(msg)
	}
	if msg.Type == tea.KeyCtrlS || (msg.Alt && msg.Type == tea.KeyRunes && strings.EqualFold(string(msg.Runes), "s")) {
		m.handleSteerKey()
		return nil
	}
	if m.agentsPanel {
		return m.handleAgentsPanelKey(msg)
	}
	if msg.Type == tea.KeyCtrlB {
		m.sidebarHidden = !m.sidebarHidden
		if m.sidebarHidden {
			m.statusMsg = "Sidebar hidden · Ctrl+B to show"
		} else {
			m.statusMsg = "Sidebar shown · Ctrl+B to hide"
		}
		m.dirty = true
		return nil
	}
	if m.activeModal != modalNone {
		return m.handleModalKey(msg)
	}

	if msg.Type == tea.KeyCtrlC {
		return m.handleCtrlC()
	}
	if msg.Type == tea.KeyCtrlT {
		switch m.thinking {
		case thinkingCollapsed:
			m.thinking = thinkingExpanded
			m.showToolDetail = true
			m.statusMsg = "Thinking expanded"
		case thinkingExpanded:
			m.thinking = thinkingHidden
			m.showToolDetail = false
			m.statusMsg = "Thinking hidden"
		default:
			m.thinking = thinkingCollapsed
			m.showToolDetail = false
			m.statusMsg = "Thinking collapsed"
		}
		m.dirty = true
		return nil
	}
	if msg.Type == tea.KeyCtrlY {
		m.toggleYolo()
		return nil
	}
	if msg.Type == tea.KeyCtrlO {
		m.toggleAgentFocus()
		return nil
	}
	if msg.Type == tea.KeyEnter && m.agentsOpen && len(m.agentOrder) > 0 && m.ed.IsEmpty() && !m.completionTokenActive() {
		m.openAgentPeek("")
		return nil
	}
	if m.agentsOpen && len(m.agentOrder) > 0 && msg.Type == tea.KeyRunes {
		switch string(msg.Runes) {
		case "c":
			m.cancelFocusedAgent()
			return nil
		case "C":
			if m.opts.Subagent != nil {
				n := m.opts.Subagent.CancelAll()
				m.addInfo(fmt.Sprintf("Cancelled %d sub-agent(s).", n))
			}
			m.dirty = true
			return nil
		}
	}
	if msg.Type == tea.KeyShiftTab {
		m.cycleReasoningEffort(1)
		return nil
	}
	if msg.Type == tea.KeyEsc {
		if m.agentsOpen {
			m.agentsOpen = false
			m.statusMsg = "Ready"
			m.dirty = true
		}
		return nil
	}

	switch msg.Type {
	case tea.KeyPgUp:
		m.viewport.PageUp()
		m.followTail = m.viewport.AtBottom()
		return nil
	case tea.KeyPgDown:
		m.viewport.PageDown()
		m.followTail = m.viewport.AtBottom()
		return nil
	case tea.KeyCtrlUp, tea.KeyCtrlDown:
		if msg.Type == tea.KeyCtrlUp {
			m.viewport.ScrollUp(3)
		} else {
			m.viewport.ScrollDown(3)
		}
		m.followTail = m.viewport.AtBottom()
		return nil
	case tea.KeyHome:
		if m.ed.IsEmpty() {
			m.viewport.GotoTop()
			m.followTail = m.viewport.AtBottom()
			return nil
		}
	case tea.KeyEnd:
		if m.ed.IsEmpty() {
			m.viewport.GotoBottom()
			m.followTail = true
			return nil
		}
	case tea.KeyUp:
		if msg.Alt {
			m.viewport.ScrollUp(3)
			m.followTail = m.viewport.AtBottom()
			return nil
		}
		if m.agentsOpen && m.ed.IsEmpty() && len(m.agentOrder) > 0 && !m.completionTokenActive() {
			m.cycleAgentFocus(-1)
			return nil
		}
	case tea.KeyDown:
		if msg.Alt {
			m.viewport.ScrollDown(3)
			m.followTail = m.viewport.AtBottom()
			return nil
		}
		if m.agentsOpen && m.ed.IsEmpty() && len(m.agentOrder) > 0 && !m.completionTokenActive() {
			m.cycleAgentFocus(1)
			return nil
		}
	}

	if msg.Type == tea.KeyTab && len(m.agentOrder) > 0 && !m.completionTokenActive() {
		m.toggleAgentFocus()
		return nil
	}

	if msg.Type == tea.KeyCtrlG {
		return m.openExternalEditor()
	}
	if msg.Type == tea.KeyCtrlP {
		m.openModelModal()
		return nil
	}

	return m.handleEditorKey(msg)
}

func (m *Model) handleEditorKey(msg tea.KeyMsg) tea.Cmd {
	if m.ed.SearchActive() {
		switch msg.Type {
		case tea.KeyCtrlR:
			m.ed.NextSearch()
			return nil
		case tea.KeyEsc:
			m.ed.CancelSearch()
			return nil
		case tea.KeyEnter:
			m.ed.AcceptSearch()
			return nil
		case tea.KeyBackspace:
			m.ed.SearchBackspace()
			return nil
		case tea.KeyRunes:
			m.ed.UpdateSearch(string(msg.Runes))
			return nil
		case tea.KeySpace:
			m.ed.UpdateSearch(" ")
			return nil
		default:
			return nil
		}
	}

	if msg.Paste && msg.Type == tea.KeyRunes {
		m.handlePaste(string(msg.Runes))
		return nil
	}

	switch msg.Type {
	case tea.KeyEnter:
		if msg.Alt || msg.String() == "shift+enter" || msg.String() == "ctrl+j" {
			m.ed.InsertNewline()
			return nil
		}
		return m.submit()
	case tea.KeyTab:
		m.complete()
		return nil
	case tea.KeyRunes:
		m.ed.InsertRunes(string(msg.Runes))
	case tea.KeySpace:
		m.ed.InsertRunes(" ")
	case tea.KeyBackspace:
		m.ed.Backspace()
	case tea.KeyDelete:
		m.ed.DeleteForward()
	case tea.KeyLeft:
		m.ed.MoveLeft()
	case tea.KeyRight:
		m.ed.MoveRight()
	case tea.KeyUp:
		m.ed.MoveUp()
	case tea.KeyDown:
		m.ed.MoveDown()
	case tea.KeyHome:
		m.ed.MoveLineStart()
	case tea.KeyEnd:
		m.ed.MoveLineEnd()
	case tea.KeyCtrlA:
		m.ed.MoveLineStart()
	case tea.KeyCtrlE:
		m.ed.MoveLineEnd()
	case tea.KeyCtrlW:
		m.ed.DeleteWordBackward()
	case tea.KeyCtrlK:
		m.ed.DeleteToLineEnd()
	case tea.KeyCtrlU:
		m.ed.DeleteToLineStart()
	case tea.KeyCtrlR:
		m.ed.StartSearch()
	}
	return nil
}

func (m *Model) handleCtrlC() tea.Cmd {
	if m.running {
		if m.cancel != nil {
			m.cancel()
		}
		m.cancel = nil
		m.running = false
		m.turnAutoApprove = false
		m.flushPending()
		m.curAssistant = nil
		m.curReasoning = nil
		if len(m.queued) > 0 {
			// Keep the queue intact: the user cancelled on purpose and should
			// choose when the next prompt runs.
			m.holdQueue = true
			m.addInfo(fmt.Sprintf("Turn cancelled (%d queued · /queue run or /queue clear)", len(m.queued)))
			m.statusMsg = fmt.Sprintf("Cancelled · %d queued", len(m.queued))
		} else {
			m.addInfo("Turn cancelled.")
			m.statusMsg = "Cancelled"
		}
		m.quitArmed = false
		m.dirty = true
		return nil
	}
	if m.quitArmed {
		return tea.Quit
	}
	m.quitArmed = true
	m.quitAt = time.Now()
	m.statusMsg = "Press Ctrl+C again to quit"
	m.addInfo("Press Ctrl+C again to quit.")
	return nil
}

func (m *Model) submit() tea.Cmd {
	text := strings.TrimSpace(m.ed.Value())
	if text == "" {
		return nil
	}
	attachments := m.ed.TakeAttachments()
	m.recordHistory(text)
	m.ed.Reset()
	m.flushPending()

	if strings.HasPrefix(text, "/") && !strings.Contains(text, "\n") {
		return m.handleSlash(text, attachments)
	}
	if m.running {
		m.queueMessage(text, attachments)
		return nil
	}
	return m.startTurn(text, attachments)
}

// recordHistory stores a submitted chatbar entry in memory and, when a global
// config dir was provided, mirrors it to the persistent history file.
func (m *Model) recordHistory(text string) {
	if !m.ed.AddHistory(text) {
		return
	}
	m.persistHistory()
}

// loadHistory restores the persisted terminal-style history. Missing or
// malformed lines are ignored so a damaged file never blocks startup.
func (m *Model) loadHistory() {
	if m.historyPath == "" {
		return
	}
	data, err := os.ReadFile(m.historyPath)
	if err != nil {
		return
	}
	var items []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry string
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if strings.TrimSpace(entry) != "" {
			items = append(items, entry)
		}
	}
	m.ed.SetHistory(items)
}

// persistHistory rewrites the history file atomically (temp file + rename). The
// file is JSONL so multi-line prompts round-trip exactly.
func (m *Model) persistHistory() {
	if m.historyPath == "" {
		return
	}
	dir := filepath.Dir(m.historyPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, "history-*.jsonl")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}
	for _, entry := range m.ed.History() {
		line, err := json.Marshal(entry)
		if err != nil {
			cleanup()
			return
		}
		if _, err := tmp.Write(append(line, '\n')); err != nil {
			cleanup()
			return
		}
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, m.historyPath); err != nil {
		os.Remove(tmpPath)
	}
}

// queueMessage appends a prompt to the FIFO queue shown above the editor.
func (m *Model) queueMessage(text string, attachments []provider.ContentPart) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	m.queued = append(m.queued, queuedMessage{text: text, attachments: attachments})
	m.statusMsg = fmt.Sprintf("%d queued · ^S steer", len(m.queued))
	m.dirty = true
}

// dequeueAndStart removes the first queued prompt and starts its turn.
func (m *Model) dequeueAndStart() tea.Cmd {
	if len(m.queued) == 0 {
		return nil
	}
	next := m.queued[0]
	m.queued = m.queued[1:]
	m.dirty = true
	return m.startTurn(next.text, next.attachments)
}

// steerRunning injects text into the active turn. It reports whether the steer
// was accepted.
func (m *Model) steerRunning(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if !m.running {
		m.statusMsg = "Nothing to steer · start a turn first"
		m.dirty = true
		return false
	}
	if m.opts.Agent == nil {
		m.addError("No agent configured.")
		return false
	}
	s, ok := any(m.opts.Agent).(steerer)
	if !ok {
		m.addError("Steering is not supported by this agent.")
		return false
	}
	if !s.Steer(text) {
		m.addError("Could not steer the running turn.")
		return false
	}
	m.addInfo("↳ Steered: " + text)
	m.pendingSteers = append(m.pendingSteers, text)
	m.statusMsg = "Steered the running turn"
	m.dirty = true
	return true
}

// handleSteerKey implements Ctrl+S / Alt+S: steer with the editor text, or
// promote the first queued message when the editor is empty.
func (m *Model) handleSteerKey() {
	if text := strings.TrimSpace(m.ed.Value()); text != "" {
		if m.steerRunning(text) {
			m.recordHistory(text)
			m.ed.Reset()
		}
		return
	}
	if len(m.queued) == 0 {
		m.statusMsg = "Nothing to steer · type a message or queue one"
		m.dirty = true
		return
	}
	next := m.queued[0]
	if !m.steerRunning(next.text) {
		return
	}
	m.queued = m.queued[1:]
	m.statusMsg = fmt.Sprintf("Steered queued message · %d left", len(m.queued))
	m.dirty = true
}

func (m *Model) startTurn(text string, attachments []provider.ContentPart) tea.Cmd {
	if m.opts.Agent == nil {
		m.addError("No agent configured.")
		return nil
	}
	if m.running {
		m.queueMessage(text, attachments)
		return nil
	}
	base := m.baseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	m.turnCtx = ctx
	m.cancel = cancel
	m.running = true
	m.turnStart = time.Now()
	m.curAssistant = nil
	m.curReasoning = nil
	m.usingTools = false
	m.followTail = true
	m.quitArmed = false
	m.outputRunes = 0
	m.speedStart = time.Time{}
	m.liveSpeed = 0
	m.lastSpeed = 0
	m.turnAutoApprove = false
	m.statusMsg = "Thinking…"
	m.addStreamContextChars(len(text))

	m.entries = append(m.entries, &transcriptEntry{kind: entryUser, text: text})
	if len(attachments) > 0 {
		m.addInfo(fmt.Sprintf("Attached %d image(s).", len(attachments)))
	}
	m.dirty = true

	go func() {
		res, err := m.opts.Agent.RunTurn(ctx, text, attachments)
		m.bus.Push(turnDoneMsg{result: res, err: err})
	}()
	return nil
}

func (m *Model) handleTurnDone(msg turnDoneMsg) {
	m.flushPending()
	m.running = false
	m.usingTools = false
	m.turnAutoApprove = false
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if msg.result != nil {
		m.usage = msg.result.TotalUsage
		m.totalCost = msg.result.TotalCostUSD
		m.cache = msg.result.CacheHitRate
		if msg.result.ContextTokens > 0 {
			m.setContextTokens(msg.result.ContextTokens)
		} else if m.opts.Agent != nil {
			m.setContextTokens(m.opts.Agent.ContextTokens())
		}
		if m.acceptSpeed(msg.result.TokensPerSec) {
			m.liveSpeed = msg.result.TokensPerSec
			m.lastSpeed = msg.result.TokensPerSec
		}
	}
	if msg.err != nil {
		if !errors.Is(msg.err, context.Canceled) && !errors.Is(msg.err, context.DeadlineExceeded) {
			m.addError(msg.err.Error())
			m.statusMsg = "Error"
		}
	} else if msg.result != nil {
		switch msg.result.StopReason {
		case agent.StopMaxSteps:
			m.addInfo("Stopped: maximum steps reached.")
		case agent.StopBudget:
			m.addInfo("Stopped: cost budget reached.")
		case agent.StopCancelled:
			m.statusMsg = "Cancelled"
		default:
			m.statusMsg = "Ready"
		}
	}
	m.curAssistant = nil
	m.curReasoning = nil
	m.dirty = true

	cancelled := errors.Is(msg.err, context.Canceled) ||
		errors.Is(msg.err, context.DeadlineExceeded) ||
		(msg.result != nil && msg.result.StopReason == agent.StopCancelled)
	if m.holdQueue {
		// A Ctrl+C cancellation already told the user the queue is held.
		m.holdQueue = false
		return
	}
	if !cancelled && len(m.queued) > 0 {
		m.dequeueAndStart()
	}
}

func (m *Model) handleAgentEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventTurnStart:
		m.running = true
		m.followTail = true
		m.dirty = true

	case agent.EventSteer:
		text := strings.TrimSpace(ev.Text)
		if text == "" {
			return
		}
		// Steering initiated from this UI is already in the transcript.
		for i, pending := range m.pendingSteers {
			if pending == text {
				m.pendingSteers = append(m.pendingSteers[:i], m.pendingSteers[i+1:]...)
				m.statusMsg = "Steered the running turn"
				m.dirty = true
				return
			}
		}
		m.addInfo("↳ Steered: " + text)
		m.statusMsg = "Steered the running turn"
		m.dirty = true

	case agent.EventReasoningDelta:
		m.pendingReasoning += ev.Text
		m.addStreamContextChars(len(ev.Text))

	case agent.EventTextDelta:
		m.pendingText += ev.Text
		m.addStreamContextChars(len(ev.Text))
		m.noteOutputStream(ev.Text)

	case agent.EventToolStart:
		m.flushPending()
		m.freezeTextSpeed()
		m.curAssistant = nil
		m.curReasoning = nil
		m.usingTools = true
		entry := &transcriptEntry{
			kind:     entryTool,
			toolName: ev.ToolName,
			toolID:   ev.ToolCallID,
			args:     string(ev.Args),
			started:  time.Now(),
		}
		m.entries = append(m.entries, entry)
		if m.toolEntries == nil {
			m.toolEntries = make(map[string]*transcriptEntry)
		}
		key := ev.ToolCallID
		if key == "" {
			key = ev.ToolName
		}
		m.toolEntries[key] = entry
		m.statusMsg = "Running " + ev.ToolName + "…"
		m.dirty = true

	case agent.EventToolEnd:
		m.flushPending()
		key := ev.ToolCallID
		if key == "" {
			key = ev.ToolName
		}
		entry := m.toolEntries[key]
		if entry == nil {
			entry = &transcriptEntry{kind: entryTool, toolName: ev.ToolName, toolID: ev.ToolCallID}
			m.entries = append(m.entries, entry)
			if m.toolEntries == nil {
				m.toolEntries = make(map[string]*transcriptEntry)
			}
			m.toolEntries[key] = entry
		}
		entry.output = ev.Output
		entry.isError = ev.IsError
		entry.elapsed = time.Duration(ev.DurationMS) * time.Millisecond
		entry.finished = true
		m.usingTools = false
		m.addStreamContextChars(len(ev.Output))
		m.statusMsg = "Thinking…"
		m.dirty = true

	case agent.EventUsage:
		if ev.Usage != nil {
			m.usage = addUsage(m.usage, *ev.Usage)
			if ev.ContextTokens > 0 {
				m.setContextTokens(ev.ContextTokens)
			} else if ev.Usage.PromptTokens > 0 {
				m.setContextTokens(ev.Usage.PromptTokens + ev.Usage.CompletionTokens)
			}
			m.cache = ev.CacheHitRate
		} else {
			if ev.ContextTokens > 0 {
				m.setContextTokens(ev.ContextTokens)
			}
			if ev.CacheHitRate > 0 {
				m.cache = ev.CacheHitRate
			}
		}
		if ev.TotalCostUSD > 0 {
			m.totalCost = ev.TotalCostUSD
		}
		if m.acceptSpeed(ev.TokensPerSec) {
			m.liveSpeed = ev.TokensPerSec
			m.lastSpeed = ev.TokensPerSec
		}

	case agent.EventTurnEnd:
		m.flushPending()
		m.running = false
		m.usingTools = false
		if ev.TotalCostUSD > 0 {
			m.totalCost = ev.TotalCostUSD
		}
		if ev.Usage != nil || ev.CacheHitRate > 0 {
			m.cache = ev.CacheHitRate
		}
		if ev.ContextTokens > 0 {
			m.setContextTokens(ev.ContextTokens)
		}
		if m.acceptSpeed(ev.TokensPerSec) {
			m.liveSpeed = ev.TokensPerSec
			m.lastSpeed = ev.TokensPerSec
		}
		m.curAssistant = nil
		m.curReasoning = nil
		m.dirty = true

	case agent.EventCompact:
		m.historyChars = 0
		m.cache = 0
		if ev.ContextTokens > 0 {
			m.setContextTokens(ev.ContextTokens)
		} else if m.opts.Agent != nil {
			m.setContextTokens(m.opts.Agent.ContextTokens())
		} else {
			m.setContextTokens(0)
		}
		if ev.Compact != nil {
			m.addInfo(fmt.Sprintf("⟳ Context compacted: %d → %d messages (%s) · cache prefix reset.",
				ev.Compact.MessagesBefore, ev.Compact.MessagesAfter, ev.Compact.Reason))
		} else if ev.Message != "" {
			m.addInfo("⟳ " + ev.Message)
		} else {
			m.addInfo("⟳ Context compacted · cache prefix reset.")
		}
		m.statusMsg = "Context compacted"
		m.dirty = true

	case agent.EventError:
		if ev.Reason == string(agent.StopCancelled) || strings.Contains(ev.Message, "context canceled") {
			return
		}
		m.flushPending()
		m.addError(ev.Message)
		m.statusMsg = "Error"

	case agent.EventCacheDebug:
		if ev.Message != "" {
			m.addInfo("cache: " + ev.Message)
		}
	}
}

func (m *Model) handleSubagentEvent(ev subagent.Event) {
	block := m.agents[ev.AgentID]
	if block == nil {
		block = &agentBlock{view: subagent.AgentView{ID: ev.AgentID}}
		m.agents[ev.AgentID] = block
		m.agentOrder = append(m.agentOrder, ev.AgentID)
	}
	if ev.AgentType != "" {
		block.view.AgentType = ev.AgentType
	}
	if ev.Description != "" {
		block.view.Description = ev.Description
	}
	if ev.Status != "" {
		block.view.Status = ev.Status
	}
	if ev.Step > 0 {
		block.view.Step = ev.Step
	}
	if ev.CurrentTool != "" {
		block.view.CurrentTool = ev.CurrentTool
	}
	if ev.Usage.TotalTokens > 0 {
		block.view.Usage = ev.Usage
	}
	if ev.CostUSD > 0 {
		block.view.CostUSD = ev.CostUSD
	}
	if ev.Elapsed > 0 {
		block.view.Elapsed = ev.Elapsed
	}

	switch ev.Type {
	case subagent.EventSubagentDelta:
		if m.agentLines[ev.AgentID] == nil {
			m.agentLines[ev.AgentID] = &strings.Builder{}
		}
		m.agentLines[ev.AgentID].WriteString(ev.Detail)
	case subagent.EventSubagentStep:
		if ev.CurrentTool != "" {
			addStep(&block.view, subagent.StepView{
				Tool:   ev.CurrentTool,
				Status: subagent.StatusRunning,
			})
		}
		m.dirty = true
	case subagent.EventSubagentApproval:
		if ev.CurrentTool != "" {
			addStep(&block.view, subagent.StepView{
				Tool:   ev.CurrentTool,
				Detail: ev.Detail,
				Status: subagent.StatusWaitingApproval,
			})
		}
		m.statusMsg = "Sub-agent approval required"
		m.dirty = true
	case subagent.EventSubagentStart:
		m.dirty = true
	case subagent.EventSubagentEnd:
		if ev.Summary != "" {
			block.view.Summary = ev.Summary
		}
		if ev.Detail != "" {
			block.view.Error = ev.Detail
		}
		m.flushAgentLine(ev.AgentID)
		m.dirty = true
	}
}

func addStep(view *subagent.AgentView, step subagent.StepView) {
	for _, existing := range view.Steps {
		if existing.Tool == step.Tool && existing.Detail == step.Detail {
			return
		}
	}
	view.Steps = append(view.Steps, step)
}

func (m *Model) flushAgentLine(id string) {
	b := m.agentLines[id]
	if b == nil {
		return
	}
	if s := b.String(); s != "" {
		if block := m.agents[id]; block != nil {
			block.view.Summary = s
		}
	}
	m.agentLines[id] = &strings.Builder{}
}

func (m *Model) flushPending() {
	if m.pendingReasoning == "" && m.pendingText == "" {
		return
	}
	if m.pendingReasoning != "" {
		if m.curReasoning == nil {
			m.curReasoning = &transcriptEntry{kind: entryReasoning}
			m.entries = append(m.entries, m.curReasoning)
		}
		m.curReasoning.text += m.pendingReasoning
		m.pendingReasoning = ""
	}
	if m.pendingText != "" {
		if m.curAssistant == nil {
			m.curAssistant = &transcriptEntry{kind: entryAssistant}
			m.entries = append(m.entries, m.curAssistant)
		}
		m.curAssistant.text += m.pendingText
		m.pendingText = ""
	}
	m.dirty = true
}

func (m *Model) addInfo(text string) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return
	}
	m.flushPending()
	m.entries = append(m.entries, &transcriptEntry{kind: entryInfo, text: text})
	m.dirty = true
}

func (m *Model) addError(text string) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return
	}
	m.flushPending()
	m.entries = append(m.entries, &transcriptEntry{kind: entryError, text: text})
	m.dirty = true
}

// View renders the whole interface.
func (m *Model) View() string {
	if m.width <= 0 {
		m.width = 100
	}
	if m.height <= 0 {
		m.height = 30
	}
	if m.running && m.pendingApproval == nil && m.pendingAsk == nil {
		m.statusMsg = m.runningStatus()
	}
	m.flushPending()
	m.rebuildContent()

	// Blocking prompts are true modal overlays: a card in the middle of the
	// terminal, never a transcript replacement that drifts with layout changes.
	if m.pendingApproval != nil {
		view := m.approvalView()
		m.positionOverlay(view)
		return m.finishView(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, view))
	}
	if m.pendingAsk != nil {
		view := m.askView()
		m.positionOverlay(view)
		return m.finishView(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, view))
	}

	colW := m.contentWidth()
	editorView := m.editorBoxView()
	todoView := m.todoView()
	agentView := m.agentsView()

	mainView := ""
	switch {
	case m.agentsPanel:
		mainView = m.agentsPanelView()
	case m.activeModal != modalNone:
		mainView = m.modalView()
	}
	if mainView != "" {
		if m.agentsPanel {
			m.positionAgentsPanel(mainView)
		} else {
			m.positionModal(mainView)
		}
		return m.finishView(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, mainView))
	}

	if m.twoPane() {
		return m.finishView(m.twoPaneView())
	}

	bottom := make([]string, 0, 5)
	if strings.TrimSpace(m.statusMsg) != "" {
		bottom = append(bottom, m.layoutBlock(m.st.paint(m.st.dim, m.statusMsg)))
	}
	if todoView != "" {
		bottom = append(bottom, m.layoutBlock(todoView))
	}
	if agentView != "" {
		bottom = append(bottom, m.layoutBlock(agentView))
	}
	if queueView := m.queueStripView(); queueView != "" {
		bottom = append(bottom, m.layoutBlock(queueView))
	}
	bottom = append(bottom, m.layoutBlock(editorView))

	used := 0
	for _, part := range bottom {
		used += lineCount(part)
	}
	// The status line is a single row underneath the editor.
	height := m.height - used - 1
	if height < 1 {
		height = 1
	}
	m.viewport.Width = colW
	m.viewport.Height = height
	m.viewportX = m.contentLeftPad()
	if m.followTail {
		m.viewport.GotoBottom()
	}
	transcript := m.layoutBlock(m.viewport.View())
	m.statusX = m.contentLeftPad()
	m.statusY = m.height - 1
	bottom = append(bottom, m.layoutBlock(m.statusView()))
	return m.finishView(strings.Join(append([]string{transcript}, bottom...), "\n"))
}

// twoPaneView renders the main chat pane beside the live inspector sidebar.
func (m *Model) twoPaneView() string {
	leftW := m.leftPaneWidth()
	innerW := maxInt(20, leftW-2)

	bottom := make([]string, 0, 3)
	if strings.TrimSpace(m.statusMsg) != "" {
		bottom = append(bottom, m.st.paint(m.st.dim, m.statusMsg))
	}
	if queueView := m.queueStripView(); queueView != "" {
		bottom = append(bottom, queueView)
	}
	bottom = append(bottom, m.editorBoxView(), m.statusView())

	used := 0
	for _, part := range bottom {
		used += lineCount(part)
	}
	height := m.height - used
	if height < 1 {
		height = 1
	}
	m.viewport.Width = innerW
	m.viewport.Height = height
	m.viewportX = 1
	if m.followTail {
		m.viewport.GotoBottom()
	}

	left := m.viewport.View()
	if len(bottom) > 0 {
		left += "\n" + strings.Join(bottom, "\n")
	}
	leftPane := m.padPane(left, leftW, innerW, m.height)
	divider := m.dividerView(m.height)
	m.statusX = 1
	m.statusY = m.height - 1
	m.sidebarX = leftW + 3
	rightPane := m.sidebarView(m.sidebarWidth(), m.height)
	return lipgloss.JoinHorizontal(lipgloss.Top, leftPane, divider, rightPane)
}

// contentWidth is the single fixed measure used by the transcript, editor and
// status bar. Computing it once (and never varying it per component) is what
// keeps the column perfectly still while typing.
func (m *Model) contentWidth() int {
	if m.twoPane() {
		return maxInt(20, m.leftPaneWidth()-2)
	}
	if m.width <= 80 {
		return maxInt(20, m.width)
	}
	width := m.width - 4
	if width > 96 {
		width = 96
	}
	return maxInt(20, width)
}

// contentLeftPad is the fixed left gutter of the centered column.
func (m *Model) contentLeftPad() int {
	pad := (m.width - m.contentWidth()) / 2
	if pad < 0 {
		pad = 0
	}
	return pad
}

// layoutBlock places every line of a block at the same fixed left gutter.
// Unlike centering each line independently, this keeps short and long lines
// (and the moving cursor) locked to one column.
func (m *Model) layoutBlock(block string) string {
	if block == "" {
		return ""
	}
	pad := m.contentLeftPad()
	if pad == 0 {
		return block
	}
	prefix := strings.Repeat(" ", pad)
	lines := strings.Split(block, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

// editorBoxView renders the input inside a subtle full-column rounded box.
// The prompt is left-aligned inside the box so typing never moves the frame.
// mainWidth is retained as an alias for the workspace measure.
func (m *Model) mainWidth() int { return m.contentWidth() }

func (m *Model) editorBoxView() string {
	colW := m.contentWidth()
	if m.st.plain {
		return m.ed.View(colW, m.st)
	}
	inner := maxInt(8, colW-4)
	body := m.ed.View(inner, m.st)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colorBorder)).
		Padding(0, 1).
		Width(maxInt(8, colW-2)).
		Render(body)
	return box
}

// queueStripView is a calm, two-line summary of prompts waiting behind the
// active turn. It deliberately uses only muted text so it reads as metadata,
// never as another panel.
func (m *Model) queueStripView() string {
	if len(m.queued) == 0 {
		return ""
	}
	preview := func(s string) string {
		s = strings.Join(strings.Fields(s), " ")
		return truncateCell(s, 32)
	}
	shown := m.queued
	if len(shown) > 2 {
		shown = shown[:2]
	}
	items := make([]string, 0, len(shown))
	for i, item := range shown {
		items = append(items, fmt.Sprintf("%d. \"%s\"", i+1, preview(item.text)))
	}
	head := fmt.Sprintf("Queued (%d):  %s", len(m.queued), strings.Join(items, "  ·  "))
	hint := "^S steer  ·  /queue clear"
	if len(m.queued) > len(shown) {
		hint = fmt.Sprintf("+%d more  ·  %s", len(m.queued)-len(shown), hint)
	}
	width := m.contentWidth()
	lines := []string{
		m.st.paint(m.st.dim, truncateCell(head, width)),
		m.st.paint(m.st.dim, truncateCell(hint, width)),
	}
	return strings.Join(lines, "\n")
}

// centerBlock horizontally centers a rendered block inside the terminal. It is
// used only for modal cards; the transcript/editor/status column uses the
// fixed layoutBlock gutter instead.
func (m *Model) centerBlock(block string) string {
	if block == "" || m.st.plain || m.width <= 80 {
		return block
	}
	return lipgloss.PlaceHorizontal(m.width, lipgloss.Center, block)
}

func (m *Model) finishView(view string) string {
	if !m.st.plain {
		return view
	}
	return asciiOnly(view)
}

// asciiOnly replaces the small set of decorative Unicode runes used by the UI
// with ASCII equivalents. It keeps NO_COLOR sessions usable on terminals that
// cannot render UTF-8 box drawing or status glyphs.
func asciiOnly(s string) string {
	replacer := strings.NewReplacer(
		"▸", ">", "❯", ">", "│", "|", "├", "|", "└", "`",
		"·", "|", "…", "...", "▌", "|", "─", "-", "⠋", "*",
		"✓", "[ok]", "✗", "[x]", "⚠", "!", "⊘", "[-]", "◐", "[~]",
		"○", "[ ]", "→", "->", "↑", "^", "↓", "v", "←", "<",
		"┃", "|", "●", "*", "┌", "+", "┐", "+", "┘", "+",
		"◌", "o", "✕", "x", "▾", "v",
	)
	return replacer.Replace(s)
}

func (m *Model) rebuildContent() {
	if !m.dirty && m.lastWidth == m.width {
		return
	}
	m.computeContextTokens()
	m.content = m.renderTranscript()
	m.viewport.SetContent(m.content)
	m.lastWidth = m.width
	m.dirty = false
	if m.followTail {
		m.viewport.GotoBottom()
	}
}

func (m *Model) setContextTokens(tokens int) {
	if tokens < 0 {
		tokens = 0
	}
	m.baseCtxTokens = tokens
	m.streamChars = 0
	m.ctxTokens = tokens
}

func (m *Model) addStreamContextChars(n int) {
	if n <= 0 {
		return
	}
	m.streamChars += n
	m.ctxTokens = m.baseCtxTokens + m.streamChars/4
}

func (m *Model) computeContextTokens() {
	if m.ctxTokens > 0 || m.opts.Agent != nil {
		return
	}
	chars := m.historyChars
	for _, entry := range m.entries {
		chars += len(entry.text) + len(entry.output)
	}
	m.setContextTokens(chars / 4)
}

func (m *Model) renderTranscript() string {
	m.entrySpans = m.entrySpans[:0]
	if len(m.entries) == 0 {
		return m.renderWelcome()
	}
	var blocks []string
	line := 0
	for _, entry := range m.entries {
		rendered := m.renderEntry(entry)
		if strings.TrimSpace(rendered) == "" {
			continue
		}
		lines := lineCount(rendered)
		m.entrySpans = append(m.entrySpans, entrySpan{entry: entry, start: line, end: line + lines})
		blocks = append(blocks, rendered)
		line += lines + 2 // renderTranscript joins blocks with a blank line
	}
	return strings.Join(blocks, "\n\n")
}

func (m *Model) renderWelcome() string {
	version := m.opts.Version
	if version == "" {
		version = "dev"
	}
	model := "unknown"
	effort := ""
	if m.opts.Config != nil {
		model = m.opts.Config.Model
		effort = strings.TrimSpace(m.opts.Config.ReasoningEffort)
	}
	modelLabel := model
	if effort != "" {
		modelLabel += " · " + effort
	}
	dim := func(text string) string { return m.st.paint(m.st.dim, text) }
	key := func(text string) string { return m.st.paint(m.st.info, text) }
	lines := make([]string, 0, 16)
	// The emblem is a quiet soaring-tern silhouette: omit it on terminals too
	// short or too narrow to show it without clipping.
	if m.height >= 20 && m.mainWidth() >= 40 {
		art := ternEmblemBlock
		if m.st.plain {
			art = ternEmblem
		}
		for _, line := range art {
			lines = append(lines, m.st.paint(m.st.quote, line))
		}
		lines = append(lines, "")
	}
	lines = append(lines,
		m.st.paint(m.st.heading, "tern"),
		m.st.paint(m.st.accent, modelLabel),
	)
	if strings.TrimSpace(m.repoSummary) != "" {
		lines = append(lines, dim(m.repoSummary))
	}
	lines = append(lines,
		"",
		dim(key("Ctrl+P")+dim(" model  ·  ")+key("Shift+Tab")+dim(" effort  ·  ")+key("/help")+dim(" commands")),
	)
	if !m.st.plain {
		return lipgloss.PlaceHorizontal(m.mainWidth(), lipgloss.Center, strings.Join(lines, "\n"))
	}
	return strings.Join(lines, "\n")
}

// ternEmblemBlock is the crisp half-block soaring-tern silhouette rendered on
// color/Unicode terminals.
var ternEmblemBlock = []string{
	"▄▄▄▄                                 ▄",
	" ▀████▄                           ▄███",
	"   ▀██████▄                    ▄█████ ",
	"     ▀▀██████▄▄▄            ▄▄█████▀  ",
	"       ▀▀████████▄      ▄▄███████▀    ",
	"          ▀████████▄ ▄▄██████▀▀       ",
	"            ███████████████▀          ",
	"             ▀███████████████▄▄       ",
	"      ██████████████████████▀▀▀█▄▄    ",
	"       ██████▀▀▀▀████▀▀▀▀▀            ",
}

// ternEmblem is the pure-ASCII fallback silhouette used for NO_COLOR / plain
// sessions so redirected output stays ANSI- and Unicode-free.
var ternEmblem = []string{
	" \\`.                  .'/",
	"  \\ `-.            .-' /",
	"   \\   `-.      .-'   /",
	"    `-.   `.  .'   .-'",
	"       `-.  \\/  .-'",
	"   _..---'`----'`---.._",
	"  `---..__      __..---'>",
}

func (m *Model) renderEntry(entry *transcriptEntry) string {
	width := maxInt(20, m.mainWidth()-2)
	switch entry.kind {
	case entryUser:
		marker := "❯ "
		if m.st.plain {
			marker = "> "
		}
		body := m.st.paint(m.st.userAccent, "┃ ") + m.st.paint(m.st.user, wrapPlainCell(entry.text, width-4))
		return m.st.paint(m.st.user, marker) + body

	case entryAssistant:
		return RenderMarkdown(entry.text, width, m.st.plain)

	case entryReasoning:
		if m.thinking == thinkingHidden {
			return ""
		}
		n := len([]rune(entry.text))
		if m.thinking == thinkingCollapsed {
			label := fmt.Sprintf("▸ Thought · %d chars  (^T expand)", n)
			return m.st.paint(m.st.reasoning, label)
		}
		header := m.st.paint(m.st.reasoning, "▾ Thought")
		body := RenderMarkdown(entry.text, maxInt(20, width-2), m.st.plain)
		lines := strings.Split(body, "\n")
		for i := range lines {
			lines[i] = m.st.paint(m.st.reasoning, "│ ") + m.st.paint(m.st.reasoning, lines[i])
		}
		return header + "\n" + strings.Join(lines, "\n")

	case entryTool:
		marker := "⚙ "
		if m.st.plain {
			marker = "* "
		}
		summary := toolSummary(entry.toolName, entry.args)
		head := m.st.paint(m.st.toolName, marker+entry.toolName)
		if summary != "" {
			head += m.st.paint(m.st.dim, " · "+summary)
		}
		if entry.finished && entry.elapsed > 0 {
			head += m.st.paint(m.st.dim, fmt.Sprintf(" · %s", formatDuration(entry.elapsed)))
		} else if !entry.finished && !entry.started.IsZero() {
			head += m.st.paint(m.st.dim, fmt.Sprintf(" · %s", formatDuration(time.Since(entry.started))))
		}
		if entry.isError {
			head += m.st.paint(m.st.error, " · failed")
		}
		if !m.showToolDetail {
			return head
		}
		var b strings.Builder
		b.WriteString(head)
		if strings.TrimSpace(entry.args) != "" && entry.args != "{}" && entry.args != "null" {
			b.WriteString("\n")
			b.WriteString(m.st.paint(m.st.dim, indentLines(wrapPlainCell(entry.args, width-4), "    ")))
		}
		if strings.TrimSpace(entry.output) != "" {
			b.WriteString("\n")
			output := truncateLines(entry.output, 20)
			style := m.st.dim
			if entry.isError {
				style = m.st.error
			}
			b.WriteString(m.st.paint(style, indentLines(wrapPlainCell(output, width-4), "    ")))
		}
		return b.String()

	case entryError:
		marker := "✗ "
		if m.st.plain {
			marker = "! "
		}
		return m.st.paint(m.st.error, marker+wrapPlainCell(entry.text, width-2))

	default: // info
		marker := "• "
		if m.st.plain {
			marker = "- "
		}
		return m.st.paint(m.st.info, marker) + m.st.paint(m.st.dim, wrapPlainCell(entry.text, width-2))
	}
}

func (m *Model) todoView() string {
	if m.opts.Todos == nil {
		return ""
	}
	items := m.opts.Todos.Items()
	if len(items) == 0 {
		return ""
	}
	lines := []string{m.st.paint(m.st.bold, "Tasks")}
	for i, item := range items {
		if i >= 7 {
			lines = append(lines, m.st.paint(m.st.dim, fmt.Sprintf("  … %d more", len(items)-i)))
			break
		}
		marker := "[ ]"
		style := m.st.dim
		switch item.Status {
		case tools.TodoInProgress:
			marker = "[~]"
			style = m.st.warn
		case tools.TodoCompleted:
			marker = "[x]"
			style = m.st.success
		case tools.TodoCancelled:
			marker = "[-]"
			style = m.st.dim
		}
		if !m.st.plain {
			switch item.Status {
			case tools.TodoInProgress:
				marker = "◐"
			case tools.TodoCompleted:
				marker = "✓"
			case tools.TodoCancelled:
				marker = "⊘"
			default:
				marker = "○"
			}
		}
		lines = append(lines, "  "+m.st.paint(style, marker+" "+wrapPlainCell(item.Content, m.width-8)))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) statusView() string {
	modelName := "unknown"
	effort := ""
	if m.opts.Config != nil {
		modelName = m.opts.Config.Model
		effort = strings.TrimSpace(m.opts.Config.ReasoningEffort)
	}
	modelLabel := modelName
	if effort != "" {
		modelLabel += " (" + effort + ")"
	}
	mode := "ask"
	if m.opts.Agent != nil && m.opts.Agent.Permissions != nil {
		mode = m.opts.Agent.Permissions.Mode().String()
	} else if m.opts.Config != nil && m.opts.Config.PermissionMode != "" {
		mode = m.opts.Config.PermissionMode
	}
	modeLabel := mode
	if mode == string(permission.ModeYolo) {
		modeLabel = yoloLabel
	}
	type statusPart struct {
		text   string
		action mouseAction
	}
	parts := []statusPart{{text: modelLabel, action: actionStatusModel}, {text: modeLabel, action: actionStatusYolo}}
	if branch := strings.TrimSpace(m.opts.GitBranch); branch != "" {
		parts = append(parts, statusPart{text: branch})
	}
	if speed := m.speed(); speed > 0 {
		parts = append(parts, statusPart{text: formatTokensPerSec(speed)})
	}
	if mode != string(permission.ModeYolo) {
		parts = append(parts, statusPart{text: "^Y yolo", action: actionStatusYolo})
	}
	parts = append(parts, statusPart{text: "^B panel", action: actionStatusPanel})
	if !m.followTail && !m.viewport.AtBottom() {
		parts = append(parts, statusPart{text: "↓ scrolled up", action: actionStatusBottom})
	}
	texts := make([]string, len(parts))
	m.statusHits = m.statusHits[:0]
	sep := 3 // both " · " and " | " occupy three columns
	x := 0
	for i, part := range parts {
		if i > 0 {
			x += sep
		}
		width := displayWidth(part.text)
		if part.action != actionNone {
			m.statusHits = append(m.statusHits, statusHit{start: x, end: x + width, action: part.action})
		}
		x += width
		texts[i] = part.text
	}
	text := m.st.statusSegments(texts...)
	width := m.contentWidth()
	if m.st.plain {
		return text
	}
	pad := width - displayWidth(text)
	if pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	return m.st.statusBar.Width(width).Render(text)
}

// speed returns the most useful throughput number for display: the live
// estimate while streaming, otherwise the last exact value.
func (m *Model) speed() float64 {
	if m.running && m.liveSpeed > 0 {
		return m.liveSpeed
	}
	if m.liveSpeed > 0 {
		return m.liveSpeed
	}
	return m.lastSpeed
}

func formatTokensPerSec(v float64) string {
	if v < 0 {
		v = 0
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f tok/s", v)
	}
	if v >= 10 {
		return fmt.Sprintf("%.0f tok/s", v)
	}
	return fmt.Sprintf("%.1f tok/s", v)
}

// noteOutputStream estimates text-only throughput from streamed deltas. The
// estimate is replaced by the provider's exact value when usage/turn-end
// events arrive.
func (m *Model) noteOutputStream(text string) {
	n := len([]rune(text))
	if n == 0 {
		return
	}
	if m.speedStart.IsZero() {
		m.speedStart = time.Now()
	}
	m.outputRunes += n
	elapsed := time.Since(m.speedStart)
	// A one-line preamble buffered in a couple of milliseconds is not
	// meaningful throughput; wait for either 50ms of streaming or a real
	// chunk of text before showing a number.
	if elapsed < 50*time.Millisecond && m.outputRunes < 40 {
		return
	}
	seconds := elapsed.Seconds()
	if seconds < 0.2 {
		seconds = 0.2
	}
	m.liveSpeed = (float64(m.outputRunes) / 4.0) / seconds
}

// acceptSpeed filters absurd sub-burst provider values. A tiny buffered
// preamble can report thousands of tok/s; only keep values that are plausible
// or backed by enough streamed text/time.
func (m *Model) acceptSpeed(v float64) bool {
	if v <= 0 {
		return false
	}
	if v <= 500 {
		return true
	}
	if m.outputRunes >= 40 {
		return true
	}
	return !m.speedStart.IsZero() && time.Since(m.speedStart) >= 50*time.Millisecond
}

// freezeTextSpeed ends the current text-speed window when a tool starts, so
// tool execution time is never included in tok/s. The last text-only value is
// kept on screen until new text starts streaming.
func (m *Model) freezeTextSpeed() {
	m.outputRunes = 0
	m.speedStart = time.Time{}
}

func (m *Model) runningStatus() string {
	frame := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
	if m.st.plain {
		frame = "*"
	}
	elapsed := formatDuration(time.Since(m.turnStart))
	speed := ""
	if m.liveSpeed > 0 {
		speed = " · " + formatTokensPerSec(m.liveSpeed)
	}
	if m.usingTools {
		return frame + " Running tools… " + elapsed + speed + m.queueStatusSuffix()
	}
	return frame + " Thinking… " + elapsed + speed + m.queueStatusSuffix()
}

// queueStatusSuffix keeps the live status aware of waiting prompts without
// crowding the spinner line.
func (m *Model) queueStatusSuffix() string {
	if len(m.queued) == 0 {
		return ""
	}
	return fmt.Sprintf(" · %d queued · ^S steer", len(m.queued))
}

func (m *Model) activeAgentCount() int {
	n := 0
	for _, id := range m.agentOrder {
		if block := m.agents[id]; block != nil {
			switch block.view.Status {
			case subagent.StatusQueued, subagent.StatusRunning, subagent.StatusWaitingApproval:
				n++
			}
		}
	}
	return n
}

func (m *Model) syncAgents() {
	if m.opts.Subagent == nil {
		return
	}
	list := m.opts.Subagent.List()
	changed := false
	for _, st := range list {
		block := m.agents[st.ID]
		if block == nil {
			block = &agentBlock{view: subagent.AgentView{ID: st.ID}}
			m.agents[st.ID] = block
			m.agentOrder = append(m.agentOrder, st.ID)
			changed = true
		}
		before := block.view
		block.view.AgentType = st.AgentType
		block.view.Description = st.Description
		block.view.Status = st.Status
		block.view.Step = st.Step
		block.view.CurrentTool = st.CurrentTool
		block.view.Usage = st.Usage
		block.view.CostUSD = st.CostUSD
		block.view.Error = st.Error
		if st.Summary != "" {
			block.view.Summary = st.Summary
		}
		if st.Status == subagent.StatusRunning || st.Status == subagent.StatusWaitingApproval || st.Status == subagent.StatusQueued {
			block.view.Elapsed = time.Since(st.StartedAt)
		}
		if before.Status != block.view.Status || before.Step != block.view.Step || before.CurrentTool != block.view.CurrentTool {
			changed = true
		}
		m.flushAgentLine(st.ID)
	}
	if changed {
		m.dirty = true
	}
}

func (m *Model) agentsExpanded() bool {
	return m.agentsOpen && len(m.agentOrder) > 0
}

// agentOpen is the expansion flag for the focused agent. It is kept separate
// from the panel visibility so Esc can collapse inspection without closing the
// panel unexpectedly.
func (m *Model) agentFocused() *agentBlock {
	if len(m.agentOrder) == 0 {
		return nil
	}
	if m.agentFocus < 0 || m.agentFocus >= len(m.agentOrder) {
		m.agentFocus = 0
	}
	return m.agents[m.agentOrder[m.agentFocus]]
}

func (m *Model) toggleAgentFocus() {
	if len(m.agentOrder) == 0 {
		m.addInfo("No sub-agents yet. Delegate work with the task tool.")
		return
	}
	if m.agentsOpen {
		m.agentsOpen = false
		m.statusMsg = "Ready"
		m.dirty = true
		return
	}
	m.agentsOpen = true
	if m.panelIndex >= len(m.agentOrder) {
		m.panelIndex = 0
	}
	m.agentFocus = m.panelIndex
	m.statusMsg = "Inspecting sub-agents (Esc to return)"
	m.dirty = true
}

func (m *Model) cycleAgentFocus(delta int) {
	if len(m.agentOrder) == 0 {
		return
	}
	m.agentFocus = (m.agentFocus + delta + len(m.agentOrder)) % len(m.agentOrder)
	m.panelIndex = m.agentFocus
	m.dirty = true
}

func (m *Model) completionCandidates(query string) []string {
	files := m.workspaceFiles()
	type scored struct {
		path  string
		score int
	}
	var matches []scored
	for _, path := range files {
		if score, ok := fuzzyScore(query, path); ok {
			matches = append(matches, scored{path: path, score: score})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score == matches[j].score {
			return matches[i].path < matches[j].path
		}
		return matches[i].score > matches[j].score
	})
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.path)
		if len(out) >= 30 {
			break
		}
	}
	return out
}

func (m *Model) workspaceFiles() []string {
	var files []string
	root := m.opts.WorkDir
	if root == "" {
		return files
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == ".git" || name == "node_modules" || name == "vendor" || name == ".tern") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		if len(files) >= 6000 {
			return filepath.SkipAll
		}
		return nil
	})
	m.files = files
	return files
}

func (m *Model) completionTokenActive() bool {
	start, _, token := m.ed.TokenAtCursor()
	if strings.HasPrefix(token, "@") {
		return true
	}
	return start == 0 && strings.HasPrefix(token, "/")
}

func (m *Model) complete() {
	start, end, token := m.ed.TokenAtCursor()
	if token == "" {
		return
	}
	var candidates []string
	var replacement func(string) string
	switch {
	case strings.HasPrefix(token, "@"):
		query := strings.TrimPrefix(token, "@")
		candidates = m.completionCandidates(query)
		replacement = func(path string) string { return "@" + path }
	case strings.HasPrefix(token, "/") && start == 0:
		query := strings.ToLower(strings.TrimPrefix(token, "/"))
		for _, name := range builtinCommandNames {
			if strings.HasPrefix(name, query) {
				candidates = append(candidates, "/"+name)
			}
		}
		for _, cmd := range m.commands {
			value := "/" + cmd.Name
			if strings.HasPrefix(strings.ToLower(cmd.Name), query) {
				found := false
				for _, existing := range candidates {
					if existing == value {
						found = true
						break
					}
				}
				if !found {
					candidates = append(candidates, value)
				}
			}
		}
		sort.Strings(candidates)
		replacement = func(value string) string { return value + " " }
	default:
		return
	}
	if len(candidates) == 0 {
		m.ed.ClearSuggestions()
		return
	}
	index := 0
	if m.ed.suggestionToken == token {
		index = (m.ed.suggestionIndex + 1) % len(candidates)
	}
	value := candidates[index]
	m.ed.ReplaceRange(start, end, replacement(value))
	m.ed.SetSuggestions(token, candidates, index)
}

func (m *Model) handlePaste(text string) {
	if m.attachPastedPaths(text) {
		return
	}
	m.ed.InsertRunes(text)
}

func (m *Model) attachPastedPaths(text string) bool {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var cleaned []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.Trim(line, `"'`)
		if line == "" {
			continue
		}
		cleaned = append(cleaned, line)
	}
	if len(cleaned) == 0 {
		return false
	}
	var replaced []string
	var attachments []provider.ContentPart
	for _, candidate := range cleaned {
		path := candidate
		if strings.HasPrefix(path, "file://") {
			path = strings.TrimPrefix(path, "file://")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(m.opts.WorkDir, path)
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return false
		}
		if isImagePath(path) {
			part, err := imagePart(path)
			if err != nil {
				return false
			}
			attachments = append(attachments, part)
			replaced = append(replaced, "[image: "+filepath.Base(path)+"]")
			continue
		}
		rel, err := filepath.Rel(m.opts.WorkDir, path)
		if err != nil {
			rel = path
		}
		replaced = append(replaced, "@"+filepath.ToSlash(rel))
	}
	for _, part := range attachments {
		m.ed.AddAttachment(part)
	}
	m.ed.InsertRunes(strings.Join(replaced, " "))
	if len(attachments) > 0 {
		m.addInfo(fmt.Sprintf("Attached %d image(s).", len(attachments)))
	}
	return true
}

func imagePart(path string) (provider.ContentPart, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return provider.ContentPart{}, err
	}
	mime := "image/png"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".gif":
		mime = "image/gif"
	case ".webp":
		mime = "image/webp"
	}
	return provider.ContentPart{
		Type:     "image_url",
		ImageURL: &provider.ImageURL{URL: "data:" + mime + ";base64," + base64Encode(data)},
	}, nil
}

func isImagePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

func (m *Model) openExternalEditor() tea.Cmd {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		m.addError("Set $EDITOR or $VISUAL to use Ctrl+G.")
		return nil
	}
	dir := os.TempDir()
	file, err := os.CreateTemp(dir, "tern-prompt-*.md")
	if err != nil {
		m.addError("external editor: " + err.Error())
		return nil
	}
	path := file.Name()
	content := m.ed.Value()
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		os.Remove(path)
		m.addError("external editor: " + err.Error())
		return nil
	}
	file.Close()

	fields := strings.Fields(editor)
	args := append(fields[1:], path)
	cmd := execCommand(fields[0], args...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer os.Remove(path)
		if err != nil {
			return editorExternalMsg{err: err}
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return editorExternalMsg{err: readErr}
		}
		return editorExternalMsg{content: string(data)}
	})
}

// toolSummary renders a one-line description of a tool call.
func toolSummary(name, rawArgs string) string {
	args := strings.TrimSpace(rawArgs)
	if args == "" || args == "{}" || args == "null" {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(args), &decoded); err != nil {
		return truncateCell(args, 80)
	}
	for _, key := range []string{"command", "path", "file_path", "pattern", "query", "url", "description", "name"} {
		if value, ok := decoded[key]; ok {
			if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
				return truncateCell(s, 90)
			}
		}
	}
	return truncateCell(args, 80)
}

func addUsage(a, b provider.Usage) provider.Usage {
	return provider.Usage{
		PromptTokens:          a.PromptTokens + b.PromptTokens,
		CompletionTokens:      a.CompletionTokens + b.CompletionTokens,
		TotalTokens:           a.TotalTokens + b.TotalTokens,
		PromptCacheHitTokens:  a.PromptCacheHitTokens + b.PromptCacheHitTokens,
		PromptCacheMissTokens: a.PromptCacheMissTokens + b.PromptCacheMissTokens,
		ReasoningTokens:       a.ReasoningTokens + b.ReasoningTokens,
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
}

func truncateLines(s string, max int) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if len(lines) > max {
		lines = append(lines[:max], fmt.Sprintf("… %d more lines", len(lines)-max))
	}
	return strings.Join(lines, "\n")
}

func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func truncateCell(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if displayWidth(s) <= max {
		return s
	}
	head, _ := splitWidth(s, max)
	return head + "…"
}

func wrapPlainCell(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			out = append(out, "")
			continue
		}
		var current string
		for _, word := range strings.Fields(line) {
			if current == "" {
				current = word
				continue
			}
			if displayWidth(current)+1+displayWidth(word) <= width {
				current += " " + word
			} else {
				out = append(out, current)
				current = word
			}
		}
		if current != "" {
			out = append(out, current)
		}
	}
	return strings.Join(out, "\n")
}

// fuzzyScore matches query runes as an ordered subsequence. Higher scores mean
// prefix, substring, or boundary matches.
func fuzzyScore(query, candidate string) (int, bool) {
	q := []rune(strings.ToLower(query))
	if len(q) == 0 {
		return 1, true
	}
	c := []rune(strings.ToLower(candidate))
	if strings.HasPrefix(string(c), string(q)) {
		return 1000 - len(c), true
	}
	if strings.Contains(string(c), string(q)) {
		return 500 - len(c), true
	}
	score := 0
	pos := 0
	last := -2
	for _, qr := range q {
		found := false
		for pos < len(c) {
			if c[pos] == qr {
				if pos == last+1 {
					score += 4
				} else {
					score++
				}
				last = pos
				pos++
				found = true
				break
			}
			pos++
		}
		if !found {
			return 0, false
		}
	}
	return score, true
}

func base64Encode(data []byte) string {
	// Kept in a helper so tests can exercise image attachment without pulling
	// in an extra import at the call site.
	return base64Encoding.EncodeToString(data)
}
