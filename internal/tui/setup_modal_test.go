package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tern-agent/tern/internal/config"
)

func TestFirstRunSetupModalOpensWithDeepSeekRecommended(t *testing.T) {
	m := New(Options{
		Config:          config.Default(),
		GlobalConfigDir: t.TempDir(),
		NeedsSetup:      true,
		NoColor:         true,
	})
	m.width = 100
	m.height = 36

	if m.activeModal != modalProvider {
		t.Fatalf("activeModal = %v, want modalProvider", m.activeModal)
	}
	view := m.providerModalView()
	for _, want := range []string{"Welcome to tern", "DeepSeek", "recommended", "OpenRouter", "Ollama", "LM Studio", "Custom"} {
		if !strings.Contains(view, want) {
			t.Fatalf("setup modal view missing %q:\n%s", want, view)
		}
	}
	if m.modal.index != 0 || m.modal.providerSetup.options[0].name != "deepseek" {
		t.Fatalf("expected DeepSeek highlighted first, got index=%d", m.modal.index)
	}
}

func TestFirstRunSetupDeepSeekAPIKey(t *testing.T) {
	var gotProvider, gotBaseURL, gotKey, gotModel string
	m := New(Options{
		Config:          config.Default(),
		GlobalConfigDir: t.TempDir(),
		NeedsSetup:      true,
		NoColor:         true,
		OnConfigureProvider: func(providerName, baseURL, apiKey, model string) error {
			gotProvider = providerName
			gotBaseURL = baseURL
			gotKey = apiKey
			gotModel = model
			return nil
		},
	})
	m.width = 100
	m.height = 36

	// Enter configures DeepSeek (index 0).
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.modal.providerSetup.step != providerStepConfigure {
		t.Fatalf("step = %v, want providerStepConfigure", m.modal.providerSetup.step)
	}
	// Empty Enter shows validation error instead of submitting.
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.modal.providerSetup.errMsg == "" {
		t.Fatal("expected error when submitting empty DeepSeek key")
	}

	// Paste key (even with Windows CR/LF and bracketed-paste control sequences) and submit.
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("\x1b[200~sk-deepseek-test\r\n\x1b[201~"), Paste: true})
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})

	// Step 3 of 3: transitions seamlessly into the model selector for the chosen provider.
	if m.activeModal != modalModel || !m.modal.fromProviderSetup {
		t.Fatalf("expected transition to Step 3 model selector, got modal=%v fromSetup=%v", m.activeModal, m.modal.fromProviderSetup)
	}
	if m.needsSetup {
		t.Fatal("expected needsSetup to be cleared")
	}
	if gotProvider != "deepseek" || gotKey != "sk-deepseek-test" || gotBaseURL != config.DefaultBaseURL || gotModel != config.DefaultModel {
		t.Fatalf("configured = (%q, %q, %q, %q)", gotProvider, gotBaseURL, gotKey, gotModel)
	}
	// Confirm model in Step 3 closes the modal.
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.activeModal != modalNone {
		t.Fatalf("expected modal to close after Step 3 model selection, got %v", m.activeModal)
	}
}

func TestFirstRunSetupOpenRouterQuickPick(t *testing.T) {
	var gotProvider, gotBaseURL, gotKey, gotModel string
	m := New(Options{
		Config:          config.Default(),
		GlobalConfigDir: t.TempDir(),
		NeedsSetup:      true,
		NoColor:         true,
		OnConfigureProvider: func(providerName, baseURL, apiKey, model string) error {
			gotProvider = providerName
			gotBaseURL = baseURL
			gotKey = apiKey
			gotModel = model
			return nil
		},
	})
	m.width = 100
	m.height = 36

	// Press '2' to quick-pick OpenRouter.
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	if m.modal.providerSetup.step != providerStepConfigure {
		t.Fatalf("expected configure step after quick pick")
	}
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("sk-or-test-key")})
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})

	if gotProvider != "openrouter" {
		t.Fatalf("provider = %q, want openrouter", gotProvider)
	}
	if gotBaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("baseURL = %q, want openrouter URL", gotBaseURL)
	}
	if gotKey != "sk-or-test-key" {
		t.Fatalf("key = %q, want sk-or-test-key", gotKey)
	}
	if gotModel != "anthropic/claude-sonnet-4" {
		t.Fatalf("model = %q, want anthropic/claude-sonnet-4", gotModel)
	}
}

func TestFirstRunSetupLocalOllamaNoKeyRequired(t *testing.T) {
	var gotProvider, gotBaseURL, gotKey, gotModel string
	m := New(Options{
		Config:          config.Default(),
		GlobalConfigDir: t.TempDir(),
		NeedsSetup:      true,
		NoColor:         true,
		OnConfigureProvider: func(providerName, baseURL, apiKey, model string) error {
			gotProvider = providerName
			gotBaseURL = baseURL
			gotKey = apiKey
			gotModel = model
			return nil
		},
	})
	m.width = 100
	m.height = 36

	// Find Ollama option index and select it.
	ollamaIdx := -1
	for i, o := range m.modal.providerSetup.options {
		if o.name == "ollama" {
			ollamaIdx = i
			break
		}
	}
	if ollamaIdx < 0 {
		t.Fatal("ollama option missing")
	}
	m.modal.index = ollamaIdx
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})
	// Immediately press Enter without entering any API key.
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})

	if m.activeModal != modalModel || !m.modal.fromProviderSetup {
		t.Fatalf("expected transition to Step 3 model selector for local Ollama, modal=%v err=%q", m.activeModal, m.modal.providerSetup.errMsg)
	}
	if gotProvider != "ollama" || gotKey != "local" || gotBaseURL != "http://localhost:11434/v1" || gotModel != "qwen2.5-coder:14b" {
		t.Fatalf("configured = (%q, %q, %q, %q)", gotProvider, gotBaseURL, gotKey, gotModel)
	}
}

func TestProviderSlashCommandOpensModal(t *testing.T) {
	m := New(Options{
		Config:          config.Default(),
		GlobalConfigDir: t.TempDir(),
		NoColor:         true,
	})
	m.width = 100
	m.height = 36

	m.handleSlash("/provider", nil)
	if m.activeModal != modalProvider {
		t.Fatalf("activeModal = %v, want modalProvider", m.activeModal)
	}
}

func TestSubmittingPromptWhileNeedsSetupReopensModal(t *testing.T) {
	m := New(Options{
		Config:          config.Default(),
		GlobalConfigDir: t.TempDir(),
		NeedsSetup:      true,
		NoColor:         true,
	})
	m.width = 100
	m.height = 36

	// Dismiss setup modal first.
	m.handleModalKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.activeModal != modalNone {
		t.Fatalf("expected modal closed after Esc")
	}
	// Type and submit a prompt -> should reopen setup and preserve draft text.
	m.ed.SetValue("hello tern")
	m.submit()
	if m.activeModal != modalProvider {
		t.Fatalf("expected setup modal to reopen on submit while needsSetup")
	}
	if got := m.ed.Value(); got != "hello tern" {
		t.Fatalf("editor draft = %q, want %q", got, "hello tern")
	}
}

func TestPendingChangelogShowsOnceOnNextTUIOpen(t *testing.T) {
	cfgDir := t.TempDir()
	marker := filepath.Join(cfgDir, "pending_changelog")
	if err := os.WriteFile(marker, []byte("v0.6.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m1 := New(Options{
		Config:          config.Default(),
		GlobalConfigDir: cfgDir,
		Version:         "v0.6.0",
		NoColor:         true,
	})
	if m1.activeModal != modalChangelog {
		t.Fatalf("first launch activeModal = %v, want modalChangelog", m1.activeModal)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("expected pending_changelog to be removed after first open")
	}

	// Second launch on the same version should not show the modal again.
	m2 := New(Options{
		Config:          config.Default(),
		GlobalConfigDir: cfgDir,
		Version:         "v0.6.0",
		NoColor:         true,
	})
	if m2.activeModal != modalNone {
		t.Fatalf("second launch activeModal = %v, want modalNone", m2.activeModal)
	}
}
