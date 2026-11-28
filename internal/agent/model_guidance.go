package agent

import "strings"

// modelGuidanceSeparator joins the static base prompt with model-specific
// guidance. It is a constant so the resulting system prompt is byte-identical
// for every step of a session, keeping the prefix cache warm.
const modelGuidanceSeparator = "\n"

// deepSeekFlashPlaybook targets the execution patterns DeepSeek Flash is
// slowest at: one-tool-per-turn loops, shell calls for file inspection, and
// repeated full-suite verification. It is static text with no volatile data,
// so it can safely live inside the cached system prompt prefix.
const deepSeekFlashPlaybook = `# DeepSeek Flash Execution Playbook

You are running on DeepSeek Flash. Reach a correct edit in as few steps as possible.

- Batch every independent read-only call into one turn: several reads, several greps, or
  several globs together. Never run a one-tool-per-turn loop, and never re-read a file you
  already understand.
- Never shell out for file work: no bash with cat, head, tail, find, grep, rg, or sed. Use
  read, grep, glob, edit, and multi_edit instead.
- Combine verification into one chained command, for example "go vet ./... && go test ./...".
  While iterating, run only the focused package test; run the full suite once at the end.
- Keep reasoning and tool plans crisp. Stop exploring as soon as you have enough context to
  make the edit.
- For broad multi-directory exploration or independent parallel work, delegate to task
  sub-agents instead of serializing many sequential reads in this conversation.
`

// ModelOptimizationGuidance returns static, model-specific guidance to append
// to tern's default system prompt, or "" when the model needs none.
//
// The result depends only on the model name: no timestamps, paths, or counters.
// That keeps the system prompt (and the cached prefix) stable per session.
func ModelOptimizationGuidance(model string) string {
	if isDeepSeekFlash(model) {
		return deepSeekFlashPlaybook
	}
	return ""
}

// isDeepSeekFlash reports whether model names DeepSeek Flash, accepting
// case-insensitive and vendor-prefixed forms such as "deepseek/deepseek-flash"
// or "openrouter:deepseek-flash-v4".
func isDeepSeekFlash(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	if i := strings.LastIndexAny(name, "/:"); i >= 0 {
		name = strings.TrimSpace(name[i+1:])
	}
	if !strings.HasPrefix(name, "deepseek-flash") {
		return false
	}
	rest := strings.TrimPrefix(name, "deepseek-flash")
	return rest == "" || strings.HasPrefix(rest, "-") || strings.HasPrefix(rest, ".") || strings.HasPrefix(rest, "@")
}

// effectiveDefaultSystemPrompt builds the system prompt used when the caller
// did not supply one: the static base prompt plus any model guidance.
func effectiveDefaultSystemPrompt(model string) string {
	guidance := ModelOptimizationGuidance(model)
	if guidance == "" {
		return SystemPrompt
	}
	return SystemPrompt + modelGuidanceSeparator + guidance
}
