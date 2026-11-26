package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	tern "github.com/tern-agent/tern"
	"github.com/tern-agent/tern/internal/config"
	terncontext "github.com/tern-agent/tern/internal/context"
	"github.com/tern-agent/tern/internal/ext"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/session"
)

// modalKind identifies the interactive overlay currently shown above the
// editor. Exactly one modal can be active at a time; Esc always closes it.
type modalKind int

const (
	modalNone modalKind = iota
	modalModel
	modalSessions
	modalStats
	modalSkills
	modalMCP
	modalExt
	modalHelp
	modalChangelog
	modalAgentPeek
	modalProvider
)

var reasoningEfforts = []string{"none", "low", "high", "max"}

type modelChoice struct {
	name       string
	pricing    config.ModelPricing
	hasPricing bool
	isFree     bool
	promptPerM float64
	outputPerM float64
	remote     bool
}

var modelSortOrders = []string{"free", "price", "context", "name"}

type modalSession struct {
	ID        string
	Name      string
	Model     string
	Messages  int
	Turns     int
	Usage     provider.Usage
	CostUSD   float64
	Duration  time.Duration
	UpdatedAt time.Time
}

// helpItem is one selectable row in the interactive /help modal. Commands
// whose usage takes an argument pre-fill the editor instead of executing.
type helpItem struct {
	name      string
	usage     string
	detail    string
	takesArgs bool
}

type modalState struct {
	kind modalKind

	index  int
	filter string
	// filterActive keeps shortcut letters (n/f/r/d) as text once the user has
	// started typing a filter. A leading "/" explicitly enters filter mode.
	filterActive bool

	rename      bool
	renameText  string
	renameFresh bool

	confirmDelete bool
	notice        string

	allModels         []modelChoice
	models            []modelChoice
	effort            string
	freeOnly          bool
	modelSort         string
	modelsLoading     bool
	modelsProvider    string
	modelsCacheKey    string
	fromProviderSetup bool

	sessions []modalSession

	stats *session.AggregateStats

	skills       []terncontext.SkillMeta
	skillPreview string

	mcp []ext.MCPStatus

	extensions []*ext.Extension

	helpItems []helpItem

	changelogScroll int

	// Agent peek modal state. peekAgentID names the sub-agent whose child
	// transcript is shown, peekScroll is the first visible body line, and
	// peekReasoning toggles the muted reasoning rail.
	peekAgentID   string
	peekScroll    int
	peekReasoning bool

	providerSetup providerSetupState
}

func (m *Model) setModal(kind modalKind) {
	m.activeModal = kind
	m.modal = modalState{kind: kind}
	m.dirty = true
}

func (m *Model) closeModal() {
	m.activeModal = modalNone
	m.modal = modalState{}
	m.statusMsg = "Ready"
	m.dirty = true
}

func (m *Model) handleModalKey(msg tea.KeyMsg) tea.Cmd {
	switch m.activeModal {
	case modalModel:
		return m.handleModelModalKey(msg)
	case modalSessions:
		return m.handleSessionsModalKey(msg)
	case modalStats:
		return m.handleStatsModalKey(msg)
	case modalSkills:
		return m.handleSkillsModalKey(msg)
	case modalMCP:
		return m.handleMCPModalKey(msg)
	case modalExt:
		return m.handleExtModalKey(msg)
	case modalHelp:
		return m.handleHelpModalKey(msg)
	case modalChangelog:
		return m.handleChangelogModalKey(msg)
	case modalAgentPeek:
		return m.handleAgentPeekKey(msg)
	case modalProvider:
		return m.handleProviderModalKey(msg)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Model selector
// ---------------------------------------------------------------------------

func (m *Model) openModelModal() tea.Cmd {
	return m.openModelModalWithFilter(false)
}

func (m *Model) openModelModalWithFilter(freeOnly bool) tea.Cmd {
	m.setModal(modalModel)
	cfg := m.opts.Config
	current := ""
	effort := ""
	provName := "deepseek"
	baseURL := ""
	if cfg != nil {
		current = cfg.Model
		effort = cfg.ReasoningEffort
		if strings.TrimSpace(cfg.Provider) != "" {
			provName = strings.ToLower(strings.TrimSpace(cfg.Provider))
		}
		baseURL = strings.TrimSpace(cfg.BaseURL)
	}
	cacheKey := provName + "|" + baseURL
	m.modal.modelsProvider = provName
	m.modal.modelsCacheKey = cacheKey
	m.modal.freeOnly = freeOnly
	m.modal.effort = effort
	if !containsString(reasoningEfforts, effort) {
		m.modal.effort = "high"
	}

	isDeepSeek := provName == "" || provName == "deepseek"
	if isDeepSeek {
		m.modal.modelSort = "name"
		names := make([]string, 0, len(cfg.Pricing))
		if cfg != nil {
			for name := range cfg.Pricing {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			m.modal.allModels = append(m.modal.allModels, modelChoice{
				name:    name,
				pricing: cfg.Pricing[name],
			})
		}
		m.rebuildModelChoices(current)
		if len(m.modal.models) == 0 {
			m.modal.notice = "No models are configured in pricing."
		}
		m.statusMsg = "Model selector"
		return nil
	}

	// Non-DeepSeek provider: default OpenRouter to free-first sorting so free
	// models are immediately visible at the top.
	if provName == "openrouter" {
		m.modal.modelSort = "free"
	} else {
		m.modal.modelSort = "name"
	}

	// Use cached live models if available for this provider/baseURL.
	if m.cachedModelsKey == cacheKey && len(m.cachedModels) > 0 {
		m.modal.allModels = remoteModelsToChoices(m.cachedModels, current)
		m.rebuildModelChoices(current)
		m.statusMsg = fmt.Sprintf("Model selector · %s", provName)
		return nil
	}

	// Seed immediately with fallback models + current model, then fetch live
	// models asynchronously from the provider's /models endpoint.
	fallback := provider.FallbackModelsForProvider(provName)
	m.modal.allModels = remoteModelsToChoices(fallback, current)
	m.rebuildModelChoices(current)
	m.statusMsg = fmt.Sprintf("Model selector · %s", provName)

	cmd := m.fetchModelsCmd(cacheKey)
	if cmd != nil {
		m.modal.modelsLoading = true
		m.modal.notice = fmt.Sprintf("Fetching available models from %s…", provName)
	}
	return cmd
}

func remoteModelsToChoices(remote []provider.RemoteModel, current string) []modelChoice {
	choices := make([]modelChoice, 0, len(remote)+1)
	seen := make(map[string]struct{}, len(remote)+1)
	for _, rm := range remote {
		id := strings.TrimSpace(rm.ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		choices = append(choices, modelChoice{
			name: id,
			pricing: config.ModelPricing{
				ContextWindow:  rm.ContextLength,
				SupportsVision: rm.SupportsVision,
			},
			hasPricing: rm.HasPricing,
			isFree:     rm.IsFree,
			promptPerM: rm.PromptPerM,
			outputPerM: rm.OutputPerM,
			remote:     true,
		})
	}
	if cur := strings.TrimSpace(current); cur != "" {
		if _, ok := seen[cur]; !ok {
			caps := provider.DefaultModelCapabilities(cur)
			choices = append(choices, modelChoice{
				name: cur,
				pricing: config.ModelPricing{
					ContextWindow:  caps.ContextWindow,
					SupportsVision: caps.SupportsVision,
				},
				isFree: strings.HasSuffix(strings.ToLower(cur), ":free"),
				remote: true,
			})
		}
	}
	return choices
}

func (m *Model) fetchModelsCmd(cacheKey string) tea.Cmd {
	if m.opts.Agent == nil || m.opts.Agent.Provider == nil {
		return nil
	}
	lister, ok := m.opts.Agent.Provider.(provider.ModelLister)
	if !ok {
		return nil
	}
	baseCtx := m.baseCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(baseCtx, 12*time.Second)
		defer cancel()
		models, err := lister.ListModels(ctx)
		return modelsLoadedMsg{
			cacheKey: cacheKey,
			models:   models,
			err:      err,
		}
	}
}

func (m *Model) handleModelsLoaded(msg modelsLoadedMsg) {
	if msg.err == nil && len(msg.models) > 0 {
		m.cachedModelsKey = msg.cacheKey
		m.cachedModels = msg.models
	}
	if m.activeModal != modalModel || m.modal.modelsCacheKey != msg.cacheKey {
		return
	}
	m.modal.modelsLoading = false
	if msg.err != nil {
		m.modal.notice = fmt.Sprintf("Could not fetch live models (%v); showing defaults.", msg.err)
		m.dirty = true
		return
	}
	if len(msg.models) == 0 {
		m.modal.notice = "Provider returned no models; showing defaults."
		m.dirty = true
		return
	}
	keepName := ""
	if m.modal.index >= 0 && m.modal.index < len(m.modal.models) {
		keepName = m.modal.models[m.modal.index].name
	} else if m.opts.Config != nil {
		keepName = m.opts.Config.Model
	}
	current := ""
	if m.opts.Config != nil {
		current = m.opts.Config.Model
	}
	m.modal.allModels = remoteModelsToChoices(msg.models, current)
	m.modal.notice = ""
	m.rebuildModelChoices(keepName)
	m.dirty = true
}

func (m *Model) rebuildModelChoices(preferName string) {
	query := strings.ToLower(strings.TrimSpace(m.modal.filter))
	filtered := make([]modelChoice, 0, len(m.modal.allModels))
	for _, mc := range m.modal.allModels {
		if m.modal.freeOnly && !mc.isFree {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(mc.name), query) {
			continue
		}
		filtered = append(filtered, mc)
	}

	sort.SliceStable(filtered, func(i, j int) bool {
		a, b := filtered[i], filtered[j]
		switch m.modal.modelSort {
		case "free":
			if a.isFree != b.isFree {
				return a.isFree
			}
			return strings.ToLower(a.name) < strings.ToLower(b.name)
		case "price":
			if a.isFree != b.isFree {
				return a.isFree
			}
			if a.hasPricing != b.hasPricing {
				return a.hasPricing
			}
			aCost := a.promptPerM + a.outputPerM
			bCost := b.promptPerM + b.outputPerM
			if aCost != bCost {
				return aCost < bCost
			}
			return strings.ToLower(a.name) < strings.ToLower(b.name)
		case "context":
			if a.pricing.ContextWindow != b.pricing.ContextWindow {
				return a.pricing.ContextWindow > b.pricing.ContextWindow
			}
			return strings.ToLower(a.name) < strings.ToLower(b.name)
		default:
			return strings.ToLower(a.name) < strings.ToLower(b.name)
		}
	})

	m.modal.models = filtered
	m.modal.index = 0
	if preferName != "" {
		for i, mc := range m.modal.models {
			if mc.name == preferName {
				m.modal.index = i
				break
			}
		}
	}
}

func (m *Model) countFreeModels() int {
	n := 0
	for _, mc := range m.modal.allModels {
		if mc.isFree {
			n++
		}
	}
	return n
}

func (m *Model) cycleModelSort() {
	cur := 0
	for i, order := range modelSortOrders {
		if order == m.modal.modelSort {
			cur = i
			break
		}
	}
	m.modal.modelSort = modelSortOrders[(cur+1)%len(modelSortOrders)]
	keep := ""
	if m.modal.index >= 0 && m.modal.index < len(m.modal.models) {
		keep = m.modal.models[m.modal.index].name
	}
	m.rebuildModelChoices(keep)
	m.dirty = true
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func (m *Model) handleModelModalKey(msg tea.KeyMsg) tea.Cmd {
	if m.modal.filterActive {
		switch msg.Type {
		case tea.KeyEsc:
			m.modal.filterActive = false
			if m.modal.filter != "" {
				m.modal.filter = ""
				m.rebuildModelChoices("")
			}
			m.dirty = true
			return nil
		case tea.KeyEnter:
			m.modal.filterActive = false
			m.applyModelModal()
			return nil
		case tea.KeyUp:
			m.modal.index--
			m.clampModelIndex()
			return nil
		case tea.KeyDown:
			m.modal.index++
			m.clampModelIndex()
			return nil
		case tea.KeyBackspace:
			if len(m.modal.filter) > 0 {
				r := []rune(m.modal.filter)
				m.modal.filter = string(r[:len(r)-1])
				m.rebuildModelChoices("")
				m.dirty = true
			}
			return nil
		case tea.KeyCtrlU:
			m.modal.filter = ""
			m.rebuildModelChoices("")
			m.dirty = true
			return nil
		case tea.KeyRunes:
			m.modal.filter += string(msg.Runes)
			m.rebuildModelChoices("")
			m.dirty = true
			return nil
		case tea.KeySpace:
			m.modal.filter += " "
			m.rebuildModelChoices("")
			m.dirty = true
			return nil
		}
		return nil
	}

	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlP:
		if m.modal.filter != "" {
			m.modal.filter = ""
			m.rebuildModelChoices("")
			m.dirty = true
			return nil
		}
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.modal.index--
		m.clampModelIndex()
		return nil
	case tea.KeyDown:
		m.modal.index++
		m.clampModelIndex()
		return nil
	case tea.KeyPgUp:
		m.modal.index -= m.modalRowLimit()
		if m.modal.index < 0 {
			m.modal.index = 0
		}
		m.dirty = true
		return nil
	case tea.KeyPgDown:
		m.modal.index += m.modalRowLimit()
		if n := len(m.modal.models); n > 0 && m.modal.index >= n {
			m.modal.index = n - 1
		}
		m.dirty = true
		return nil
	case tea.KeyLeft:
		m.cycleEffort(-1)
		return nil
	case tea.KeyRight, tea.KeyTab:
		m.cycleEffort(1)
		return nil
	case tea.KeyShiftTab:
		m.cycleEffort(-1)
		return nil
	case tea.KeyEnter:
		m.applyModelModal()
		return nil
	case tea.KeyRunes:
		if len(msg.Runes) == 1 {
			r := msg.Runes[0]
			switch {
			case r >= '1' && r <= '4':
				if n := int(r - '1'); n >= 0 && n < len(reasoningEfforts) {
					m.modal.effort = reasoningEfforts[n]
					m.dirty = true
					return nil
				}
			case r == 'f' || r == 'F':
				m.modal.freeOnly = !m.modal.freeOnly
				keep := ""
				if m.modal.index >= 0 && m.modal.index < len(m.modal.models) {
					keep = m.modal.models[m.modal.index].name
				}
				m.rebuildModelChoices(keep)
				m.dirty = true
				return nil
			case r == 's' || r == 'S':
				m.cycleModelSort()
				return nil
			case r == '/':
				m.modal.filterActive = true
				m.dirty = true
				return nil
			case r == 'p' || r == 'P':
				m.openProviderModal(false)
				return nil
			case r == 'r' || r == 'R':
				if m.modal.modelsProvider != "" && m.modal.modelsProvider != "deepseek" {
					m.cachedModelsKey = ""
					m.cachedModels = nil
					if cmd := m.fetchModelsCmd(m.modal.modelsCacheKey); cmd != nil {
						m.modal.modelsLoading = true
						m.modal.notice = fmt.Sprintf("Refreshing models from %s…", m.modal.modelsProvider)
						m.dirty = true
						return cmd
					}
				}
			}
		}
	}
	return nil
}

func (m *Model) clampModelIndex() {
	n := len(m.modal.models)
	if n == 0 {
		m.modal.index = 0
		return
	}
	if m.modal.index < 0 {
		m.modal.index = n - 1
	}
	if m.modal.index >= n {
		m.modal.index = 0
	}
}

func (m *Model) cycleEffort(delta int) {
	index := 0
	for i, effort := range reasoningEfforts {
		if effort == m.modal.effort {
			index = i
			break
		}
	}
	index = (index + delta + len(reasoningEfforts)) % len(reasoningEfforts)
	m.modal.effort = reasoningEfforts[index]
	m.dirty = true
}

// cycleReasoningEffort changes the live effort level from the main view
// (Shift+Tab) and reports it briefly in the status line.
func (m *Model) cycleReasoningEffort(delta int) {
	current := ""
	if m.opts.Config != nil {
		current = strings.TrimSpace(m.opts.Config.ReasoningEffort)
	}
	index := 2 // default to high when unset or unknown
	if !containsString(reasoningEfforts, current) {
		index = 2
	}
	for i, effort := range reasoningEfforts {
		if effort == current {
			index = i
			break
		}
	}
	index = (index + delta + len(reasoningEfforts)) % len(reasoningEfforts)
	level := reasoningEfforts[index]
	m.applyReasoningEffort(level)
	m.statusMsg = "Reasoning effort: " + level
}

func (m *Model) applyModelModal() {
	effort := m.modal.effort
	fromSetup := m.modal.fromProviderSetup
	if len(m.modal.models) > 0 {
		selected := m.modal.models[m.modal.index]
		m.applyModel(selected.name)
		if fromSetup && m.opts.Config != nil && m.opts.GlobalConfigDir != "" {
			_ = config.SaveGlobalProviderSelection(m.opts.GlobalConfigDir, m.opts.Config.Provider, m.opts.Config.BaseURL, selected.name)
		}
		m.closeModal()
		m.applyReasoningEffort(effort)
		m.addInfo(fmt.Sprintf("Model set to %s (%s).", selected.name, effort))
		return
	}
	m.closeModal()
	m.applyReasoningEffort(effort)
	m.addInfo("Reasoning effort set to " + effort + ".")
}

func (m *Model) applyModel(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	if m.opts.Config != nil {
		m.opts.Config.Model = name
	}
	if m.opts.Agent != nil {
		if m.opts.Agent.Config == nil {
			m.opts.Agent.Config = m.opts.Config
		}
		m.opts.Agent.Config.Model = name
		m.opts.Agent.SetModel(name)
		if m.opts.Agent.Provider != nil {
			if caps := m.opts.Agent.Provider.ModelInfo(name); caps.ContextWindow > 0 {
				m.ctxWindow = caps.ContextWindow
			}
		}
	}
	if m.opts.Config != nil {
		if pricing, ok := m.opts.Config.Pricing[name]; ok && pricing.ContextWindow > 0 {
			m.ctxWindow = pricing.ContextWindow
		}
	}
	m.dirty = true
}

func (m *Model) applyReasoningEffort(effort string) {
	effort = strings.TrimSpace(effort)
	if m.opts.Config != nil {
		m.opts.Config.ReasoningEffort = effort
	}
	if m.opts.Agent != nil {
		if m.opts.Agent.Config == nil {
			m.opts.Agent.Config = m.opts.Config
		}
		m.opts.Agent.Config.ReasoningEffort = effort
		m.opts.Agent.SetReasoningEffort(effort)
	}
	m.dirty = true
}
func (m *Model) modelModalView() string {
	m.modalRows = nil
	var b strings.Builder
	current := ""
	if m.opts.Config != nil {
		current = m.opts.Config.Model
	}
	provName := m.modal.modelsProvider
	if provName == "" {
		provName = "deepseek"
	}
	isRemote := provName != "deepseek"
	title := "Model"
	if m.modal.fromProviderSetup {
		title = "Choose Model · " + provName
	} else if isRemote {
		title = "Model · " + provName
	}

	if m.modal.fromProviderSetup {
		b.WriteString(m.renderSetupStepper(3, provName) + "\n\n")
	}

	fmt.Fprintf(&b, "Current model: %s", current)
	if isRemote {
		freeCount := m.countFreeModels()
		if freeCount > 0 {
			fmt.Fprintf(&b, "  %s", m.st.paint(m.st.dim, fmt.Sprintf("(%d free · %d total)", freeCount, len(m.modal.allModels))))
		} else if len(m.modal.allModels) > 0 {
			fmt.Fprintf(&b, "  %s", m.st.paint(m.st.dim, fmt.Sprintf("(%d models)", len(m.modal.allModels))))
		}
	}
	b.WriteString("\n")

	if isRemote || m.modal.freeOnly || m.modal.filterActive || m.modal.filter != "" {
		freeChip := m.st.paint(m.st.dim, "[ All ]")
		if m.modal.freeOnly {
			freeChip = m.st.paint(m.st.success, "[ ● Free only ]")
		}
		sortLabel := m.modal.modelSort
		if sortLabel == "free" {
			sortLabel = "free-first"
		}
		filterLabel := ""
		if m.modal.filterActive {
			filterLabel = "  " + m.st.paint(m.st.accent, "/"+m.modal.filter+"▏")
		} else if m.modal.filter != "" {
			filterLabel = "  " + m.st.paint(m.st.dim, "filter: "+m.modal.filter)
		}
		b.WriteString(freeChip + "  " + m.st.paint(m.st.dim, "sort: "+sortLabel) + filterLabel + "\n")
	}

	if len(m.modal.models) == 0 {
		emptyMsg := "No models configured in pricing."
		if m.modal.freeOnly {
			emptyMsg = "No free models match. Press f to show all models."
		} else if m.modal.filter != "" {
			emptyMsg = "No models match filter \"" + m.modal.filter + "\"."
		}
		b.WriteString(m.st.paint(m.st.dim, emptyMsg) + "\n")
	} else {
		limit := m.modalRowLimit()
		start, end := windowRange(len(m.modal.models), m.modal.index, limit)
		if start > 0 {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d above\n", start)))
		}
		maxW := maxInt(30, m.modalTextWidth())
		for i := start; i < end; i++ {
			m.addModalRow(strings.Count(b.String(), "\n"), i)
			model := m.modal.models[i]
			marker := "  "
			style := m.st.dim
			if i == m.modal.index {
				marker = "▸ "
				style = m.st.selected
				if m.st.plain {
					marker = "> "
				}
			}
			active := " "
			if model.name == current {
				active = "*"
			}
			tag := "text"
			if model.pricing.SupportsVision {
				tag = "vision"
			}
			ctx := compactContext(model.pricing.ContextWindow)
			if model.remote {
				nameWidth := minInt(44, maxInt(22, maxW-34))
				priceStr := ""
				if model.isFree {
					priceStr = "[FREE]"
				} else if model.hasPricing {
					priceStr = fmt.Sprintf("$%.2f/$%.2f", model.promptPerM, model.outputPerM)
				}
				base := fmt.Sprintf("%s%s %-*s %-5s %-6s",
					marker, active, nameWidth, truncateCell(model.name, nameWidth), ctx, tag)
				row := m.st.paint(style, base)
				if model.isFree {
					row += "  " + m.st.paint(m.st.success, priceStr)
				} else if priceStr != "" {
					row += "  " + m.st.paint(m.st.dim, priceStr)
				}
				b.WriteString(row)
			} else {
				line := fmt.Sprintf("%s%s %-18s %-5s %-6s  off %.2f/%.2f/%.2f  peak %.2f/%.2f/%.2f",
					marker, active, truncateCell(model.name, 18), ctx, tag,
					model.pricing.OffPeakCacheHitPerM, model.pricing.OffPeakCacheMissPerM, model.pricing.OffPeakOutputPerM,
					model.pricing.PeakCacheHitPerM, model.pricing.PeakCacheMissPerM, model.pricing.PeakOutputPerM,
				)
				b.WriteString(m.st.paint(style, truncateCell(line, maxW)))
			}
			b.WriteString("\n")
		}
		if end < len(m.modal.models) {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d below\n", len(m.modal.models)-end)))
		}
	}
	b.WriteString("\n")
	b.WriteString(m.st.paint(m.st.accent, "Reasoning: "))
	for i, effort := range reasoningEfforts {
		if i > 0 {
			b.WriteString(" ")
		}
		if effort == m.modal.effort {
			b.WriteString(m.st.paint(m.st.selected, "[ ● "+effort+" ]"))
		} else {
			b.WriteString(m.st.paint(m.st.dim, "[ "+effort+" ]"))
		}
	}
	b.WriteString(m.st.paint(m.st.dim, "   ←/→ · tab · 1-4 pick   enter save"))
	if m.modal.notice != "" {
		b.WriteString("\n" + m.st.paint(m.st.amber, m.modal.notice))
	}
	footer := "↑/↓ select · p provider · ←/→ effort · 1-4 pick · enter apply · esc close"
	if isRemote {
		footer = "↑/↓ select · f free only · s sort · / search · p provider · r refresh · enter apply · esc close"
	}
	return m.modalFrame(title, strings.TrimRight(b.String(), "\n"), footer)
}

func compactContext(window int) string {
	if window >= 1_000_000 {
		return fmt.Sprintf("%dM", window/1_000_000)
	}
	if window >= 1_000 {
		return fmt.Sprintf("%dk", window/1_000)
	}
	return fmt.Sprintf("%d", window)
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

func (m *Model) openSessionsModal(filter string) {
	if m.opts.SessionStore == nil {
		m.addError("Session storage is not configured.")
		return
	}
	m.setModal(modalSessions)
	m.modal.filter = strings.TrimSpace(filter)
	m.modal.filterActive = m.modal.filter != ""
	m.refreshSessions()
	m.statusMsg = "Sessions"
}

func (m *Model) refreshSessions() {
	m.modal.sessions = nil
	store := m.opts.SessionStore
	if store == nil {
		return
	}
	stats, err := store.Stats()
	if err == nil && stats != nil {
		for _, row := range stats.Sessions {
			m.modal.sessions = append(m.modal.sessions, modalSession{
				ID: row.ID, Name: row.Name, Model: row.Model,
				Messages: row.Messages, Turns: row.Turns,
				Usage: row.Usage, CostUSD: row.CostUSD,
				Duration: row.Duration, UpdatedAt: row.UpdatedAt,
			})
		}
	}
	if len(m.modal.sessions) == 0 {
		if list, listErr := store.List(""); listErr == nil {
			for _, item := range list {
				m.modal.sessions = append(m.modal.sessions, modalSession{
					ID: item.ID, Name: item.Name, Model: item.Model,
					Messages: item.MessageCount, Usage: item.Usage,
					CostUSD: item.CostUSD, UpdatedAt: item.UpdatedAt,
				})
			}
		} else if err == nil {
			m.modal.notice = "sessions: " + listErr.Error()
		}
	}
	sort.SliceStable(m.modal.sessions, func(i, j int) bool {
		return m.modal.sessions[i].UpdatedAt.After(m.modal.sessions[j].UpdatedAt)
	})
	m.clampSessionIndex()
}

func (m *Model) filteredSessions() []modalSession {
	query := strings.ToLower(strings.TrimSpace(m.modal.filter))
	if query == "" {
		return m.modal.sessions
	}
	out := make([]modalSession, 0, len(m.modal.sessions))
	for _, item := range m.modal.sessions {
		haystack := strings.ToLower(item.ID + " " + item.Name + " " + item.Model)
		if strings.Contains(haystack, query) {
			out = append(out, item)
		}
	}
	return out
}

func (m *Model) clampSessionIndex() {
	n := len(m.filteredSessions())
	if n == 0 {
		m.modal.index = 0
		return
	}
	if m.modal.index < 0 {
		m.modal.index = n - 1
	}
	if m.modal.index >= n {
		m.modal.index = 0
	}
}

func (m *Model) selectedSession() *modalSession {
	items := m.filteredSessions()
	if len(items) == 0 || m.modal.index < 0 || m.modal.index >= len(items) {
		return nil
	}
	return &items[m.modal.index]
}

func (m *Model) handleSessionsModalKey(msg tea.KeyMsg) tea.Cmd {
	if m.modal.rename {
		return m.handleSessionRenameKey(msg)
	}
	if m.modal.confirmDelete {
		switch msg.Type {
		case tea.KeyRunes:
			switch strings.ToLower(string(msg.Runes)) {
			case "y":
				m.deleteSelectedSession()
				return nil
			case "n":
				m.modal.confirmDelete = false
				m.modal.notice = ""
				return nil
			}
			return nil
		case tea.KeySpace:
			m.modal.confirmDelete = false
			m.modal.notice = ""
			return nil
		case tea.KeyEsc:
			m.modal.confirmDelete = false
			m.modal.notice = ""
			return nil
		case tea.KeyEnter:
			m.deleteSelectedSession()
			return nil
		}
		return nil
	}

	switch msg.Type {
	case tea.KeyEsc:
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.modal.index--
		m.clampSessionIndex()
		return nil
	case tea.KeyDown:
		m.modal.index++
		m.clampSessionIndex()
		return nil
	case tea.KeyEnter:
		m.resumeSelectedSession()
		return nil
	case tea.KeyBackspace:
		m.backspaceSessionFilter()
		return nil
	case tea.KeySpace:
		m.modal.filter += " "
		m.modal.filterActive = true
		m.modal.index = 0
		m.clampSessionIndex()
		return nil
	case tea.KeyRunes:
		text := string(msg.Runes)
		if text == "" {
			return nil
		}
		if !m.modal.filterActive && m.modal.filter == "" {
			switch text {
			case "n":
				m.createSessionFromModal()
				return nil
			case "f":
				m.forkSelectedSession()
				return nil
			case "r":
				m.startSessionRename()
				return nil
			case "d":
				m.armSessionDelete()
				return nil
			case "/":
				m.modal.filterActive = true
				m.modal.notice = "filter: type to narrow sessions"
				return nil
			}
		}
		m.modal.filter += text
		m.modal.filterActive = true
		m.modal.index = 0
		m.clampSessionIndex()
		return nil
	}
	return nil
}

func (m *Model) backspaceSessionFilter() {
	runes := []rune(m.modal.filter)
	if len(runes) == 0 {
		m.modal.filterActive = false
		return
	}
	m.modal.filter = string(runes[:len(runes)-1])
	if m.modal.filter == "" {
		m.modal.filterActive = false
	}
	m.modal.index = 0
	m.clampSessionIndex()
}

func (m *Model) handleSessionRenameKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.modal.rename = false
		m.modal.renameText = ""
		m.modal.renameFresh = false
		m.modal.notice = ""
		return nil
	case tea.KeyEnter:
		name := strings.TrimSpace(m.modal.renameText)
		selected := m.selectedSession()
		if selected == nil {
			m.modal.rename = false
			return nil
		}
		if name == "" {
			m.modal.notice = "Session name cannot be empty."
			return nil
		}
		if err := m.opts.SessionStore.Rename(selected.ID, name); err != nil {
			m.modal.notice = "rename failed: " + err.Error()
			return nil
		}
		if m.opts.Session != nil && m.opts.Session.ID() == selected.ID {
			_ = m.opts.Session.Rename(name)
		}
		m.modal.rename = false
		m.modal.renameText = ""
		m.modal.renameFresh = false
		m.modal.notice = "Renamed session to " + name + "."
		m.refreshSessions()
		return nil
	case tea.KeyBackspace:
		if m.modal.renameFresh {
			m.modal.renameText = ""
			m.modal.renameFresh = false
			return nil
		}
		runes := []rune(m.modal.renameText)
		if len(runes) > 0 {
			m.modal.renameText = string(runes[:len(runes)-1])
		}
		return nil
	case tea.KeySpace:
		if m.modal.renameFresh {
			m.modal.renameText = ""
			m.modal.renameFresh = false
		}
		m.modal.renameText += " "
		return nil
	case tea.KeyRunes:
		if m.modal.renameFresh {
			m.modal.renameText = ""
			m.modal.renameFresh = false
		}
		m.modal.renameText += string(msg.Runes)
		return nil
	}
	return nil
}

func (m *Model) startSessionRename() {
	selected := m.selectedSession()
	if selected == nil {
		return
	}
	name := selected.Name
	if strings.TrimSpace(name) == "" {
		name = selected.ID
	}
	m.modal.rename = true
	m.modal.renameText = name
	m.modal.renameFresh = true
	m.modal.notice = ""
}

func (m *Model) armSessionDelete() {
	selected := m.selectedSession()
	if selected == nil {
		return
	}
	if m.opts.Session != nil && selected.ID == m.opts.Session.ID() {
		m.modal.notice = "Cannot delete the active session. Switch to another session first."
		return
	}
	m.modal.confirmDelete = true
	m.modal.notice = "Delete session " + truncateCell(selected.displayName(), 32) + "? y/n"
}

func (m *Model) deleteSelectedSession() {
	selected := m.selectedSession()
	m.modal.confirmDelete = false
	if selected == nil || m.opts.SessionStore == nil {
		return
	}
	if m.opts.Session != nil && selected.ID == m.opts.Session.ID() {
		m.modal.notice = "Cannot delete the active session. Switch to another session first."
		return
	}
	if err := m.opts.SessionStore.Delete(selected.ID); err != nil {
		m.modal.notice = "delete failed: " + err.Error()
		return
	}
	m.modal.notice = "Deleted session " + selected.ID + "."
	m.refreshSessions()
}

func (m *Model) createSessionFromModal() {
	if m.opts.SessionStore == nil {
		return
	}
	meta := session.SessionMeta{WorkDir: m.opts.WorkDir}
	if m.opts.Config != nil {
		meta.Model = m.opts.Config.Model
	}
	sess, err := m.opts.SessionStore.Create(meta)
	if err != nil {
		m.modal.notice = "create failed: " + err.Error()
		return
	}
	m.activateSession(sess, "Started new session "+sess.ID()+".")
}

func (m *Model) forkSelectedSession() {
	selected := m.selectedSession()
	if selected == nil || m.opts.SessionStore == nil {
		return
	}
	forked, err := m.opts.SessionStore.Fork(selected.ID, "")
	if err != nil {
		m.modal.notice = "fork failed: " + err.Error()
		return
	}
	m.activateSession(forked, "Forked session "+selected.ID+" → "+forked.ID()+".")
}

func (m *Model) resumeSelectedSession() {
	selected := m.selectedSession()
	if selected == nil || m.opts.SessionStore == nil {
		return
	}
	sess, err := m.opts.SessionStore.Open(selected.ID)
	if err != nil {
		m.modal.notice = "open failed: " + err.Error()
		return
	}
	m.activateSession(sess, "Resumed session "+sess.ID()+".")
}

func (m *Model) activateSession(sess *session.Session, message string) {
	if sess == nil {
		return
	}
	if m.running {
		m.modal.notice = "Cannot switch sessions while a turn is running."
		return
	}
	m.opts.Session = sess
	if m.opts.Agent != nil {
		m.opts.Agent.SwitchSession(sess)
		m.usage, m.totalCost = m.opts.Agent.TotalUsage()
	} else {
		m.usage, m.totalCost = sess.TotalUsage()
	}
	m.cache = cacheRate(m.usage)
	m.rebuildTranscriptFromSession(sess)
	m.closeModal()
	m.statusMsg = message
}

func (m *Model) rebuildTranscriptFromSession(sess *session.Session) {
	msgs := sess.ReplayMessages()
	m.entries = nil
	m.curAssistant = nil
	m.curReasoning = nil
	m.pendingText = ""
	m.pendingReasoning = ""
	m.toolEntries = make(map[string]*transcriptEntry)
	for _, msg := range msgs {
		switch msg.Role {
		case provider.RoleUser:
			text := messageText(msg)
			if strings.TrimSpace(text) != "" {
				m.entries = append(m.entries, &transcriptEntry{kind: entryUser, text: text})
			}
		case provider.RoleAssistant:
			if strings.TrimSpace(msg.ReasoningContent) != "" {
				m.entries = append(m.entries, &transcriptEntry{kind: entryReasoning, text: msg.ReasoningContent})
			}
			if strings.TrimSpace(msg.Content) != "" {
				m.entries = append(m.entries, &transcriptEntry{kind: entryAssistant, text: msg.Content})
			}
			for _, call := range msg.ToolCalls {
				m.entries = append(m.entries, &transcriptEntry{
					kind: entryTool, toolName: call.Function.Name,
					toolID: call.ID, args: call.Function.Arguments, finished: true,
				})
			}
		case provider.RoleTool:
			if strings.TrimSpace(msg.Content) != "" {
				m.entries = append(m.entries, &transcriptEntry{
					kind: entryTool, toolName: emptyFallback(msg.Name, "tool"),
					toolID: msg.ToolCallID, output: msg.Content, finished: true,
				})
			}
		}
	}
	m.historyChars = historyChars(msgs)
	if m.opts.Agent != nil && len(msgs) > 0 {
		m.setContextTokens(m.opts.Agent.ContextTokens())
	} else {
		m.setContextTokens(m.historyChars / 4)
	}
	m.followTail = true
	m.dirty = true
}

func messageText(msg provider.Message) string {
	if len(msg.Parts) > 0 {
		var b strings.Builder
		for _, part := range msg.Parts {
			if part.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(part.Text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return msg.Content
}

func (item modalSession) displayName() string {
	if strings.TrimSpace(item.Name) != "" {
		return item.Name
	}
	return item.ID
}

func (m *Model) sessionsModalView() string {
	m.modalRows = nil
	var b strings.Builder
	items := m.filteredSessions()
	filter := m.modal.filter
	if m.modal.rename {
		b.WriteString(m.st.paint(m.st.amber, "rename: ") + m.modal.renameText + "▌\n\n")
	} else {
		b.WriteString(m.st.paint(m.st.dim, "filter: ") + filter)
		if filter == "" {
			b.WriteString(m.st.paint(m.st.dim, " (type to narrow, / to force)"))
		}
		b.WriteString("\n\n")
	}
	activeID := ""
	if m.opts.Session != nil {
		activeID = m.opts.Session.ID()
	}
	if len(items) == 0 {
		b.WriteString(m.st.paint(m.st.dim, "No sessions found. Press n to create one."))
	} else {
		limit := m.modalRowLimit()
		start, end := windowRange(len(items), m.modal.index, limit)
		if start > 0 {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d above\n", start)))
		}
		for i := start; i < end; i++ {
			m.addModalRow(strings.Count(b.String(), "\n"), i)
			item := items[i]
			marker := "  "
			style := m.st.dim
			if i == m.modal.index {
				marker = "▸ "
				style = m.st.selected
				if m.st.plain {
					marker = "> "
				}
			}
			active := " "
			if item.ID == activeID {
				active = "*"
			}
			line := fmt.Sprintf("%s%s %-24s %-16s %2dt %3dm %6s $%.3f %6s %s",
				marker, active,
				truncateCell(item.displayName(), 24),
				truncateCell(item.Model, 16),
				item.Turns, item.Messages,
				compactTokens(item.Usage.TotalTokens),
				item.CostUSD,
				compactDuration(item.Duration),
				item.UpdatedAt.Local().Format("01-02 15:04"),
			)
			b.WriteString(m.st.paint(style, truncateCell(line, maxInt(30, m.modalTextWidth()))))
			b.WriteString("\n")
		}
		if end < len(items) {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d below\n", len(items)-end)))
		}
	}
	if m.modal.notice != "" {
		b.WriteString("\n" + m.st.paint(m.st.amber, m.modal.notice))
	}
	return m.modalFrame("Sessions", strings.TrimRight(b.String(), "\n"),
		"↑/↓ select · enter open · n new · f fork · r rename · d delete · esc close")
}

// ---------------------------------------------------------------------------
// Usage & activity stats
// ---------------------------------------------------------------------------

func (m *Model) openStatsModal() {
	if m.opts.SessionStore == nil {
		m.addError("Session storage is not configured.")
		return
	}
	stats, err := m.opts.SessionStore.Stats()
	if err != nil {
		m.addError("stats: " + err.Error())
		return
	}
	m.setModal(modalStats)
	m.modal.stats = stats
	m.modal.index = 0
	m.statusMsg = "Usage & activity"
}

func (m *Model) handleStatsModalKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.modal.index--
		m.clampStatsIndex()
		return nil
	case tea.KeyDown:
		m.modal.index++
		m.clampStatsIndex()
		return nil
	case tea.KeyPgUp:
		m.modal.index -= 5
		m.clampStatsIndex()
		return nil
	case tea.KeyPgDown:
		m.modal.index += 5
		m.clampStatsIndex()
		return nil
	case tea.KeyEnter:
		m.resumeStatsSession()
		return nil
	}
	return nil
}

func (m *Model) clampStatsIndex() {
	n := 0
	if m.modal.stats != nil {
		n = len(m.modal.stats.Sessions)
	}
	if n == 0 {
		m.modal.index = 0
		return
	}
	if m.modal.index < 0 {
		m.modal.index = n - 1
	}
	if m.modal.index >= n {
		m.modal.index = 0
	}
}

func (m *Model) resumeStatsSession() {
	if m.modal.stats == nil || m.opts.SessionStore == nil {
		return
	}
	if len(m.modal.stats.Sessions) == 0 {
		return
	}
	row := m.modal.stats.Sessions[m.modal.index]
	sess, err := m.opts.SessionStore.Open(row.ID)
	if err != nil {
		m.modal.notice = "open failed: " + err.Error()
		return
	}
	m.activateSession(sess, "Resumed session "+sess.ID()+".")
}

func (m *Model) statsModalView() string {
	m.modalRows = nil
	if m.modal.stats == nil {
		return m.modalFrame("Usage & activity", "No stats available.", "esc close")
	}
	s := m.modal.stats
	var b strings.Builder

	// 1-line KPI strip.
	fmt.Fprintf(&b, "%s %d  %s %d  %s %s  %s %s  %s %s  %s $%.3f\n",
		m.st.paint(m.st.dim, "Sessions"), s.SessionCount,
		m.st.paint(m.st.dim, "Turns"), s.TurnCount,
		m.st.paint(m.st.dim, "Tokens"), compactTokens(s.TotalUsage.TotalTokens),
		m.st.paint(m.st.dim, "Active"), compactDuration(s.TotalDuration),
		m.st.paint(m.st.dim, "Avg"), formatTokensPerSec(s.AvgTokensPerSec),
		m.st.paint(m.st.dim, "Cost"), s.TotalCostUSD)

	// Cache hit gauge: exactly 20 cells.
	cachePct := s.CacheHitRate * 100
	b.WriteString(m.st.paint(m.st.dim, "Cache   "))
	b.WriteString(m.st.paint(m.st.sage, barGauge(s.CacheHitRate, 20)))
	fmt.Fprintf(&b, "  %s\n", m.st.paint(m.st.accent, fmt.Sprintf("%.1f%%", cachePct)))

	// Token volume and speed sparklines over recent sessions (oldest -> newest).
	series := recentStatsRows(s.Sessions, 24)
	volumes := make([]float64, 0, len(series))
	speeds := make([]float64, 0, len(series))
	for _, row := range series {
		volumes = append(volumes, float64(row.Usage.TotalTokens))
		speeds = append(speeds, row.AvgTokensPerSec)
	}
	b.WriteString(m.st.paint(m.st.dim, "Activity ") + " " + m.st.paint(m.st.accent, sparkline(volumes, 12)) + "\n")
	b.WriteString(m.st.paint(m.st.dim, "Speed    ") + " " + m.st.paint(m.st.teal, sparkline(speeds, 12)) + "\n")

	// Compact model usage bars (one line per model, capped).
	b.WriteString(m.st.paint(m.st.bold, "Models") + "\n")
	if len(s.Models) == 0 {
		b.WriteString(m.st.paint(m.st.dim, "  no model usage recorded") + "\n")
	} else {
		maxTokens := 1
		for _, model := range s.Models {
			if model.Usage.TotalTokens > maxTokens {
				maxTokens = model.Usage.TotalTokens
			}
		}
		limit := len(s.Models)
		if limit > 3 {
			limit = 3
		}
		for _, model := range s.Models[:limit] {
			width := 0
			if maxTokens > 0 {
				width = int(float64(model.Usage.TotalTokens)/float64(maxTokens)*10 + 0.5)
			}
			bar := strings.Repeat("█", width) + strings.Repeat("░", 10-width)
			fmt.Fprintf(&b, "  %-14s %s %8s  %s\n",
				truncateCell(model.Model, 14), m.st.paint(m.st.accent, bar),
				compactTokens(model.Usage.TotalTokens), m.st.paint(m.st.dim, fmt.Sprintf("$%.3f", model.CostUSD)))
		}
		if len(s.Models) > limit {
			// The count keeps the dashboard compact without an extra row.
			fmt.Fprintf(&b, "  %s\n", m.st.paint(m.st.dim, fmt.Sprintf("(%d more models not shown)", len(s.Models)-limit)))
		}
	}

	// Recent sessions, capped at five rows with a highlighted selection.
	recentHeader := "Recent"
	if len(s.Sessions) > 5 {
		recentHeader = fmt.Sprintf("Recent (%d of %d)", 5, len(s.Sessions))
	}
	b.WriteString(m.st.paint(m.st.bold, recentHeader) + " " + m.st.paint(m.st.dim, "(enter resumes)") + "\n")
	if len(s.Sessions) == 0 {
		b.WriteString(m.st.paint(m.st.dim, "  no sessions recorded yet"))
	} else {
		start, end := windowRange(len(s.Sessions), m.modal.index, 5)
		activeID := ""
		if m.opts.Session != nil {
			activeID = m.opts.Session.ID()
		}
		for i := start; i < end; i++ {
			m.addModalRow(strings.Count(b.String(), "\n"), i)
			row := s.Sessions[i]
			marker := "  "
			style := m.st.dim
			if i == m.modal.index {
				marker = "▸ "
				style = m.st.selected
				if m.st.plain {
					marker = "> "
				}
			}
			active := " "
			if row.ID == activeID {
				active = "*"
			}
			name := row.Name
			if strings.TrimSpace(name) == "" {
				name = row.ID
			}
			line := fmt.Sprintf("%s%s %-20s %-12s %2dt %7s $%.3f %s",
				marker, active, truncateCell(name, 20), truncateCell(row.Model, 12),
				row.Turns, compactTokens(row.Usage.TotalTokens), row.CostUSD, compactDuration(row.Duration))
			b.WriteString(m.st.paint(style, truncateCell(line, maxInt(30, m.modalTextWidth()))))
			b.WriteString("\n")
		}
	}
	if m.modal.notice != "" {
		b.WriteString(m.st.paint(m.st.amber, m.modal.notice))
	}
	return m.modalFrame("Usage & activity", strings.TrimRight(b.String(), "\n"),
		"↑/↓ highlight · enter resume · esc close")
}

func recentStatsRows(rows []session.SessionStatRow, limit int) []session.SessionStatRow {
	if len(rows) == 0 || limit <= 0 {
		return nil
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]session.SessionStatRow, len(rows))
	for i, row := range rows {
		out[len(rows)-1-i] = row
	}
	return out
}

// barGauge renders a proportional filled bar with the given number of cells.
func barGauge(ratio float64, width int) string {
	if width <= 0 {
		return ""
	}
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	filled := int(ratio*float64(width) + 0.5)
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// sparkline renders values as a compact left-to-right activity strip. Short
// series are left-padded with the lowest glyph so the shape stays readable.
func sparkline(values []float64, minWidth int) string {
	if minWidth < 1 {
		minWidth = 1
	}
	if len(values) == 0 {
		return strings.Repeat(string(sparkRunes[0]), minWidth)
	}
	maxValue := 0.0
	for _, value := range values {
		if value > maxValue {
			maxValue = value
		}
	}
	var b strings.Builder
	if len(values) < minWidth {
		b.WriteString(strings.Repeat(string(sparkRunes[0]), minWidth-len(values)))
	}
	for _, value := range values {
		idx := 0
		if maxValue > 0 && value > 0 {
			idx = int(value/maxValue*float64(len(sparkRunes)-1) + 0.5)
			if idx < 0 {
				idx = 0
			}
			if idx >= len(sparkRunes) {
				idx = len(sparkRunes) - 1
			}
		}
		b.WriteRune(sparkRunes[idx])
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

func (m *Model) openSkillsModal() {
	skills, err := terncontext.ListSkills(m.opts.WorkDir, m.opts.GlobalConfigDir)
	if err != nil {
		m.addError("skills: " + err.Error())
		return
	}
	m.setModal(modalSkills)
	m.modal.skills = skills
	m.modal.index = 0
	m.loadSkillPreview()
	m.statusMsg = "Skills"
}

func (m *Model) loadSkillPreview() {
	m.modal.skillPreview = ""
	if len(m.modal.skills) == 0 {
		return
	}
	if m.modal.index < 0 {
		m.modal.index = len(m.modal.skills) - 1
	}
	if m.modal.index >= len(m.modal.skills) {
		m.modal.index = 0
	}
	meta := m.modal.skills[m.modal.index]
	skill, err := terncontext.LoadSkill(m.opts.WorkDir, m.opts.GlobalConfigDir, meta.Name)
	if err != nil {
		m.modal.skillPreview = "Could not load skill: " + err.Error()
		return
	}
	m.modal.skillPreview = skill.Body
}

func (m *Model) handleSkillsModalKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.modal.index--
		m.loadSkillPreview()
		return nil
	case tea.KeyDown:
		m.modal.index++
		m.loadSkillPreview()
		return nil
	case tea.KeyEnter:
		m.loadSelectedSkill()
		return nil
	case tea.KeySpace:
		return nil
	case tea.KeyRunes:
		switch strings.ToLower(string(msg.Runes)) {
		case "c":
			m.createExampleSkill()
		}
		return nil
	}
	return nil
}

func (m *Model) loadSelectedSkill() {
	if len(m.modal.skills) == 0 {
		return
	}
	meta := m.modal.skills[m.modal.index]
	skill, err := terncontext.LoadSkill(m.opts.WorkDir, m.opts.GlobalConfigDir, meta.Name)
	if err != nil {
		m.modal.notice = "skills: " + err.Error()
		return
	}
	m.ed.SetValue(skill.Body)
	m.closeModal()
	m.statusMsg = "Loaded skill " + meta.Name + " into the editor"
	m.addInfo("Loaded skill " + meta.Name + " into the editor. Press Enter to submit it as context.")
}

func (m *Model) createExampleSkill() {
	dir := filepath.Join(m.opts.WorkDir, ".tern", "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.modal.notice = "create skill: " + err.Error()
		return
	}
	path := filepath.Join(dir, "example.md")
	if _, err := os.Stat(path); err == nil {
		m.modal.notice = "Starter skill already exists at " + path
		return
	}
	content := "---\n" +
		"name: example\n" +
		"description: A starter skill for tern.\n" +
		"tags: [example]\n" +
		"---\n\n" +
		"# Example skill\n\n" +
		"Describe when to use this skill and the steps the agent should follow.\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		m.modal.notice = "create skill: " + err.Error()
		return
	}
	skills, err := terncontext.ListSkills(m.opts.WorkDir, m.opts.GlobalConfigDir)
	if err == nil {
		m.modal.skills = skills
		for i, skill := range skills {
			if skill.Name == "example" {
				m.modal.index = i
				break
			}
		}
		m.loadSkillPreview()
	}
	m.modal.notice = "Created starter skill at " + path
}

func (m *Model) skillsModalView() string {
	m.modalRows = nil
	var b strings.Builder
	if len(m.modal.skills) == 0 {
		b.WriteString(m.st.paint(m.st.dim, "No skills found."))
		b.WriteString("\n\nPress c to create a starter skill at ./.tern/skills/example.md.\n")
	} else {
		limit := maxInt(4, minInt(len(m.modal.skills), m.modalRowLimit()/2))
		start, end := windowRange(len(m.modal.skills), m.modal.index, limit)
		if start > 0 {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d above\n", start)))
		}
		for i := start; i < end; i++ {
			m.addModalRow(strings.Count(b.String(), "\n"), i)
			skill := m.modal.skills[i]
			marker := "  "
			style := m.st.dim
			if i == m.modal.index {
				marker = "▸ "
				style = m.st.selected
				if m.st.plain {
					marker = "> "
				}
			}
			source := skill.Scope
			if source == "" {
				source = "project"
			}
			line := fmt.Sprintf("%s%-20s %-7s %s", marker, truncateCell(skill.Name, 20),
				truncateCell(source, 7), truncateCell(skill.Description, maxInt(20, m.modalTextWidth()-32)))
			b.WriteString(m.st.paint(style, line))
			b.WriteString("\n")
		}
		if end < len(m.modal.skills) {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d below\n", len(m.modal.skills)-end)))
		}
		b.WriteString("\n" + m.st.paint(m.st.bold, "Preview") + "\n")
		preview := m.modal.skillPreview
		if strings.TrimSpace(preview) == "" {
			preview = "(empty skill body)"
		}
		preview = truncateLines(preview, m.modalRowLimit()/2)
		b.WriteString(m.st.paint(m.st.dim, indentLines(wrapPlainCell(preview, maxInt(20, m.modalTextWidth()-4)), "  ")))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(m.st.paint(m.st.dim, "global: ") + filepath.Join(m.opts.GlobalConfigDir, "skills") + m.st.paint(m.st.dim, "  (~/.config/tern/skills)"))
	b.WriteString("\n")
	b.WriteString(m.st.paint(m.st.dim, "project: ") + filepath.Join(m.opts.WorkDir, ".tern", "skills") + m.st.paint(m.st.dim, "  (./.tern/skills)"))
	if m.modal.notice != "" {
		b.WriteString("\n" + m.st.paint(m.st.amber, m.modal.notice))
	}
	return m.modalFrame("Skills", strings.TrimRight(b.String(), "\n"),
		"↑/↓ browse · enter load into editor · c create starter · esc close")
}

// ---------------------------------------------------------------------------
// Extensions
// ---------------------------------------------------------------------------

func (m *Model) openExtModal() {
	m.setModal(modalExt)
	m.refreshExtensions()
	m.statusMsg = "Extensions"
}

func (m *Model) refreshExtensions() {
	extensions, warnings := ext.ListAllExtensions(m.opts.WorkDir, m.opts.GlobalConfigDir)
	m.modal.extensions = extensions
	if len(warnings) > 0 {
		m.modal.notice = strings.Join(warnings, " · ")
	} else {
		m.modal.notice = ""
	}
	m.clampExtIndex()
	m.dirty = true
}

func (m *Model) clampExtIndex() {
	n := len(m.modal.extensions)
	if n == 0 {
		m.modal.index = 0
		return
	}
	if m.modal.index < 0 {
		m.modal.index = n - 1
	}
	if m.modal.index >= n {
		m.modal.index = 0
	}
}

func (m *Model) handleExtModalKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.modal.index--
		m.clampExtIndex()
		m.dirty = true
		return nil
	case tea.KeyDown:
		m.modal.index++
		m.clampExtIndex()
		m.dirty = true
		return nil
	case tea.KeyRunes:
		switch strings.ToLower(string(msg.Runes)) {
		case "r":
			m.refreshExtensions()
			m.modal.notice = "Extensions refreshed."
			m.statusMsg = "Extensions refreshed"
		}
		return nil
	}
	return nil
}

// extCountLabel renders a compact "N tools · N hooks · N mcp" count summary.
func extCountLabel(item *ext.Extension) string {
	if item == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if n := len(item.Manifest.Tools); n > 0 {
		parts = append(parts, fmt.Sprintf("%d tool(s)", n))
	}
	if n := len(item.Manifest.Hooks); n > 0 {
		parts = append(parts, fmt.Sprintf("%d hook(s)", n))
	}
	if n := len(item.Manifest.MCP); n > 0 {
		parts = append(parts, fmt.Sprintf("%d mcp", n))
	}
	if len(parts) == 0 {
		return "no tools"
	}
	return strings.Join(parts, " · ")
}

func (m *Model) extModalView() string {
	m.modalRows = nil
	var b strings.Builder
	if len(m.modal.extensions) == 0 {
		b.WriteString(m.st.paint(m.st.dim, "No plugins installed."))
		b.WriteString("\n\n")
		b.WriteString(m.st.paint(m.st.bold, "Plugin locations") + "\n")
		b.WriteString("  " + m.st.paint(m.st.dim, "global  ") + "~/.config/tern/plugins/\n")
		b.WriteString("  " + m.st.paint(m.st.dim, "project ") + "./.tern/plugins/\n")
		b.WriteString("\n" + m.st.paint(m.st.bold, "Create or install") + "\n")
		b.WriteString("  " + m.st.paint(m.st.teal, "tern plugin new <name>") + "\n")
		b.WriteString("  " + m.st.paint(m.st.teal, "tern plugin install <path-or-git-url>"))
	} else {
		m.clampExtIndex()
		limit := maxInt(4, minInt(len(m.modal.extensions), m.modalRowLimit()/2))
		start, end := windowRange(len(m.modal.extensions), m.modal.index, limit)
		if start > 0 {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d above\n", start)))
		}
		for i := start; i < end; i++ {
			m.addModalRow(strings.Count(b.String(), "\n"), i)
			item := m.modal.extensions[i]
			marker := "  "
			style := m.st.dim
			if i == m.modal.index {
				marker = "▸ "
				style = m.st.selected
				if m.st.plain {
					marker = "> "
				}
			}
			version := emptyFallback(item.Manifest.Version, "-")
			source := emptyFallback(item.Source, ext.ExtensionSourceProject)
			badge := "enabled"
			if !item.Manifest.IsEnabled() {
				badge = "disabled"
			}
			line := fmt.Sprintf("%s%-18s %-8s %-7s %-8s  %s",
				marker, truncateCell(item.Manifest.Name, 18), truncateCell(version, 8),
				truncateCell(source, 7), truncateCell(badge, 8),
				truncateCell(extCountLabel(item), maxInt(12, m.modalTextWidth()-48)))
			b.WriteString(m.st.paint(style, truncateCell(line, maxInt(30, m.modalTextWidth()))))
			b.WriteString("\n")
		}
		if end < len(m.modal.extensions) {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d below\n", len(m.modal.extensions)-end)))
		}
		selected := m.modal.extensions[m.modal.index]
		description := strings.TrimSpace(selected.Manifest.Description)
		if description == "" {
			description = "(no description)"
		}
		b.WriteString("\n" + m.st.paint(m.st.bold, "Description") + "\n")
		b.WriteString(m.st.paint(m.st.dim, indentLines(wrapPlainCell(description, maxInt(20, m.modalTextWidth()-4)), "  ")))
		b.WriteString("\n\n")
		b.WriteString(m.st.paint(m.st.dim, "source: "+selected.Source+"  ·  dir: "+selected.Dir))
	}
	if m.modal.notice != "" {
		b.WriteString("\n\n" + m.st.paint(m.st.amber, m.modal.notice))
	}
	return m.modalFrame("Plugins", strings.TrimRight(b.String(), "\n"),
		"↑/↓ select · r refresh · esc close")
}

// ---------------------------------------------------------------------------
// MCP servers
// ---------------------------------------------------------------------------

func (m *Model) openMCPModal() {
	m.setModal(modalMCP)
	m.refreshMCPStatus()
	m.statusMsg = "MCP servers"
}

func (m *Model) refreshMCPStatus() {
	m.modal.mcp = nil
	if m.opts.MCP == nil {
		if m.opts.Extensions != nil {
			m.opts.MCP = m.opts.Extensions
		}
	}
	if m.opts.MCP != nil {
		m.modal.mcp = m.opts.MCP.Status()
	}
	n := len(m.modal.mcp)
	if n == 0 {
		m.modal.index = 0
		return
	}
	if m.modal.index < 0 {
		m.modal.index = n - 1
	}
	if m.modal.index >= n {
		m.modal.index = 0
	}
}

func (m *Model) handleMCPModalKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.modal.index--
		m.refreshMCPStatus()
		return nil
	case tea.KeyDown:
		m.modal.index++
		m.refreshMCPStatus()
		return nil
	case tea.KeyRunes:
		switch strings.ToLower(string(msg.Runes)) {
		case "r":
			m.reconnectMCP()
		}
		return nil
	}
	return nil
}

func (m *Model) reconnectMCP() {
	if m.opts.MCP == nil {
		m.modal.notice = "No MCP manager configured."
		return
	}
	m.modal.notice = "Reconnecting MCP servers…"
	m.statusMsg = "Reconnecting MCP servers…"
	m.dirty = true
	manager := m.opts.MCP
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := manager.Connect(ctx)
		if err != nil {
			m.bus.Push(cmdResultMsg{text: "MCP reconnect", err: err})
			return
		}
		m.bus.Push(cmdResultMsg{text: "MCP servers reconnected."})
	}()
}

func (m *Model) mcpModalView() string {
	m.modalRows = nil
	m.refreshMCPStatus()
	var b strings.Builder
	if len(m.modal.mcp) == 0 {
		b.WriteString(m.st.paint(m.st.dim, "No MCP servers configured."))
		b.WriteString("\n\n")
		b.WriteString(m.st.paint(m.st.bold, "Example ~/.config/tern/mcp.toml") + "\n")
		b.WriteString(m.st.paint(m.st.codeBlock, strings.Join([]string{
			"[[servers]]",
			`name = "filesystem"`,
			`transport = "stdio"`,
			`command = "npx"`,
			`args = ["-y", "@modelcontextprotocol/server-filesystem", "/path"]`,
			"",
			"[[servers]]",
			`name = "remote"`,
			`transport = "http"`,
			`url = "https://example.com/mcp"`,
		}, "\n")))
		b.WriteString("\n\n")
		b.WriteString(m.st.paint(m.st.dim, "global: ~/.config/tern/mcp.toml") + "\n")
		b.WriteString(m.st.paint(m.st.dim, "project: ./.tern/mcp.toml"))
	} else {
		limit := maxInt(4, minInt(len(m.modal.mcp), m.modalRowLimit()/2))
		start, end := windowRange(len(m.modal.mcp), m.modal.index, limit)
		for i := start; i < end; i++ {
			m.addModalRow(strings.Count(b.String(), "\n"), i)
			server := m.modal.mcp[i]
			marker := "  "
			style := m.st.dim
			if i == m.modal.index {
				marker = "▸ "
				style = m.st.selected
				if m.st.plain {
					marker = "> "
				}
			}
			dot := "●"
			dotStyle := m.st.sage
			state := "connected"
			if !server.Connected {
				dot = "✗"
				dotStyle = m.st.coral
				state = "error"
			}
			if server.Error != "" {
				state = "error"
			}
			transport := emptyFallback(server.Transport, "stdio")
			line := fmt.Sprintf("%s%s %-18s %-7s %-9s %d tool(s)",
				marker, m.st.paint(dotStyle, dot), truncateCell(server.Name, 18),
				truncateCell(transport, 7), truncateCell(state, 9), server.ToolCount)
			b.WriteString(m.st.paint(style, line))
			b.WriteString("\n")
		}
		if end < len(m.modal.mcp) {
			b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d below\n", len(m.modal.mcp)-end)))
		}
		server := m.modal.mcp[m.modal.index]
		if server.Error != "" {
			b.WriteString("\n" + m.st.paint(m.st.coral, "error: "+server.Error))
		}
		b.WriteString("\n\n" + m.st.paint(m.st.bold, "Tools") + "\n")
		if len(server.Tools) == 0 {
			b.WriteString(m.st.paint(m.st.dim, "  (none reported)"))
		} else {
			for _, tool := range server.Tools {
				b.WriteString("  " + m.st.paint(m.st.teal, truncateCell(tool, maxInt(20, m.modalTextWidth()-4))) + "\n")
			}
		}
	}
	if m.modal.notice != "" {
		b.WriteString("\n" + m.st.paint(m.st.amber, m.modal.notice))
	}
	return m.modalFrame("MCP servers", strings.TrimRight(b.String(), "\n"),
		"↑/↓ select · r reconnect · esc close")
}

// ---------------------------------------------------------------------------
// Interactive help
// ---------------------------------------------------------------------------

func (m *Model) openHelpModal() {
	m.setModal(modalHelp)
	m.modal.helpItems = m.helpItems()
	m.modal.index = 0
	m.statusMsg = "Help"
}

func (m *Model) helpItems() []helpItem {
	items := []helpItem{
		{name: "help", usage: "/help", detail: "show this interactive command reference"},
		{name: "model", usage: "/model [name]", detail: "open the model + effort selector or switch model directly"},
		{name: "effort", usage: "/effort <none|low|high|max>", detail: "set reasoning effort immediately", takesArgs: true},
		{name: "thinking", usage: "/thinking <none|low|high|max>", detail: "alias for /effort", takesArgs: true},
		{name: "plan", usage: "/plan", detail: "toggle read-only plan mode"},
		{name: "theme", usage: "/theme [dark|light]", detail: "switch between the dark and light palettes, or toggle when omitted"},
		{name: "stats", usage: "/stats", detail: "visual usage and activity monitor"},
		{name: "sessions", usage: "/sessions [query]", detail: "browse, resume, fork, rename or delete sessions"},
		{name: "agents", usage: "/agents", detail: "inspect or cancel sub-agents"},
		{name: "compact", usage: "/compact [text]", detail: "summarize older context", takesArgs: true},
		{name: "steer", usage: "/steer <text>", detail: "steer the running turn (Ctrl+S), or start a turn when idle", takesArgs: true},
		{name: "queue", usage: "/queue [clear|run]", detail: "show queued prompts, clear them, or run the next one", takesArgs: true},
		{name: "cost", usage: "/cost", detail: "show token usage and estimated cost"},
		{name: "context", usage: "/context", detail: "show context window usage"},
		{name: "fork", usage: "/fork [name]", detail: "fork the current session transcript", takesArgs: true},
		{name: "name", usage: "/name <name>", detail: "rename the current session", takesArgs: true},
		{name: "export", usage: "/export [md|html] [path]", detail: "export the session transcript", takesArgs: true},
		{name: "rewind", usage: "/rewind [turn]", detail: "list turns or restore the state before a turn", takesArgs: true},
		{name: "skills", usage: "/skills [name]", detail: "browse skills or show one directly"},
		{name: "mcp", usage: "/mcp", detail: "MCP servers, tools and reconnect"},
		{name: "ext", usage: "/ext", detail: "browse installed extensions, their tools, hooks and MCP servers"},
		{name: "config", usage: "/config", detail: "show the effective configuration"},
		{name: "clear", usage: "/clear", detail: "clear the transcript view"},
		{name: "undo", usage: "/undo", detail: "restore files from the latest checkpoint"},
		{name: "quit", usage: "/quit", detail: "exit tern"},
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	for _, cmd := range m.commands {
		detail := cmd.Description
		if detail == "" {
			detail = cmd.Path
		}
		items = append(items, helpItem{name: cmd.Name, usage: "/" + cmd.Name, detail: detail})
	}
	return items
}

func (m *Model) handleHelpModalKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.closeModal()
		return nil
	case tea.KeyUp:
		m.modal.index--
		m.clampHelpIndex()
		return nil
	case tea.KeyDown:
		m.modal.index++
		m.clampHelpIndex()
		return nil
	case tea.KeyPgUp:
		m.modal.index -= 8
		m.clampHelpIndex()
		return nil
	case tea.KeyPgDown:
		m.modal.index += 8
		m.clampHelpIndex()
		return nil
	case tea.KeyEnter:
		if len(m.modal.helpItems) == 0 {
			m.closeModal()
			return nil
		}
		item := m.modal.helpItems[m.modal.index]
		m.closeModal()
		if item.name == "help" {
			m.openHelpModal()
			return nil
		}
		if item.takesArgs {
			m.ed.SetValue("/" + item.name + " ")
			m.statusMsg = item.usage
			return nil
		}
		return m.handleSlash("/"+item.name, nil)
	}
	return nil
}

func (m *Model) clampHelpIndex() {
	n := len(m.modal.helpItems)
	if n == 0 {
		m.modal.index = 0
		return
	}
	if m.modal.index < 0 {
		m.modal.index = n - 1
	}
	if m.modal.index >= n {
		m.modal.index = 0
	}
}

func (m *Model) helpModalView() string {
	m.modalRows = nil
	if len(m.modal.helpItems) == 0 {
		return m.modalFrame("Help", "No commands available.", "esc close")
	}
	m.clampHelpIndex()
	var b strings.Builder
	b.WriteString(m.st.paint(m.st.bold, "Commands") + "\n")
	limit := m.height - 12
	if limit < 12 {
		limit = 12
	}
	if limit > len(m.modal.helpItems) {
		limit = len(m.modal.helpItems)
	}
	start, end := windowRange(len(m.modal.helpItems), m.modal.index, limit)
	if start > 0 {
		b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d above\n", start)))
	}
	width := maxInt(30, m.modalTextWidth())
	for i := start; i < end; i++ {
		m.addModalRow(strings.Count(b.String(), "\n"), i)
		item := m.modal.helpItems[i]
		marker := "  "
		style := m.st.dim
		if i == m.modal.index {
			marker = "▸ "
			style = m.st.selected
			if m.st.plain {
				marker = "> "
			}
		}
		line := fmt.Sprintf("%s%-15s %s", marker, "/"+item.name, item.detail)
		b.WriteString(m.st.paint(style, truncateCell(line, width)))
		b.WriteString("\n")
	}
	if end < len(m.modal.helpItems) {
		b.WriteString(m.st.paint(m.st.dim, fmt.Sprintf("  … %d below\n", len(m.modal.helpItems)-end)))
	}
	b.WriteString("\n" + m.st.paint(m.st.dim, "Keys  Ctrl+S steer · Ctrl+P model · Shift+Tab effort · Ctrl+T details · Ctrl+O agents · Ctrl+B sidebar · Ctrl+C cancel/quit"))
	current := m.modal.helpItems[m.modal.index]
	footer := current.usage + " — " + current.detail + " · ↑/↓ scroll · enter run · esc close"
	return m.modalFrame("Help", strings.TrimRight(b.String(), "\n"), footer)
}

// ---------------------------------------------------------------------------
// Changelog / What's New modal
// ---------------------------------------------------------------------------

func (m *Model) changelogSource() string {
	if strings.TrimSpace(m.opts.Changelog) != "" {
		return m.opts.Changelog
	}
	return tern.Changelog
}

func (m *Model) maybeShowUpgradeChangelog(configDir string) {
	pendingPath := filepath.Join(configDir, "pending_changelog")
	pending := false
	if _, err := os.Stat(pendingPath); err == nil {
		pending = true
		_ = os.Remove(pendingPath)
	}

	ver := strings.TrimSpace(m.opts.Version)
	isRealVer := ver != "" && ver != "dev" && ver != "test"
	path := filepath.Join(configDir, "last_seen_version")
	prevBytes, err := os.ReadFile(path)
	prev := strings.TrimSpace(string(prevBytes))

	if pending || (isRealVer && err == nil && prev != "" && prev != ver) {
		m.openChangelogModal()
	}
	if isRealVer && prev != ver {
		_ = os.MkdirAll(configDir, 0o755)
		_ = os.WriteFile(path, []byte(ver+"\n"), 0o600)
	}
}

func (m *Model) openChangelogModal() {
	m.setModal(modalChangelog)
	m.modal.changelogScroll = 0
	m.statusMsg = "What's New"
}

func (m *Model) handleChangelogModalKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyEnter:
		m.closeModal()
		return nil
	case tea.KeyUp:
		if m.modal.changelogScroll > 0 {
			m.modal.changelogScroll--
		}
		return nil
	case tea.KeyDown:
		m.modal.changelogScroll++
		return nil
	case tea.KeyPgUp:
		m.modal.changelogScroll -= 8
		if m.modal.changelogScroll < 0 {
			m.modal.changelogScroll = 0
		}
		return nil
	case tea.KeyPgDown:
		m.modal.changelogScroll += 8
		return nil
	case tea.KeyHome:
		m.modal.changelogScroll = 0
		return nil
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "q":
			m.closeModal()
		case "k":
			if m.modal.changelogScroll > 0 {
				m.modal.changelogScroll--
			}
		case "j":
			m.modal.changelogScroll++
		case "g":
			m.modal.changelogScroll = 0
		}
	}
	return nil
}

func (m *Model) changelogModalView() string {
	width := maxInt(36, m.modalTextWidth()-4)
	lines := m.renderChangelogLines(width)
	if len(lines) == 0 {
		lines = []string{m.st.paint(m.st.dim, "No changelog entries available.")}
	}
	visible := m.height - 12
	if visible < 6 {
		visible = 6
	}
	if visible > 24 {
		visible = 24
	}
	maxScroll := len(lines) - visible
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.modal.changelogScroll > maxScroll {
		m.modal.changelogScroll = maxScroll
	}
	if m.modal.changelogScroll < 0 {
		m.modal.changelogScroll = 0
	}
	start := m.modal.changelogScroll
	end := start + visible
	if end > len(lines) {
		end = len(lines)
	}

	title := "What's New"
	if ver := m.versionLabel(); ver != "" {
		title += " · tern " + ver
	}
	footer := "↑↓ scroll  ·  PgUp/PgDn page  ·  Enter / Esc close"
	if maxScroll > 0 {
		footer = fmt.Sprintf("Lines %d–%d of %d  ·  %s", start+1, end, len(lines), footer)
	}
	return m.modalFrame(title, strings.Join(lines[start:end], "\n"), footer)
}

func (m *Model) renderChangelogLines(width int) []string {
	src := strings.ReplaceAll(m.changelogSource(), "\r\n", "\n")
	rawLines := strings.Split(src, "\n")
	out := make([]string, 0, len(rawLines))
	seenVersion := false
	for _, raw := range rawLines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		// Skip top-level "# Changelog" heading, intro prose before first version,
		// and bottom markdown reference links ([v0.4.1]: https://...).
		if strings.HasPrefix(trimmed, "# ") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.Contains(trimmed, "]: http") {
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			if seenVersion {
				out = append(out, "")
			}
			seenVersion = true
			heading := strings.TrimPrefix(trimmed, "## ")
			heading = strings.ReplaceAll(heading, "[", "")
			heading = strings.ReplaceAll(heading, "]", "")
			out = append(out, m.st.paint(m.st.accent, heading))
			continue
		}
		if !seenVersion {
			continue
		}
		if strings.HasPrefix(trimmed, "### ") {
			sec := strings.TrimPrefix(trimmed, "### ")
			style := m.st.info
			switch strings.ToLower(sec) {
			case "added":
				style = m.st.success
			case "fixed":
				style = m.st.warn
			case "removed":
				style = m.st.error
			}
			out = append(out, "", "  "+m.st.paint(style, sec))
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			item := strings.TrimSpace(trimmed[2:])
			bullet := "• "
			if m.st.plain {
				bullet = "- "
			}
			wrapped := strings.Split(wrapPlainCell(item, maxInt(20, width-6)), "\n")
			for i, wline := range wrapped {
				if i == 0 {
					out = append(out, "    "+m.st.paint(m.st.dim, bullet)+wline)
				} else {
					out = append(out, "      "+wline)
				}
			}
			continue
		}
		wrapped := strings.Split(wrapPlainCell(trimmed, maxInt(20, width-4)), "\n")
		for _, wline := range wrapped {
			out = append(out, "  "+m.st.paint(m.st.dim, wline))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Modal frame and helpers
// ---------------------------------------------------------------------------

func (m *Model) modalView() string {
	switch m.activeModal {
	case modalModel:
		return m.modelModalView()
	case modalSessions:
		return m.sessionsModalView()
	case modalStats:
		return m.statsModalView()
	case modalSkills:
		return m.skillsModalView()
	case modalMCP:
		return m.mcpModalView()
	case modalExt:
		return m.extModalView()
	case modalHelp:
		return m.helpModalView()
	case modalChangelog:
		return m.changelogModalView()
	case modalAgentPeek:
		return m.agentPeekView()
	case modalProvider:
		return m.providerModalView()
	}
	return ""
}

func (m *Model) modalFrame(title, body, footer string) string {
	if m.st.plain {
		var b strings.Builder
		b.WriteString(title)
		b.WriteString("\n\n")
		b.WriteString(body)
		if footer != "" {
			b.WriteString("\n\n")
			b.WriteString(footer)
		}
		return b.String()
	}
	content := m.st.paint(m.st.modalTitle, title) + "\n\n" + body
	if footer != "" {
		content += "\n\n" + m.st.paint(m.st.dim, footer)
	}
	width := m.modalTextWidth()
	box := m.st.modalBorder.Width(width).Render(content)
	return lipgloss.PlaceHorizontal(maxInt(m.width, width+6), lipgloss.Center, box)
}

func (m *Model) modalTextWidth() int {
	width := m.width - 8
	if width < 32 {
		width = 32
	}
	if width > 104 {
		width = 104
	}
	return width
}

func (m *Model) modalRowLimit() int {
	rows := (m.height - 16) / 1
	if rows < 5 {
		rows = 5
	}
	if rows > 18 {
		rows = 18
	}
	return rows
}

func windowRange(total, index, limit int) (int, int) {
	if total <= 0 {
		return 0, 0
	}
	if limit <= 0 {
		limit = 1
	}
	if index < 0 {
		index = 0
	}
	if index >= total {
		index = total - 1
	}
	if total <= limit {
		return 0, total
	}
	start := index - limit/2
	if start < 0 {
		start = 0
	}
	if start+limit > total {
		start = total - limit
	}
	return start, start + limit
}

func compactTokens(tokens int) string {
	switch {
	case tokens >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	case tokens >= 1_000:
		return fmt.Sprintf("%.1fk", float64(tokens)/1_000)
	default:
		return fmt.Sprintf("%d", tokens)
	}
}

func compactDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d >= time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	if d >= time.Minute {
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
	return fmt.Sprintf("%.0fs", d.Seconds())
}
