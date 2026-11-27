package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tern-agent/tern/internal/checkpoint"
	"github.com/tern-agent/tern/internal/config"
	terncontext "github.com/tern-agent/tern/internal/context"
	"github.com/tern-agent/tern/internal/permission"
	"github.com/tern-agent/tern/internal/provider"
)

// builtinCommandNames lists every built-in slash command. It drives completion
// and /help.
var builtinCommandNames = []string{
	"agents",
	"balance",
	"changelog",
	"clear",
	"compact",
	"config",
	"context",
	"copy",
	"cost",
	"effort",
	"export",
	"ext",
	"fork",
	"help",
	"mcp",
	"model",
	"mouse",
	"name",
	"plan",
	"plugins",
	"provider",
	"queue",
	"quit",
	"rewind",
	"sessions",
	"skills",
	"stats",
	"steer",
	"theme",
	"thinking",
	"tools",
	"undo",
	"version",
	"yolo",
}

var builtinCommandDescriptions = map[string]string{
	"agents":    "Inspect sub-agents",
	"balance":   "Check remaining provider credit balance",
	"changelog": "Show what's new in tern",
	"clear":     "Clear transcript view",
	"compact":  "Compact conversation context",
	"config":   "Show effective settings",
	"context":  "Show context token usage",
	"copy":     "Copy last assistant response",
	"cost":     "Show session cost & cache",
	"effort":   "Set reasoning effort",
	"export":   "Export session transcript",
	"ext":      "List loaded plugins (alias)",
	"fork":     "Fork current session",
	"help":     "Show commands & shortcuts",
	"mcp":      "Inspect MCP servers",
	"model":    "Switch model & reasoning",
	"mouse":    "Toggle TUI mouse capture (native select)",
	"name":     "Rename current session",
	"plan":     "Toggle read-only plan mode",
	"plugins":  "List loaded plugins",
	"provider": "Configure or switch AI provider",
	"queue":    "Inspect or clear queued prompts",
	"quit":     "Exit tern",
	"rewind":   "Rewind edits to an earlier turn",
	"sessions": "Browse & resume sessions",
	"skills":   "Browse & load skills",
	"stats":    "Usage & cache dashboard",
	"steer":    "Steer active turn mid-flight",
	"theme":    "Switch color theme",
	"thinking": "Set reasoning effort or visibility",
	"tools":    "Toggle tool display (compact/expanded/hidden)",
	"undo":     "Undo latest edit turn",
	"version":  "Show version & update status",
	"yolo":     "Toggle allow-all permission mode",
}

// handleSlash dispatches a slash command. Custom commands loaded from the
// project/global commands directories are expanded and submitted as a prompt.
func (m *Model) handleSlash(line string, attachments []provider.ContentPart) tea.Cmd {
	line = strings.TrimSpace(strings.TrimPrefix(line, "/"))
	if line == "" {
		m.addError("Empty command. Type /help for available commands.")
		return nil
	}
	name, args := splitCommandLine(line)
	switch name {
	case "help":
		m.openHelpModal()
		return nil

	case "model":
		return m.cmdModel(args)

	case "provider", "providers":
		m.openProviderModal(false)
		return nil

	case "balance":
		return m.fetchBalanceCmd(true)

	case "effort", "thinking":
		return m.cmdEffort(args)

	case "theme":
		return m.cmdTheme(args)

	case "plan":
		return m.cmdPlan()

	case "steer":
		return m.cmdSteer(args)

	case "queue":
		return m.cmdQueue(args)

	case "yolo":
		m.toggleYolo()
		return nil

	case "version":
		return m.cmdVersion()

	case "changelog", "whatsnew":
		m.openChangelogModal()
		return nil

	case "copy":
		for i := len(m.entries) - 1; i >= 0; i-- {
			if m.entries[i].kind == entryAssistant && strings.TrimSpace(m.entries[i].text) != "" {
				m.copyText(m.entries[i].text)
				return nil
			}
		}
		m.addInfo("No assistant message to copy yet.")
		return nil

	case "tools":
		return m.cmdTools(args)

	case "mouse":
		m.mouseDisabled = !m.mouseDisabled
		if m.mouseDisabled {
			m.statusMsg = "Mouse capture off — native terminal selection active (/mouse to re-enable)"
			m.dirty = true
			return tea.DisableMouse
		}
		m.statusMsg = "Mouse capture on — drag to select & copy"
		m.dirty = true
		return tea.EnableMouseCellMotion

	case "compact":
		return m.cmdCompact(args)

	case "cost":
		m.addInfo(m.costText())
		return nil

	case "context":
		m.addInfo(m.contextText())
		return nil

	case "agents":
		if strings.TrimSpace(args) != "" {
			m.openAgentPeekArg(args)
			return nil
		}
		m.agentsPanel = true
		m.agentsOpen = false
		if len(m.agentOrder) > 0 {
			if m.panelIndex >= len(m.agentOrder) {
				m.panelIndex = 0
			}
			m.statusMsg = "Sub-agents"
		}
		m.dirty = true
		return nil

	case "clear":
		m.entries = nil
		m.pendingText = ""
		m.pendingReasoning = ""
		m.curAssistant = nil
		m.curReasoning = nil
		m.toolEntries = nil
		m.content = ""
		m.addInfo("Cleared the transcript view. Session history on disk is unchanged.")
		return nil

	case "undo":
		return m.cmdUndo()

	case "rewind":
		return m.cmdRewind(args)

	case "sessions":
		return m.cmdSessions(args)

	case "fork":
		return m.cmdFork(args)

	case "name":
		return m.cmdName(args)

	case "export":
		return m.cmdExport(args)

	case "skills":
		return m.cmdSkills(args)

	case "mcp":
		m.openMCPModal()
		return nil

	case "plugin", "plugins", "ext":
		m.openExtModal()
		return nil

	case "stats":
		m.openStatsModal()
		return nil

	case "config":
		if m.opts.Config == nil {
			m.addError("No configuration loaded.")
		} else {
			m.addInfo(configSummary(m.opts.Config))
		}
		return nil

	case "quit":
		return tea.Quit
	}

	for _, cmd := range m.commands {
		if strings.EqualFold(cmd.Name, name) {
			expanded := cmd.Expand(args)
			if strings.TrimSpace(expanded) == "" {
				m.addError("Custom command /" + cmd.Name + " expanded to an empty prompt.")
				return nil
			}
			return m.startTurn(expanded, attachments)
		}
	}

	m.addError("Unknown command /" + name + ". Type /help for available commands.")
	return nil
}

func splitCommandLine(line string) (string, string) {
	fields := strings.SplitN(line, " ", 2)
	name := strings.TrimSpace(fields[0])
	args := ""
	if len(fields) > 1 {
		args = strings.TrimSpace(fields[1])
	}
	return name, args
}

func configSummary(cfg *config.Config) string {
	key := "[not set]"
	if strings.TrimSpace(cfg.APIKey) != "" {
		key = "[set]"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "api_key = %q\n", key)
	fmt.Fprintf(&b, "model = %q\n", cfg.Model)
	fmt.Fprintf(&b, "base_url = %q\n", cfg.BaseURL)
	fmt.Fprintf(&b, "reasoning_effort = %q\n", cfg.ReasoningEffort)
	fmt.Fprintf(&b, "permission_mode = %q\n", cfg.PermissionMode)
	fmt.Fprintf(&b, "web_permission = %q\n", cfg.WebPermission)
	fmt.Fprintf(&b, "max_steps = %d\n", cfg.MaxSteps)
	fmt.Fprintf(&b, "max_cost_usd = %g\n", cfg.MaxCostUSD)
	fmt.Fprintf(&b, "max_agents = %d\n", cfg.MaxAgents)
	fmt.Fprintf(&b, "max_agent_depth = %d\n", cfg.MaxAgentDepth)
	fmt.Fprintf(&b, "auto_compact_threshold = %g\n", cfg.AutoCompactThreshold)
	fmt.Fprintf(&b, "shell = %q\n", cfg.Shell)
	fmt.Fprintf(&b, "search_backend = %q\n", cfg.SearchBackend)
	fmt.Fprintf(&b, "pricing_models = %d\n", len(cfg.Pricing))
	return strings.TrimRight(b.String(), "\n")
}

func (m *Model) helpText() string {
	var b strings.Builder
	b.WriteString("Slash commands:\n")
	help := map[string]string{
		"/help":                    "show this help",
		"/model [name]":            "open the model selector or switch directly",
		"/stats":                   "usage and activity log across sessions",
		"/plan":                    "toggle read-only plan mode",
		"/theme [dark|light]":      "switch the palette, or toggle when omitted",
		"/yolo":                    "toggle dangerous allow-all mode (Ctrl+Y)",
		"/compact [text]":          "summarize older context (optional instructions)",
		"/steer <text>":            "steer the running turn, or start one when idle (Ctrl+S)",
		"/queue [clear|run]":       "show, clear, or run queued prompts",
		"/cost":                    "show token usage and estimated cost",
		"/context":                 "show context window usage",
		"/agents":                  "inspect, cancel one, or cancel all sub-agents",
		"/clear":                   "clear the transcript view",
		"/undo":                    "restore files from the latest checkpoint",
		"/rewind [turn]":           "list turns or restore the state before a turn",
		"/sessions [query]":        "browse, resume, fork, rename or delete sessions",
		"/fork [name]":             "fork the current session transcript",
		"/name <name>":             "rename the current session",
		"/export [md|html] [path]": "export the session transcript",
		"/skills [name]":           "browse skills or show one directly",
		"/mcp":                     "MCP servers, tools and reconnect",
		"/ext":                     "browse installed extensions, their tools, hooks and MCP servers",
		"/config":                  "show the effective configuration",
		"/quit":                    "exit tern",
	}
	names := make([]string, 0, len(help))
	for name := range help {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "  %-28s %s\n", name, help[name])
	}
	if len(m.commands) > 0 {
		b.WriteString("\nCustom commands:\n")
		for _, cmd := range m.commands {
			desc := cmd.Description
			if desc == "" {
				desc = cmd.Path
			}
			fmt.Fprintf(&b, "  /%-26s %s\n", cmd.Name, desc)
		}
	}
	b.WriteString("\nKeys: enter send · shift/alt+enter newline · ctrl+s steer · ctrl+r history search · ctrl+t thinking · ctrl+y yolo · pgup/pgdn scroll · ctrl+g $EDITOR · tab complete · ctrl+c cancel/quit")
	return strings.TrimRight(b.String(), "\n")
}

// cmdSteer steers the active turn, or starts a fresh turn when idle.
func (m *Model) cmdSteer(args string) tea.Cmd {
	text := strings.TrimSpace(args)
	if text == "" {
		m.addError("Usage: /steer <text>")
		return nil
	}
	if m.running {
		m.steerRunning(text)
		return nil
	}
	return m.startTurn(text, nil)
}

// cmdQueue inspects and manages prompts waiting behind the active turn.
func (m *Model) cmdQueue(args string) tea.Cmd {
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "clear":
		n := len(m.queued)
		m.queued = nil
		m.holdQueue = false
		m.dirty = true
		if n == 0 {
			m.addInfo("Queue is already empty.")
			return nil
		}
		m.addInfo(fmt.Sprintf("Cleared %d queued message(s).", n))
		return nil
	case "run":
		if len(m.queued) == 0 {
			m.addInfo("Queue is empty.")
			return nil
		}
		if m.running {
			m.addInfo(fmt.Sprintf("%d queued · waiting for the current turn to finish.", len(m.queued)))
			return nil
		}
		return m.dequeueAndStart()
	case "":
		if len(m.queued) == 0 {
			m.addInfo("Queue is empty.")
			return nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Queued (%d):\n", len(m.queued))
		for i, item := range m.queued {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, truncateCell(strings.Join(strings.Fields(item.text), " "), 80))
		}
		b.WriteString("\n^S steer · /queue run · /queue clear")
		m.addInfo(b.String())
		return nil
	default:
		m.addError("Usage: /queue [clear|run]")
		return nil
	}
}

func (m *Model) cmdModel(args string) tea.Cmd {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return m.openModelModal()
	}
	if strings.EqualFold(trimmed, "free") {
		return m.openModelModalWithFilter(true)
	}
	name := strings.TrimSpace(strings.Fields(trimmed)[0])
	m.applyModel(name)
	m.addInfo("Model set to " + name + ".")
	return nil
}

// cmdEffort sets the reasoning effort immediately, or opens the model/effort
// selector when invoked without an argument.
func (m *Model) cmdEffort(args string) tea.Cmd {
	level := strings.ToLower(strings.TrimSpace(args))
	if level == "" {
		m.openModelModal()
		m.statusMsg = "Reasoning effort · choose with ←/→ or 1-4"
		return nil
	}
	if !containsString(reasoningEfforts, level) {
		m.addError("effort: expected one of none, low, high, max")
		return nil
	}
	m.applyReasoningEffort(level)
	m.statusMsg = "Reasoning effort: " + level
	m.addInfo("Reasoning effort set to " + level + ".")
	return nil
}

// cmdTheme switches between the calm neutral dark palette and the crisp
// paper-toned light palette. With no argument it toggles the current theme.
func (m *Model) cmdTheme(args string) tea.Cmd {
	theme := strings.ToLower(strings.TrimSpace(args))
	switch theme {
	case "":
		if m.theme == "light" {
			theme = "dark"
		} else {
			theme = "light"
		}
	case "dark", "light":
	default:
		m.addError("Usage: /theme [dark|light]")
		return nil
	}
	m.theme = theme
	m.st = newStylesForTheme(noColor(m.opts.NoColor), theme)
	m.dirty = true
	m.statusMsg = "Theme: " + theme
	m.addInfo("Theme set to " + theme + ".")
	return nil
}

func (m *Model) cmdPlan() tea.Cmd {
	mode := permission.ModeAsk
	current := permission.ModeAsk
	if m.opts.Agent != nil && m.opts.Agent.Permissions != nil {
		current = m.opts.Agent.Permissions.Mode()
	}
	if current == permission.ModePlan {
		mode = permission.ModeAsk
	} else {
		mode = permission.ModePlan
	}
	m.applyPermissionMode(mode)
	if mode == permission.ModePlan {
		m.addInfo("Plan mode enabled: writes and mutating commands are denied.")
	} else {
		m.addInfo("Plan mode disabled: permission mode is ask.")
	}
	return nil
}

// applyPermissionMode updates both the configured mode and the live evaluator
// used by the running agent. Toggling modes never requires restarting a session.
func (m *Model) applyPermissionMode(mode permission.Mode) {
	if m.opts.Config != nil {
		m.opts.Config.PermissionMode = mode.String()
	}
	if m.opts.Agent != nil {
		if m.opts.Agent.Permissions == nil {
			m.opts.Agent.Permissions = permission.New(permission.Policy{
				Mode:    mode,
				WorkDir: m.opts.WorkDir,
			})
		} else {
			policy := m.opts.Agent.Permissions.Policy()
			policy.Mode = mode
			policy.WorkDir = m.opts.WorkDir
			m.opts.Agent.Permissions = permission.New(policy)
		}
		if m.opts.Subagent != nil {
			m.opts.Subagent.SetPermission(m.opts.Agent.Permissions)
		}
		if m.opts.Agent.Config != nil {
			m.opts.Agent.Config.PermissionMode = mode.String()
		} else {
			m.opts.Agent.Config = m.opts.Config
		}
	}
	m.dirty = true
}

// toggleYolo flips dangerous allow-all mode and restores the previous mode when
// it is switched off again.
func (m *Model) toggleYolo() {
	current := permission.ModeAsk
	if m.opts.Agent != nil && m.opts.Agent.Permissions != nil {
		current = m.opts.Agent.Permissions.Mode()
	} else if m.opts.Config != nil && m.opts.Config.PermissionMode != "" {
		if parsed, err := permission.ParseMode(m.opts.Config.PermissionMode); err == nil {
			current = parsed
		}
	}
	if current == permission.ModeYolo {
		target := m.previousPermissionMode
		if target == "" || target == permission.ModeYolo {
			target = permission.ModeAsk
		}
		m.applyPermissionMode(target)
		m.addInfo("YOLO mode disabled: permission mode is " + target.String() + ".")
		return
	}
	m.previousPermissionMode = current
	m.applyPermissionMode(permission.ModeYolo)
	m.addInfo("YOLO mode enabled: every tool call is allowed without prompting.")
}

func (m *Model) cmdCompact(instructions string) tea.Cmd {
	if m.running {
		m.addError("Cannot compact while a turn is running. Press Ctrl+C first.")
		return nil
	}
	if m.opts.Agent == nil {
		m.addError("No agent configured.")
		return nil
	}
	base := m.baseCtx
	if base == nil {
		base = context.Background()
	}
	m.addInfo("Compacting context…")
	go func() {
		ctx, cancel := context.WithTimeout(base, 2*time.Minute)
		defer cancel()
		result, err := m.opts.Agent.Compact(ctx, instructions)
		if err != nil {
			m.bus.Push(cmdResultMsg{text: "compaction failed", err: err})
			return
		}
		m.bus.Push(cmdResultMsg{
			text: fmt.Sprintf("Context compacted: %d → %d messages (%d summarized, %d kept).",
				result.MessagesBefore, result.MessagesAfter, result.MessagesSummarized, result.KeptTail),
		})
	}()
	return nil
}

func (m *Model) costText() string {
	if !m.running && m.opts.Agent != nil {
		if usage, cost := m.opts.Agent.TotalUsage(); usage.TotalTokens > 0 || cost > 0 {
			m.usage, m.totalCost = usage, cost
			m.cache = cacheRate(m.usage)
		}
	}
	cachePct := int(m.cache*100 + 0.5)
	var b strings.Builder
	fmt.Fprintf(&b, "Cost: $%.4f\n", m.totalCost)
	fmt.Fprintf(&b, "Tokens: %d prompt, %d completion, %d total\n",
		m.usage.PromptTokens, m.usage.CompletionTokens, m.usage.TotalTokens)
	fmt.Fprintf(&b, "Cache: %d hit / %d miss (%d%%)\n",
		m.usage.PromptCacheHitTokens, m.usage.PromptCacheMissTokens, cachePct)
	if m.opts.Config.MaxCostUSD > 0 {
		fmt.Fprintf(&b, "Budget: $%.4f of $%.4f\n", m.totalCost, m.opts.Config.MaxCostUSD)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *Model) contextText() string {
	tokens := m.ctxTokens
	messages := len(m.entries)
	if !m.running && m.opts.Agent != nil {
		history := m.opts.Agent.History()
		messages = len(history)
		if len(history) > 0 {
			if actual := m.opts.Agent.ContextTokens(); actual > 0 {
				tokens = actual
			}
		}
	}
	window := m.ctxWindow
	if window <= 0 {
		window = 1_000_000
	}
	pct := float64(tokens) / float64(window) * 100
	prefix := "~"
	if m.usage.PromptTokens > 0 && m.streamChars == 0 {
		prefix = ""
	}
	return fmt.Sprintf("Context: %s%d / %d tokens (%.1f%%) · %d active messages", prefix, tokens, window, pct, messages)
}

func (m *Model) cmdUndo() tea.Cmd {
	if m.opts.Checkpoints == nil {
		m.addError("Checkpoints are not enabled for this session.")
		return nil
	}
	result, err := m.opts.Checkpoints.Undo()
	if err != nil {
		m.addError("undo failed: " + err.Error())
		return nil
	}
	m.addInfo(formatRestore("Undid the latest turn", result))
	return nil
}

func (m *Model) cmdRewind(args string) tea.Cmd {
	if m.opts.Checkpoints == nil {
		m.addError("Checkpoints are not enabled for this session.")
		return nil
	}
	args = strings.TrimSpace(args)
	if args == "" {
		turns := m.opts.Checkpoints.List()
		if len(turns) == 0 {
			m.addInfo("No checkpointed turns.")
			return nil
		}
		var b strings.Builder
		b.WriteString("Checkpointed turns (newest last):\n")
		for _, turn := range turns {
			fmt.Fprintf(&b, "  %d  %s  %d file(s)  %s\n",
				turn.TurnID, turn.Timestamp.Local().Format("2006-01-02 15:04"), turn.FileCount, turn.Summary)
		}
		b.WriteString("\nUse /rewind <turn> to restore the state before that turn.")
		m.addInfo(b.String())
		return nil
	}
	turnID, err := strconv.Atoi(args)
	if err != nil {
		m.addError("rewind: turn must be an integer")
		return nil
	}
	result, err := m.opts.Checkpoints.RewindBefore(turnID)
	if err != nil {
		m.addError("rewind failed: " + err.Error())
		return nil
	}
	m.addInfo(formatRestore(fmt.Sprintf("Rewound to before turn %d", turnID), result))
	return nil
}

func formatRestore(prefix string, result *checkpoint.RestoreResult) string {
	if result == nil {
		return prefix + "."
	}
	return fmt.Sprintf("%s: %d restored, %d deleted (turns %v).",
		prefix, len(result.Restored), len(result.Deleted), result.TurnIDs)
}

func (m *Model) cmdSessions(query string) tea.Cmd {
	m.openSessionsModal(query)
	return nil
}

func (m *Model) cmdFork(args string) tea.Cmd {
	if m.opts.SessionStore == nil || m.opts.Session == nil {
		m.addError("Session storage is not configured.")
		return nil
	}
	forked, err := m.opts.SessionStore.Fork(m.opts.Session.ID(), strings.TrimSpace(args))
	if err != nil {
		m.addError("fork: " + err.Error())
		return nil
	}
	m.addInfo("Forked session " + forked.ID() + ". Start tern with --resume " + forked.ID() + " to continue it.")
	return nil
}

func (m *Model) cmdName(args string) tea.Cmd {
	name := strings.TrimSpace(args)
	if name == "" {
		m.addError("Usage: /name <name>")
		return nil
	}
	if m.opts.Session != nil {
		if err := m.opts.Session.Rename(name); err != nil {
			m.addError("rename failed: " + err.Error())
			return nil
		}
		m.addInfo("Session renamed to " + name + ".")
		return nil
	}
	if m.opts.SessionStore == nil {
		m.addError("Session storage is not configured.")
		return nil
	}
	m.addError("No active session to rename.")
	return nil
}

func (m *Model) cmdTools(args string) tea.Cmd {
	arg := strings.ToLower(strings.TrimSpace(args))
	switch arg {
	case "":
		switch m.tools {
		case toolsCompact:
			m.tools = toolsHidden
			m.showToolDetail = false
			m.statusMsg = "Tools hidden · /tools to show"
		case toolsHidden:
			m.tools = toolsExpanded
			m.showToolDetail = true
			m.statusMsg = "Tools expanded"
		default:
			m.tools = toolsCompact
			m.showToolDetail = false
			m.statusMsg = "Tools compact"
		}
	case "hide", "hidden", "off":
		m.tools = toolsHidden
		m.showToolDetail = false
		m.statusMsg = "Tools hidden · /tools to show"
	case "expand", "expanded", "full":
		m.tools = toolsExpanded
		m.showToolDetail = true
		m.statusMsg = "Tools expanded"
	case "compact", "show", "on":
		m.tools = toolsCompact
		m.showToolDetail = false
		m.statusMsg = "Tools compact"
	default:
		m.addError("Usage: /tools [compact|expanded|hidden]")
		return nil
	}
	m.dirty = true
	return nil
}

func (m *Model) cmdVersion() tea.Cmd {
	ver := m.versionLabel()
	if ver == "" {
		ver = "dev"
	}
	if m.updateAvailable != "" {
		m.addInfo(fmt.Sprintf("tern %s · update available: %s (run \"tern update\")", ver, m.updateAvailable))
		return nil
	}
	if m.updateChecked {
		m.addInfo(fmt.Sprintf("tern %s · up to date", ver))
		return nil
	}
	if m.opts.CheckUpdate == nil {
		m.addInfo("tern " + ver)
		return nil
	}
	m.addInfo("tern " + ver + " · checking for updates…")
	check := m.opts.CheckUpdate
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		latest, available, err := check(ctx)
		if err != nil {
			return nil
		}
		return updateCheckMsg{latest: latest, available: available, announce: true}
	}
}

func (m *Model) cmdExport(args string) tea.Cmd {
	if m.opts.Session == nil {
		m.addError("No active session to export.")
		return nil
	}
	format := "md"
	rest := strings.TrimSpace(args)
	fields := strings.Fields(rest)
	if len(fields) > 0 {
		candidate := strings.ToLower(fields[0])
		if candidate == "md" || candidate == "markdown" {
			format = "md"
			rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
		} else if candidate == "html" || candidate == "htm" {
			format = "html"
			rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
		}
	}
	path := strings.TrimSpace(strings.Trim(rest, `"'`))
	if path == "" {
		ext := ".md"
		if format == "html" {
			ext = ".html"
		}
		path = filepath.Join(m.opts.WorkDir, "tern-"+m.opts.Session.ID()+ext)
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(m.opts.WorkDir, path)
	}
	file, err := os.Create(path)
	if err != nil {
		m.addError("export: " + err.Error())
		return nil
	}
	if format == "html" {
		err = m.opts.Session.ExportHTML(file)
	} else {
		err = m.opts.Session.ExportMarkdown(file)
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		m.addError("export: " + err.Error())
		return nil
	}
	m.addInfo("Exported session to " + path)
	return nil
}

func (m *Model) cmdSkills(args string) tea.Cmd {
	name := strings.TrimSpace(args)
	if name == "" {
		m.openSkillsModal()
		return nil
	}
	skill, err := terncontext.LoadSkill(m.opts.WorkDir, m.opts.GlobalConfigDir, name)
	if err != nil {
		m.addError("skills: " + err.Error())
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", skill.Name)
	if skill.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", skill.Description)
	}
	b.WriteString(skill.Body)
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

func (m *Model) mcpText() string {
	if m.opts.Extensions == nil {
		return "No MCP servers configured."
	}
	status := m.opts.Extensions.Status()
	if len(status) == 0 {
		return "No MCP servers configured."
	}
	var b strings.Builder
	b.WriteString("MCP servers:\n")
	for _, server := range status {
		state := "disconnected"
		if server.Connected {
			state = "connected"
		}
		fmt.Fprintf(&b, "  %-20s %-10s %-13s %d tool(s)\n", server.Name, server.Transport, state, server.ToolCount)
		if server.Error != "" {
			fmt.Fprintf(&b, "    error: %s\n", server.Error)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
