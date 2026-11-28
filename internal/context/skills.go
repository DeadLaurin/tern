package context

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrSkillNotFound is returned by LoadSkill when no skill with the requested
// name exists.
var ErrSkillNotFound = errors.New("context: skill not found")

// BuiltinDeepSeekFlashSkill is the name of tern's built-in skill for DeepSeek
// Flash. It is not stored on disk and is deliberately excluded from
// ListSkills, so directory discovery results are unchanged; LoadSkill falls
// back to it only when no user skill overrides the name.
const BuiltinDeepSeekFlashSkill = "deepseek-flash-fast"

// builtinSkillBodies holds tern's built-in skills by name.
var builtinSkillBodies = map[string]Skill{
	BuiltinDeepSeekFlashSkill: {
		SkillMeta: SkillMeta{
			Name:        BuiltinDeepSeekFlashSkill,
			Description: "Fast execution playbook for DeepSeek Flash: batch reads, skip the shell, verify once.",
			Tags:        []string{"deepseek", "deepseek-flash", "performance", "workflow"},
		},
		Body: `# DeepSeek Flash fast execution

Use this when running on DeepSeek Flash (deepseek-flash). It targets the patterns Flash is
slowest at: one-tool-per-turn loops, shelling out for file inspection, and re-running full
verification after every edit.

## Rules

1. Batch independent work. Issue every independent read/grep/glob call in one turn. Several
   reads, several greps, or several globs together, never one call per round-trip.
2. Skip the shell for file work. Do not run bash with cat, head, tail, find, grep, rg, or sed.
   Use the native read, grep, glob, edit, and multi_edit tools.
3. Edit once. Collect all changes to a file and apply them with multi_edit, then re-read only
   the region you changed if needed.
4. Verify cheaply. While iterating, run the focused package test only, for example
   "go test ./internal/agent/". At the end run one chained command covering everything, for
   example "go vet ./... && go test ./...".
5. Stop exploring early. Once you have enough context to make the edit, make it. Do not
   re-read files you already understand or survey directories you do not need.
6. Delegate breadth. For wide multi-directory exploration or independent parallel tasks, hand
   work to task sub-agents instead of serializing many sequential reads yourself.
`,
	},
}

// BuiltinModelSkill returns the built-in skill that matches model, or nil when
// the model has no built-in skill. User skills with the same name always take
// precedence at load time.
func BuiltinModelSkill(model string) *Skill {
	name := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndexAny(name, "/:"); i >= 0 {
		name = strings.TrimSpace(name[i+1:])
	}
	if !strings.HasPrefix(name, "deepseek-flash") {
		return nil
	}
	rest := strings.TrimPrefix(name, "deepseek-flash")
	if rest != "" && !strings.HasPrefix(rest, "-") && !strings.HasPrefix(rest, ".") && !strings.HasPrefix(rest, "@") {
		return nil
	}
	return builtinSkill(BuiltinDeepSeekFlashSkill)
}

// builtinSkill returns a copy of a built-in skill so callers cannot mutate
// the shared definition.
func builtinSkill(name string) *Skill {
	skill, ok := builtinSkillBodies[name]
	if !ok {
		return nil
	}
	skill.Tags = append([]string(nil), skill.Tags...)
	return &skill
}

// SkillMeta is the lightweight metadata returned by ListSkills. The markdown
// body is deliberately omitted so that a system-prompt index can be built
// without reading every skill in full.
type SkillMeta struct {
	Name        string
	Description string
	Tags        []string
	// Path is the absolute path of the markdown file.
	Path string
	// Scope is ScopeGlobal or ScopeProject.
	Scope string
}

// Skill is a fully loaded skill, including its markdown body.
type Skill struct {
	SkillMeta
	// Body is the markdown following any frontmatter.
	Body string
}

type skillCandidate struct {
	meta     SkillMeta
	priority int // higher wins when two files in one root resolve to one name
}

// ListSkills discovers skills in the global and project skill directories,
// including automatic detection of Claude (.claude/skills) and Agent Skills
// (.agents/skills) directories.
//
// Both "<dir>/<name>.md" and "<dir>/<name>/SKILL.md" layouts (including
// symlinked skill folders) are supported. Project skills override global
// skills with the same name, and tern-specific directories take precedence
// over Claude/Agent directories within the same scope. The result is sorted by
// name.
func ListSkills(workDir, globalConfigDir string) ([]SkillMeta, error) {
	workDir, err := resolveDir(workDir)
	if err != nil {
		return nil, err
	}

	merged := make(map[string]skillCandidate)

	mergeRoot := func(dir, scope string) error {
		found, err := discoverSkills(dir)
		if err != nil {
			return err
		}
		for name, candidate := range found {
			candidate.meta.Scope = scope
			merged[name] = candidate
		}
		return nil
	}

	if strings.TrimSpace(globalConfigDir) != "" {
		globalDir, err := resolveDir(globalConfigDir)
		if err != nil {
			return nil, err
		}
		if home, herr := os.UserHomeDir(); herr == nil && isPathInside(globalDir, home) {
			if err := mergeRoot(filepath.Join(home, ".agents", "skills"), ScopeGlobal); err != nil {
				return nil, err
			}
			if err := mergeRoot(filepath.Join(home, ".claude", "skills"), ScopeGlobal); err != nil {
				return nil, err
			}
		}
		if err := mergeRoot(filepath.Join(globalDir, "skills"), ScopeGlobal); err != nil {
			return nil, err
		}
	}

	for _, rel := range []string{
		filepath.Join(".agents", "skills"),
		filepath.Join(".claude", "skills"),
		filepath.Join(".tern", "skills"),
	} {
		if err := mergeRoot(filepath.Join(workDir, rel), ScopeProject); err != nil {
			return nil, err
		}
	}

	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == names[j] {
			return merged[names[i]].meta.Path < merged[names[j]].meta.Path
		}
		return names[i] < names[j]
	})

	skills := make([]SkillMeta, 0, len(names))
	for _, name := range names {
		skills = append(skills, merged[name].meta)
	}
	return skills, nil
}

// LoadSkill finds a skill by name and reads its complete markdown body.
func LoadSkill(workDir, globalConfigDir, name string) (*Skill, error) {
	name = strings.TrimSpace(name)
	metas, err := ListSkills(workDir, globalConfigDir)
	if err != nil {
		return nil, err
	}

	for _, meta := range metas {
		if meta.Name != name {
			continue
		}
		data, err := os.ReadFile(meta.Path)
		if err != nil {
			return nil, fmt.Errorf("context: read skill %s: %w", meta.Path, err)
		}
		fm, body := splitFrontMatter(string(data))
		if fm.Name != "" {
			meta.Name = fm.Name
		}
		if fm.Description != "" {
			meta.Description = fm.Description
		}
		if len(fm.Tags) > 0 {
			meta.Tags = fm.Tags
		}
		return &Skill{SkillMeta: meta, Body: strings.TrimSpace(body)}, nil
	}

	// User skills win; built-ins are only a fallback.
	if skill := builtinSkill(name); skill != nil {
		return skill, nil
	}

	return nil, fmt.Errorf("%w: %q", ErrSkillNotFound, name)
}

// discoverSkills scans a skills directory. A missing directory is not an
// error.
func discoverSkills(dir string) (map[string]skillCandidate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]skillCandidate{}, nil
		}
		return nil, fmt.Errorf("context: read skills directory %s: %w", dir, err)
	}

	found := make(map[string]skillCandidate)
	for _, entry := range entries {
		entryName := entry.Name()
		if strings.HasPrefix(entryName, ".") {
			continue
		}

		var (
			path     string
			priority int
			isDir    bool
		)
		isDirEntry := entry.IsDir()
		if !isDirEntry && entry.Type()&os.ModeSymlink != 0 {
			if st, statErr := os.Stat(filepath.Join(dir, entryName)); statErr == nil && st.IsDir() {
				isDirEntry = true
			}
		}
		if isDirEntry {
			isDir = true
			path = filepath.Join(dir, entryName, "SKILL.md")
			info, err := os.Stat(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("context: stat skill %s: %w", path, err)
			}
			if info.IsDir() {
				continue
			}
			priority = 1
		} else {
			if !strings.EqualFold(filepath.Ext(entryName), ".md") {
				continue
			}
			path = filepath.Join(dir, entryName)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("context: read skill %s: %w", path, err)
		}
		fm, _ := splitFrontMatter(string(data))

		name := strings.TrimSpace(fm.Name)
		if name == "" {
			if isDir {
				name = entryName
			} else {
				name = strings.TrimSuffix(entryName, filepath.Ext(entryName))
			}
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		candidate := skillCandidate{
			meta: SkillMeta{
				Name:        name,
				Description: fm.Description,
				Tags:        fm.Tags,
				Path:        path,
			},
			priority: priority,
		}

		if previous, ok := found[name]; ok {
			// Prefer <name>/SKILL.md over <name>.md. For equally specific
			// layouts, prefer the lexicographically smaller path.
			if candidate.priority < previous.priority {
				continue
			}
			if candidate.priority == previous.priority && candidate.meta.Path > previous.meta.Path {
				continue
			}
		}
		found[name] = candidate
	}

	return found, nil
}
