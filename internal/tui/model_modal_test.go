package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/provider"
)

type fakeListerProvider struct {
	models []provider.RemoteModel
}

func (f *fakeListerProvider) Stream(context.Context, provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	ch := make(chan provider.StreamEvent)
	close(ch)
	return ch, nil
}

func (f *fakeListerProvider) ModelInfo(model string) provider.ModelCapabilities {
	return provider.DefaultModelCapabilities(model)
}

func (f *fakeListerProvider) ListModels(context.Context) ([]provider.RemoteModel, error) {
	return f.models, nil
}

func TestOpenRouterModelModalListsMarksFiltersAndSortsFreeModels(t *testing.T) {
	cfg := config.Default()
	cfg.Provider = "openrouter"
	cfg.BaseURL = "https://openrouter.ai/api/v1"
	cfg.Model = "anthropic/claude-sonnet-4"

	prov := &fakeListerProvider{
		models: []provider.RemoteModel{
			{ID: "anthropic/claude-sonnet-4", ContextLength: 200_000, PromptPerM: 3.0, OutputPerM: 15.0, HasPricing: true, SupportsVision: true},
			{ID: "qwen/qwen3-coder:free", ContextLength: 262_144, HasPricing: true, IsFree: true},
			{ID: "deepseek/deepseek-r1:free", ContextLength: 163_840, HasPricing: true, IsFree: true},
			{ID: "google/gemini-2.5-pro", ContextLength: 1_048_576, PromptPerM: 1.25, OutputPerM: 10.0, HasPricing: true, SupportsVision: true},
		},
	}
	ag := agent.New(agent.Options{Config: cfg, Provider: prov})
	m := New(Options{
		Config:          cfg,
		Agent:           ag,
		GlobalConfigDir: t.TempDir(),
		NoColor:         true,
	})
	m.width = 110
	m.height = 36

	cmd := m.handleSlash("/model", nil)
	if m.activeModal != modalModel {
		t.Fatalf("activeModal = %v, want modalModel", m.activeModal)
	}
	if cmd == nil {
		t.Fatal("expected async fetch command for OpenRouter")
	}
	// Run the async command and feed the result into Update.
	msg := cmd()
	m.Update(msg)

	// OpenRouter defaults to free-first sorting and should NOT list DeepSeek's local-only deepseek-flash.
	if len(m.modal.models) != 4 {
		t.Fatalf("len(models) = %d, want 4", len(m.modal.models))
	}
	if !m.modal.models[0].isFree || !m.modal.models[1].isFree {
		t.Fatalf("expected free models sorted first, got %+v", m.modal.models)
	}
	view := m.modelModalView()
	if !strings.Contains(view, "[FREE]") || !strings.Contains(view, "openrouter") {
		t.Fatalf("expected [FREE] badge and openrouter header in view:\n%s", view)
	}
	if strings.Contains(view, "deepseek-flash") {
		t.Fatalf("OpenRouter /model should not show DeepSeek-only deepseek-flash:\n%s", view)
	}

	// Press 'f' to filter to FREE models only.
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	if !m.modal.freeOnly {
		t.Fatal("expected freeOnly=true after pressing f")
	}
	if len(m.modal.models) != 2 {
		t.Fatalf("freeOnly models = %d, want 2", len(m.modal.models))
	}
	for _, mc := range m.modal.models {
		if !mc.isFree {
			t.Fatalf("non-free model leaked into freeOnly mode: %+v", mc)
		}
	}

	// Toggle free-only off and cycle sort to 'context' (largest context first -> gemini-2.5-pro).
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}) // price
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}) // context
	if m.modal.modelSort != "context" {
		t.Fatalf("modelSort = %q, want context", m.modal.modelSort)
	}
	if m.modal.models[0].name != "google/gemini-2.5-pro" {
		t.Fatalf("largest context model = %q, want google/gemini-2.5-pro", m.modal.models[0].name)
	}

	// Search with '/' for "qwen" and apply with Enter.
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("qwen")})
	if len(m.modal.models) != 1 || m.modal.models[0].name != "qwen/qwen3-coder:free" {
		t.Fatalf("filtered models = %+v, want qwen/qwen3-coder:free", m.modal.models)
	}
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cfg.Model != "qwen/qwen3-coder:free" {
		t.Fatalf("cfg.Model = %q, want qwen/qwen3-coder:free", cfg.Model)
	}
}

func TestModelFreeSlashShortcutOpensFreeOnly(t *testing.T) {
	cfg := config.Default()
	cfg.Provider = "openrouter"
	cfg.BaseURL = "https://openrouter.ai/api/v1"
	cfg.Model = "anthropic/claude-sonnet-4"

	m := New(Options{
		Config:          cfg,
		GlobalConfigDir: t.TempDir(),
		NoColor:         true,
	})
	m.width = 110
	m.height = 36

	m.handleSlash("/model free", nil)
	if m.activeModal != modalModel || !m.modal.freeOnly {
		t.Fatalf("expected /model free to open modalModel with freeOnly=true")
	}
	if len(m.modal.models) == 0 {
		t.Fatal("expected fallback free OpenRouter models to be shown")
	}
	for _, mc := range m.modal.models {
		if !mc.isFree {
			t.Fatalf("expected only free models, got %+v", mc)
		}
	}
}
