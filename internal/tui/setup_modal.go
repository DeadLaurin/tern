package tui

import (
	"fmt"
	"net/url"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tern-agent/tern/internal/config"
)

type providerSetupStep int

const (
	providerStepSelect providerSetupStep = iota
	providerStepConfigure
)

type providerOption struct {
	name         string
	displayName  string
	baseURL      string
	defaultModel string
	envVar       string
	note         string
	dataNotice   string
	recommended  bool
	freeTier     bool
	local        bool
	custom       bool
	hasSavedKey  bool
}

type providerSetupState struct {
	firstRun bool
	step     providerSetupStep
	options  []providerOption

	// Step 2 form fields.
	fieldIdx int
	apiKey   []rune
	model    []rune
	baseURL  []rune
	errMsg   string
}

func (m *Model) openProviderModal(firstRun bool) {
	m.setModal(modalProvider)
	opts := m.buildProviderOptions()
	selected := 0
	if !firstRun && m.opts.Config != nil {
		cur := strings.ToLower(strings.TrimSpace(m.opts.Config.Provider))
		for i, o := range opts {
			if o.name == cur {
				selected = i
				break
			}
		}
	}
	m.modal.index = selected
	m.modal.providerSetup = providerSetupState{
		firstRun: firstRun,
		step:     providerStepSelect,
		options:  opts,
	}
	if firstRun {
		m.statusMsg = "Welcome to tern — choose a provider"
	} else {
		m.statusMsg = "Choose AI provider"
	}
}

func (m *Model) buildProviderOptions() []providerOption {
	presets := config.BuiltinProviders()
	opts := make([]providerOption, 0, len(presets)+1)
	notes := map[string]string{
		"deepseek":   "fast, affordable, prompt caching",
		"openrouter": "300+ models (Claude, GPT, Gemini, DeepSeek, :free tier)",
		"gemini":     "1M context · free tier (no credit card · logs data)",
		"groq":       "ultra-fast inference · free tier (no credit card)",
		"openai":     "GPT-4.1, o3, o4-mini",
		"xai":        "Grok models",
		"mistral":    "Mistral & Codestral",
		"together":   "open-weight hosted models",
		"ollama":     "local models · private · no key",
		"lmstudio":   "local models · private · no key",
	}
	for _, p := range presets {
		local := !p.RequiresKey
		hasKey := false
		if p.RequiresKey && m.opts.GlobalConfigDir != "" {
			if k, err := config.LoadAPIKeyForProvider(m.opts.GlobalConfigDir, p.Name); err == nil && strings.TrimSpace(k) != "" {
				hasKey = true
			}
		}
		opts = append(opts, providerOption{
			name:         p.Name,
			displayName:  p.DisplayName,
			baseURL:      p.BaseURL,
			defaultModel: p.DefaultModel,
			envVar:       p.EnvVar,
			note:         notes[p.Name],
			dataNotice:   p.DataNotice,
			recommended:  p.Name == "deepseek",
			freeTier:     p.FreeTier,
			local:        local,
			hasSavedKey:  hasKey,
		})
	}
	opts = append(opts, providerOption{
		name:         "custom",
		displayName:  "Custom OpenAI-compatible",
		baseURL:      "http://localhost:8000/v1",
		defaultModel: "default",
		note:         "vLLM, llama.cpp, LiteLLM, or any /v1 endpoint",
		custom:       true,
	})
	return opts
}

func (m *Model) clampProviderIndex() {
	n := len(m.modal.providerSetup.options)
	if n == 0 {
		m.modal.index = 0
		return
	}
	m.modal.index = clampInt(m.modal.index, 0, n-1)
}

func (m *Model) enterProviderConfigure() {
	ps := &m.modal.providerSetup
	if len(ps.options) == 0 {
		return
	}
	m.clampProviderIndex()
	opt := ps.options[m.modal.index]
	ps.step = providerStepConfigure
	ps.fieldIdx = 0
	ps.apiKey = nil
	ps.model = []rune(opt.defaultModel)
	ps.baseURL = []rune(opt.baseURL)
	ps.errMsg = ""
	if m.opts.Config != nil && strings.EqualFold(m.opts.Config.Provider, opt.name) {
		if strings.TrimSpace(m.opts.Config.Model) != "" {
			ps.model = []rune(m.opts.Config.Model)
		}
		if strings.TrimSpace(m.opts.Config.BaseURL) != "" {
			ps.baseURL = []rune(m.opts.Config.BaseURL)
		}
	}
	m.dirty = true
}

func (m *Model) providerFieldCount(opt providerOption) int {
	if opt.custom {
		return 3 // Base URL, Model, API Key (optional)
	}
	if opt.local {
		return 1 // Base URL (model picked in Step 3!)
	}
	return 1 // Cloud: API Key only (model picked in Step 3!)
}

func (m *Model) activeProviderFieldBuf(opt providerOption, ps *providerSetupState) *[]rune {
	if opt.custom {
		switch ps.fieldIdx {
		case 0:
			return &ps.baseURL
		case 1:
			return &ps.model
		default:
			return &ps.apiKey
		}
	}
	if opt.local {
		return &ps.baseURL
	}
	return &ps.apiKey
}

func (m *Model) handleProviderModalKey(msg tea.KeyMsg) tea.Cmd {
	ps := &m.modal.providerSetup
	if ps.step == providerStepSelect {
		return m.handleProviderSelectKey(msg)
	}
	return m.handleProviderConfigureKey(msg)
}

func (m *Model) handleProviderSelectKey(msg tea.KeyMsg) tea.Cmd {
	ps := &m.modal.providerSetup
	switch msg.Type {
	case tea.KeyEsc:
		wasFirstRun := ps.firstRun
		m.closeModal()
		if wasFirstRun && m.needsSetup {
			m.statusMsg = "Run /provider to configure a provider"
			m.addInfo("Setup skipped. Run /provider anytime to enter an API key or choose a local model.")
		}
		return nil
	case tea.KeyUp:
		if m.modal.index > 0 {
			m.modal.index--
		} else if len(ps.options) > 0 {
			m.modal.index = len(ps.options) - 1
		}
		m.dirty = true
		return nil
	case tea.KeyDown:
		if len(ps.options) > 0 {
			m.modal.index = (m.modal.index + 1) % len(ps.options)
		}
		m.dirty = true
		return nil
	case tea.KeyEnter:
		m.enterProviderConfigure()
		return nil
	case tea.KeyRunes:
		if len(msg.Runes) == 1 {
			r := msg.Runes[0]
			switch {
			case r == 'j':
				if len(ps.options) > 0 {
					m.modal.index = (m.modal.index + 1) % len(ps.options)
				}
				m.dirty = true
				return nil
			case r == 'k':
				if m.modal.index > 0 {
					m.modal.index--
				} else if len(ps.options) > 0 {
					m.modal.index = len(ps.options) - 1
				}
				m.dirty = true
				return nil
			case r >= '1' && r <= '9':
				idx := int(r - '1')
				if idx < len(ps.options) {
					m.modal.index = idx
					m.enterProviderConfigure()
					return nil
				}
			}
		}
	}
	return nil
}

func (m *Model) handleProviderConfigureKey(msg tea.KeyMsg) tea.Cmd {
	ps := &m.modal.providerSetup
	if len(ps.options) == 0 {
		m.closeModal()
		return nil
	}
	opt := ps.options[m.modal.index]
	fieldCount := m.providerFieldCount(opt)
	buf := m.activeProviderFieldBuf(opt, ps)

	if msg.Paste && msg.Type == tea.KeyRunes && buf != nil {
		cleaned := cleanSetupInput(string(msg.Runes), opt, ps.fieldIdx)
		if cleaned != "" {
			*buf = append(*buf, []rune(cleaned)...)
			ps.errMsg = ""
			m.dirty = true
		}
		return nil
	}

	switch msg.Type {
	case tea.KeyEsc:
		ps.step = providerStepSelect
		ps.errMsg = ""
		m.dirty = true
		return nil
	case tea.KeyTab, tea.KeyDown:
		if fieldCount > 1 {
			ps.fieldIdx = (ps.fieldIdx + 1) % fieldCount
			m.dirty = true
		}
		return nil
	case tea.KeyShiftTab, tea.KeyUp:
		if fieldCount > 1 {
			ps.fieldIdx = (ps.fieldIdx - 1 + fieldCount) % fieldCount
			m.dirty = true
		}
		return nil
	case tea.KeyBackspace:
		if buf != nil && len(*buf) > 0 {
			*buf = (*buf)[:len(*buf)-1]
			ps.errMsg = ""
			m.dirty = true
		}
		return nil
	case tea.KeyCtrlU:
		if buf != nil {
			*buf = nil
			ps.errMsg = ""
			m.dirty = true
		}
		return nil
	case tea.KeySpace:
		if buf != nil && !isAPIKeyField(opt, ps.fieldIdx) {
			*buf = append(*buf, ' ')
			ps.errMsg = ""
			m.dirty = true
		}
		return nil
	case tea.KeyRunes:
		if buf != nil {
			cleaned := cleanSetupInput(string(msg.Runes), opt, ps.fieldIdx)
			if cleaned != "" {
				*buf = append(*buf, []rune(cleaned)...)
				ps.errMsg = ""
				m.dirty = true
			}
		}
		return nil
	case tea.KeyEnter:
		return m.commitProviderSetup(opt, ps)
	}
	return nil
}

func isAPIKeyField(opt providerOption, fieldIdx int) bool {
	if opt.custom {
		return fieldIdx == 2
	}
	if opt.local {
		return false
	}
	return fieldIdx == 0
}

func cleanSetupInput(raw string, opt providerOption, fieldIdx int) string {
	if isAPIKeyField(opt, fieldIdx) {
		return config.SanitizeAPIKey(raw)
	}
	var out strings.Builder
	for _, r := range raw {
		if r >= 0x20 && r != 0x7f {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func (m *Model) commitProviderSetup(opt providerOption, ps *providerSetupState) tea.Cmd {
	apiKey := strings.TrimSpace(string(ps.apiKey))
	model := strings.TrimSpace(string(ps.model))
	baseURL := strings.TrimSpace(string(ps.baseURL))

	if baseURL == "" {
		baseURL = opt.baseURL
	}
	if model == "" {
		model = opt.defaultModel
	}
	if opt.custom || opt.local {
		u, err := url.Parse(baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			ps.errMsg = "Enter a valid http:// or https:// Base URL."
			m.dirty = true
			return nil
		}
	}

	if opt.local {
		apiKey = "local"
	} else {
		apiKey = config.SanitizeAPIKey(apiKey)
		if !opt.custom && apiKey == "" {
			if opt.hasSavedKey && m.opts.GlobalConfigDir != "" {
				if saved, err := config.LoadAPIKeyForProvider(m.opts.GlobalConfigDir, opt.name); err == nil && strings.TrimSpace(saved) != "" {
					apiKey = config.SanitizeAPIKey(saved)
				}
			}
			if apiKey == "" {
				ps.errMsg = fmt.Sprintf("Paste your %s API key above (or press Esc to pick another provider).", opt.displayName)
				ps.fieldIdx = 0
				m.dirty = true
				return nil
			}
		}
	}

	if m.opts.OnConfigureProvider != nil {
		if err := m.opts.OnConfigureProvider(opt.name, baseURL, apiKey, model); err != nil {
			ps.errMsg = err.Error()
			m.dirty = true
			return nil
		}
	} else if m.opts.Config != nil {
		m.opts.Config.Provider = opt.name
		m.opts.Config.BaseURL = baseURL
		m.opts.Config.Model = model
		m.opts.Config.APIKey = apiKey
	}

	m.needsSetup = false
	m.cachedModelsKey = ""
	m.cachedModels = nil
	m.balance = nil
	m.ctxWindow = defaultContextWindow(m.opts)

	// Step 3 of 3: seamlessly open the live model picker for this provider.
	cmd := m.openModelModalWithFilter(false)
	m.modal.fromProviderSetup = true
	m.statusMsg = fmt.Sprintf("Step 3 of 3 · Choose a model from %s", opt.displayName)
	balCmd := m.fetchBalanceCmd(false)
	if cmd != nil && balCmd != nil {
		return tea.Batch(cmd, balCmd)
	}
	if cmd != nil {
		return cmd
	}
	return balCmd
}

func (m *Model) renderSetupStepper(step int, providerLabel string) string {
	s1 := "① Provider"
	if providerLabel != "" && step > 1 {
		s1 = "✓ Provider (" + providerLabel + ")"
	} else if step == 1 {
		s1 = "● ① Provider"
	}
	s2 := "② Connect"
	if step > 2 {
		s2 = "✓ Connected"
	} else if step == 2 {
		s2 = "● ② Connect"
	}
	s3 := "③ Model"
	if step == 3 {
		s3 = "● ③ Model"
	}

	paintStep := func(idx int, label string) string {
		if idx < step {
			return m.st.paint(m.st.success, label)
		}
		if idx == step {
			return m.st.paint(m.st.selected, label)
		}
		return m.st.paint(m.st.dim, label)
	}
	arrow := m.st.paint(m.st.dim, "  →  ")
	return paintStep(1, s1) + arrow + paintStep(2, s2) + arrow + paintStep(3, s3)
}

func (m *Model) providerModalView() string {
	ps := &m.modal.providerSetup
	if ps.step == providerStepSelect {
		return m.providerSelectView(ps)
	}
	return m.providerConfigureView(ps)
}

func (m *Model) providerSelectView(ps *providerSetupState) string {
	m.modalRows = nil
	title := "Choose Provider"
	subtitle := "Select a provider, connect it, then pick any model from that provider."
	if ps.firstRun {
		title = "Welcome to tern"
		subtitle = "DeepSeek is recommended for tern's speed & prompt caching. Or choose OpenRouter, another cloud API, or a local LLM."
	}

	width := m.modalTextWidth()
	var lines []string
	lines = append(lines, m.renderSetupStepper(1, ""))
	lines = append(lines, "")
	for _, wline := range strings.Split(wrapPlainCell(subtitle, maxInt(24, width-2)), "\n") {
		lines = append(lines, m.st.paint(m.st.dim, wline))
	}
	lines = append(lines, "")

	for i, opt := range ps.options {
		rowLine := len(lines)
		m.addModalRow(rowLine, i)

		selected := i == m.modal.index
		marker := "  "
		if selected {
			marker = "▸ "
			if m.st.plain {
				marker = "> "
			}
		}

		num := fmt.Sprintf("%d. ", i+1)
		if i >= 9 {
			num = "   "
		}

		badge := ""
		switch {
		case opt.recommended:
			if opt.hasSavedKey {
				badge = m.st.paint(m.st.success, "★ recommended · saved")
			} else {
				badge = m.st.paint(m.st.accent, "★ recommended")
			}
		case opt.freeTier && opt.hasSavedKey:
			badge = m.st.paint(m.st.success, "free tier · saved")
		case opt.freeTier:
			badge = m.st.paint(m.st.success, "free tier")
		case opt.local:
			badge = m.st.paint(m.st.info, "local · private")
		case opt.hasSavedKey:
			badge = m.st.paint(m.st.success, "key saved")
		}

		nameStr := opt.displayName
		if selected {
			nameStr = m.st.paint(m.st.selected, nameStr)
		}
		line := marker + m.st.paint(m.st.dim, num) + nameStr
		if badge != "" {
			line += "  " + badge
		}
		if opt.note != "" {
			line += "  " + m.st.paint(m.st.dim, "· "+opt.note)
		}
		lines = append(lines, line)
	}

	footer := "↑/↓ move · 1-9 quick pick · Enter next · Esc close"
	if ps.firstRun {
		footer = "↑/↓ move · 1-9 quick pick · Enter next · Ctrl+C quit"
	}
	return m.modalFrame(title, strings.Join(lines, "\n"), footer)
}

func (m *Model) providerConfigureView(ps *providerSetupState) string {
	m.modalRows = nil
	if len(ps.options) == 0 {
		return ""
	}
	opt := ps.options[m.modal.index]
	title := fmt.Sprintf("Connect to %s", opt.displayName)
	if opt.recommended {
		title += " ★"
	}

	boxInner := maxInt(28, m.modalTextWidth()-8)
	var lines []string
	lines = append(lines, m.renderSetupStepper(2, opt.displayName))
	lines = append(lines, "")

	if opt.local {
		lines = append(lines, m.st.paint(m.st.dim, "Local server — no API key needed. Prompts stay on your machine."))
	} else if opt.custom {
		lines = append(lines, m.st.paint(m.st.dim, "Connect to any OpenAI-compatible /v1 endpoint."))
	} else {
		hint := fmt.Sprintf("Endpoint: %s", opt.baseURL)
		if opt.envVar != "" {
			hint += fmt.Sprintf("  (%s)", opt.envVar)
		}
		lines = append(lines, m.st.paint(m.st.dim, hint))
		lines = append(lines, m.st.paint(m.st.dim, "Saved to ~/.config/tern/credentials.toml (0600). Next you'll pick a model."))
		if opt.dataNotice != "" {
			lines = append(lines, "")
			for _, wline := range strings.Split(wrapPlainCell(opt.dataNotice, boxInner), "\n") {
				lines = append(lines, m.st.paint(m.st.warn, wline))
			}
		}
	}
	lines = append(lines, "")

	renderInputBox := func(idx int, label, value, placeholder string) {
		active := ps.fieldIdx == idx
		labelLine := "  " + m.st.paint(m.st.dim, label)
		if active {
			labelLine = m.st.paint(m.st.accent, "▸ ") + m.st.paint(m.st.selected, label)
		}
		if len([]rune(value)) > 0 {
			labelLine += "  " + m.st.paint(m.st.success, fmt.Sprintf("✓ %d chars", len([]rune(value))))
		}
		lines = append(lines, labelLine)

		// Show the actual typed/pasted text clearly (no hidden dots so pasting is
		// obvious and verifiable). Placeholder only appears when empty.
		innerText := ""
		if value == "" {
			innerText = m.st.paint(m.st.dim, truncateCell(placeholder, boxInner-2))
			if active {
				innerText = m.st.paint(m.st.accent, "▏") + innerText
			}
		} else {
			visible := value
			runes := []rune(visible)
			maxChars := boxInner - 3
			if maxChars < 10 {
				maxChars = 10
			}
			if len(runes) > maxChars {
				visible = "…" + string(runes[len(runes)-(maxChars-1):])
			}
			innerText = visible
			if active {
				innerText += m.st.paint(m.st.accent, "▏")
			}
		}

		if m.st.plain {
			lines = append(lines, "  [ "+innerText+" ]")
		} else {
			borderStyle := m.st.dim
			if active {
				borderStyle = m.st.accent
			}
			top := "  " + m.st.paint(borderStyle, "┌"+strings.Repeat("─", boxInner)+"┐")
			mid := "  " + m.st.paint(borderStyle, "│ ") + innerText
			bot := "  " + m.st.paint(borderStyle, "└"+strings.Repeat("─", boxInner)+"┘")
			lines = append(lines, top, mid, bot)
		}
		lines = append(lines, "")
	}

	if opt.custom {
		renderInputBox(0, "Base URL", string(ps.baseURL), "http://localhost:8000/v1")
		renderInputBox(1, "Default Model", string(ps.model), "model-name")
		renderInputBox(2, "API Key (optional)", string(ps.apiKey), "leave empty for local/unauthenticated servers")
	} else if opt.local {
		renderInputBox(0, "Base URL", string(ps.baseURL), opt.baseURL)
	} else {
		keyPlaceholder := "paste your " + opt.displayName + " API key here"
		if opt.hasSavedKey {
			keyPlaceholder = "key already saved — press Enter to keep, or paste a new one"
		}
		renderInputBox(0, opt.displayName+" API Key", string(ps.apiKey), keyPlaceholder)
	}

	if ps.errMsg != "" {
		lines = append(lines, m.st.paint(m.st.error, "⚠ "+ps.errMsg))
	}

	footer := "Enter connect & pick model · Ctrl+U clear · Esc back"
	if opt.custom {
		footer = "Enter connect & pick model · Tab next field · Ctrl+U clear · Esc back"
	}
	return m.modalFrame(title, strings.Join(lines, "\n"), footer)
}
