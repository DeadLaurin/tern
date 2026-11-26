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
	recommended  bool
	local        bool
	custom       bool
	hasSavedKey  bool
}

type providerSetupState struct {
	firstRun bool
	step     providerSetupStep
	options  []providerOption

	// Step 1 form fields.
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
		"openrouter": "300+ models (Claude, GPT, Gemini, DeepSeek)",
		"openai":     "GPT-4.1, o3, o4-mini",
		"groq":       "ultra-fast inference",
		"xai":        "Grok models",
		"mistral":    "Mistral & Codestral",
		"together":   "open-weight hosted models",
		"ollama":     "local models · no API key needed",
		"lmstudio":   "local models · no API key needed",
	}
	for _, p := range presets {
		local := p.EnvVar == ""
		hasKey := false
		if !local && m.opts.GlobalConfigDir != "" {
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
			recommended:  p.Name == "deepseek",
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
	return 2 // Cloud: API Key, Model | Local: Model, Base URL
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
		if ps.fieldIdx == 0 {
			return &ps.model
		}
		return &ps.baseURL
	}
	if ps.fieldIdx == 0 {
		return &ps.apiKey
	}
	return &ps.model
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

	if msg.Paste && msg.Type == tea.KeyRunes {
		cleaned := strings.ReplaceAll(strings.ReplaceAll(string(msg.Runes), "\r", ""), "\n", "")
		*buf = append(*buf, []rune(cleaned)...)
		ps.errMsg = ""
		m.dirty = true
		return nil
	}

	switch msg.Type {
	case tea.KeyEsc:
		ps.step = providerStepSelect
		ps.errMsg = ""
		m.dirty = true
		return nil
	case tea.KeyTab, tea.KeyDown:
		ps.fieldIdx = (ps.fieldIdx + 1) % fieldCount
		m.dirty = true
		return nil
	case tea.KeyShiftTab, tea.KeyUp:
		ps.fieldIdx = (ps.fieldIdx - 1 + fieldCount) % fieldCount
		m.dirty = true
		return nil
	case tea.KeyBackspace:
		if len(*buf) > 0 {
			*buf = (*buf)[:len(*buf)-1]
			ps.errMsg = ""
			m.dirty = true
		}
		return nil
	case tea.KeyCtrlU:
		*buf = nil
		ps.errMsg = ""
		m.dirty = true
		return nil
	case tea.KeySpace:
		*buf = append(*buf, ' ')
		ps.errMsg = ""
		m.dirty = true
		return nil
	case tea.KeyRunes:
		*buf = append(*buf, msg.Runes...)
		ps.errMsg = ""
		m.dirty = true
		return nil
	case tea.KeyEnter:
		return m.commitProviderSetup(opt, ps)
	}
	return nil
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
	if opt.custom {
		u, err := url.Parse(baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			ps.errMsg = "Enter a valid http:// or https:// Base URL."
			m.dirty = true
			return nil
		}
	}

	if opt.local {
		apiKey = "local"
	} else if !opt.custom && apiKey == "" {
		if opt.hasSavedKey && m.opts.GlobalConfigDir != "" {
			if saved, err := config.LoadAPIKeyForProvider(m.opts.GlobalConfigDir, opt.name); err == nil && strings.TrimSpace(saved) != "" {
				apiKey = strings.TrimSpace(saved)
			}
		}
		if apiKey == "" {
			ps.errMsg = fmt.Sprintf("Enter your %s API key (or press Esc to pick another provider).", opt.displayName)
			ps.fieldIdx = 0
			m.dirty = true
			return nil
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
	m.ctxWindow = defaultContextWindow(m.opts)
	m.closeModal()
	m.statusMsg = fmt.Sprintf("Using %s · %s", opt.displayName, model)
	m.addInfo(fmt.Sprintf("Provider configured: %s (%s). Switch anytime with /provider.", opt.displayName, model))
	return nil
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
	title := "Switch AI Provider"
	subtitle := "Choose a cloud provider or a local OpenAI-compatible server."
	if ps.firstRun {
		title = "Welcome to tern — Choose your AI provider"
		subtitle = "DeepSeek is recommended for tern's speed & prompt caching. Or pick OpenRouter, another cloud API, or a local LLM."
	}

	width := m.modalTextWidth()
	var lines []string
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
			marker = "› "
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
				badge = m.st.paint(m.st.success, "★ recommended · key saved")
			} else {
				badge = m.st.paint(m.st.accent, "★ recommended")
			}
		case opt.local:
			badge = m.st.paint(m.st.info, "local · no key")
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

	footer := "↑/↓ move · 1-9 quick pick · Enter select · Esc close"
	if ps.firstRun {
		footer = "↑/↓ move · 1-9 quick pick · Enter configure · Ctrl+C quit"
	}
	return m.modalFrame(title, strings.Join(lines, "\n"), footer)
}

func (m *Model) providerConfigureView(ps *providerSetupState) string {
	m.modalRows = nil
	if len(ps.options) == 0 {
		return ""
	}
	opt := ps.options[m.modal.index]
	title := fmt.Sprintf("Configure %s", opt.displayName)
	if opt.recommended {
		title += " ★"
	}

	var lines []string
	if opt.local {
		lines = append(lines, m.st.paint(m.st.dim, "Local server — no API key required. Press Enter to connect."))
	} else if opt.custom {
		lines = append(lines, m.st.paint(m.st.dim, "Connect to any OpenAI-compatible /v1 endpoint."))
	} else {
		hint := fmt.Sprintf("Endpoint: %s", opt.baseURL)
		if opt.envVar != "" {
			hint += fmt.Sprintf("  (%s)", opt.envVar)
		}
		lines = append(lines, m.st.paint(m.st.dim, hint))
		lines = append(lines, m.st.paint(m.st.dim, "Saved securely to ~/.config/tern/credentials.toml (0600)."))
	}
	lines = append(lines, "")

	renderField := func(idx int, label, value, placeholder string, secret bool) {
		active := ps.fieldIdx == idx
		prefix := "  "
		labelText := m.st.paint(m.st.dim, label)
		if active {
			prefix = m.st.paint(m.st.accent, "› ")
			labelText = m.st.paint(m.st.selected, label)
		}
		display := value
		if secret && len(value) > 0 {
			display = strings.Repeat("•", len([]rune(value)))
		}
		if display == "" {
			display = m.st.paint(m.st.dim, placeholder)
		}
		cursor := ""
		if active {
			cursor = m.st.paint(m.st.accent, "▏")
		}
		lines = append(lines, fmt.Sprintf("%s%s", prefix, labelText))
		lines = append(lines, fmt.Sprintf("    %s%s", display, cursor))
		lines = append(lines, "")
	}

	if opt.custom {
		renderField(0, "Base URL", string(ps.baseURL), "http://localhost:8000/v1", false)
		renderField(1, "Model", string(ps.model), "model-name", false)
		renderField(2, "API Key (optional)", string(ps.apiKey), "leave empty for local/unauthenticated servers", true)
	} else if opt.local {
		renderField(0, "Model", string(ps.model), opt.defaultModel, false)
		renderField(1, "Base URL", string(ps.baseURL), opt.baseURL, false)
	} else {
		keyPlaceholder := "paste your API key…"
		if opt.hasSavedKey {
			keyPlaceholder = "saved — press Enter to keep, or paste a new key"
		}
		renderField(0, "API Key", string(ps.apiKey), keyPlaceholder, true)
		renderField(1, "Model", string(ps.model), opt.defaultModel, false)
	}

	if ps.errMsg != "" {
		lines = append(lines, m.st.paint(m.st.error, "⚠ "+ps.errMsg))
	}

	footer := "Enter save & start · Tab switch field · Ctrl+U clear · Esc back"
	return m.modalFrame(title, strings.Join(lines, "\n"), footer)
}
