package agent

import (
	"strings"
	"testing"

	"github.com/tern-agent/tern/internal/config"
	terncontext "github.com/tern-agent/tern/internal/context"
)

func TestModelOptimizationGuidanceDeepSeekFlash(t *testing.T) {
	models := []string{
		"deepseek-flash",
		"DeepSeek-Flash",
		"  deepseek-flash  ",
		"DEEPSEEK-FLASH",
		"deepseek/deepseek-flash",
		"deepseek:deepseek-flash",
		"openrouter/deepseek/deepseek-flash",
		"deepseek-flash-v4",
		"deepseek-flash@2024-06",
	}
	for _, model := range models {
		guidance := ModelOptimizationGuidance(model)
		if !strings.Contains(guidance, "# DeepSeek Flash Execution Playbook") {
			t.Fatalf("ModelOptimizationGuidance(%q) = %q, want the Flash playbook", model, guidance)
		}
		for _, want := range []string{"read", "grep", "glob", "multi_edit", "bash", "task"} {
			if !strings.Contains(guidance, want) {
				t.Errorf("ModelOptimizationGuidance(%q) missing %q", model, want)
			}
		}
	}
}

func TestModelOptimizationGuidanceOtherModels(t *testing.T) {
	models := []string{
		"",
		"   ",
		"deepseek-v4-pro",
		"deepseek-reasoner",
		"deepseek-flashlight",
		"gpt-4o",
		"anthropic/claude-sonnet-4",
		"gemini-2.5-pro",
	}
	for _, model := range models {
		if guidance := ModelOptimizationGuidance(model); guidance != "" {
			t.Fatalf("ModelOptimizationGuidance(%q) = %q, want empty", model, guidance)
		}
	}
}

func TestDefaultSystemPromptAppendsGuidanceForFlash(t *testing.T) {
	cfg := config.Default()
	cfg.Model = "deepseek/deepseek-flash"
	cfg.PermissionMode = "yolo"
	cfg.WebPermission = "allow"

	a := New(Options{Config: cfg})

	got := a.systemPrompt()
	if !strings.HasPrefix(got, SystemPrompt) {
		t.Fatalf("system prompt does not start with the static base prompt")
	}
	if !strings.Contains(got, "# DeepSeek Flash Execution Playbook") {
		t.Fatalf("system prompt missing Flash playbook:\n%s", got)
	}
}

func TestDefaultSystemPromptIgnoresGuidanceForOtherModels(t *testing.T) {
	cfg := config.Default()
	cfg.Model = "deepseek-v4-pro"
	cfg.PermissionMode = "yolo"
	cfg.WebPermission = "allow"

	a := New(Options{Config: cfg})
	if got := a.systemPrompt(); got != SystemPrompt {
		t.Fatalf("system prompt = %q, want the unmodified static prompt", got)
	}
}

func TestCustomSystemPromptIsNeverAltered(t *testing.T) {
	cfg := config.Default()
	cfg.Model = "deepseek-flash"
	cfg.PermissionMode = "yolo"
	cfg.WebPermission = "allow"

	const custom = "custom test prompt"
	a := New(Options{Config: cfg, SystemPrompt: custom})
	if got := a.systemPrompt(); got != custom {
		t.Fatalf("system prompt = %q, want %q", got, custom)
	}

	req := a.buildRequest()
	if len(req.Messages) == 0 || req.Messages[0].Content != custom {
		t.Fatalf("request system message = %q, want %q", req.Messages[0].Content, custom)
	}
}

func TestSystemPromptStableAcrossStepsAndTurns(t *testing.T) {
	cfg := config.Default()
	cfg.Model = "deepseek-flash"
	cfg.PermissionMode = "yolo"
	cfg.WebPermission = "allow"

	a := New(Options{Config: cfg})

	first := a.systemPrompt()
	for i := 0; i < 5; i++ {
		if got := a.systemPrompt(); got != first {
			t.Fatalf("system prompt changed on call %d: prefix cache would be invalidated", i)
		}
		req := a.buildRequest()
		if len(req.Messages) == 0 || req.Messages[0].Content != first {
			t.Fatalf("request system message diverged from the cached prompt")
		}
	}

	// A model switch may change the prompt once, but not between steps on the
	// same model.
	a.SetModel("deepseek-v4-pro")
	switched := a.systemPrompt()
	if switched != SystemPrompt {
		t.Fatalf("after switching to a non-Flash model, system prompt = %q, want base prompt", switched)
	}
	for i := 0; i < 3; i++ {
		if got := a.systemPrompt(); got != switched {
			t.Fatalf("system prompt changed after model switch on call %d", i)
		}
	}
}

func TestStaticSystemPromptHasNoVolatileData(t *testing.T) {
	if strings.Contains(SystemPrompt, "# DeepSeek Flash Execution Playbook") {
		t.Fatalf("model guidance must not be baked into the shared static prompt")
	}
	if !strings.Contains(SystemPrompt, "multi_edit") || !strings.Contains(SystemPrompt, "Batch all independent") {
		t.Fatalf("static prompt is missing the shell-free batching rules")
	}
}

func TestBuiltinDeepSeekFlashSkill(t *testing.T) {
	cfg := config.Default()
	cfg.PermissionMode = "yolo"
	cfg.WebPermission = "allow"

	skill := terncontext.BuiltinModelSkill("deepseek/deepseek-flash")
	if skill == nil {
		t.Fatalf("BuiltinModelSkill(deepseek-flash) = nil")
	}
	if skill.Name != terncontext.BuiltinDeepSeekFlashSkill {
		t.Fatalf("skill name = %q", skill.Name)
	}
	if !strings.Contains(skill.Body, "multi_edit") {
		t.Fatalf("builtin skill body is missing the multi_edit rule")
	}
	if terncontext.BuiltinModelSkill("deepseek-v4-pro") != nil {
		t.Fatalf("BuiltinModelSkill(deepseek-v4-pro) should be nil")
	}

	// The built-in skill must not pollute directory discovery.
	workDir := t.TempDir()
	globalDir := t.TempDir()
	metas, err := terncontext.ListSkills(workDir, globalDir)
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	for _, meta := range metas {
		if meta.Name == terncontext.BuiltinDeepSeekFlashSkill {
			t.Fatalf("ListSkills must not return built-in skills on empty dirs")
		}
	}
	if len(metas) != 0 {
		t.Fatalf("ListSkills on empty dirs returned %d skills", len(metas))
	}

	// LoadSkill falls back to the built-in skill.
	loaded, err := terncontext.LoadSkill(workDir, globalDir, terncontext.BuiltinDeepSeekFlashSkill)
	if err != nil {
		t.Fatalf("LoadSkill(builtin): %v", err)
	}
	if loaded.Name != terncontext.BuiltinDeepSeekFlashSkill {
		t.Fatalf("loaded builtin name = %q", loaded.Name)
	}
	if _, err := terncontext.LoadSkill(workDir, globalDir, "missing"); err == nil {
		t.Fatalf("LoadSkill(missing) should still fail")
	}
}
