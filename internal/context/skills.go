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
