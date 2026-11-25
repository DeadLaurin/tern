package subagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Loader discovers built-in and custom agent definitions.
//
// Custom agents are markdown files with optional YAML-like frontmatter:
//
//	---
//	name: docs
//	description: Write documentation
//	tools: read, grep, write
//	model: deepseek-flash
//	max_steps: 20
//	permission: ask
//	---
//	You are the documentation sub-agent...
//
// Files are loaded from <globalConfigDir>/agents/*.md and then
// <workDir>/.tern/agents/*.md. Later definitions (project) override earlier
// ones (global, then built-in) with the same name.
type Loader struct {
	GlobalConfigDir string
	WorkDir         string
}

// NewLoader returns a loader rooted at the given config and work directories.
func NewLoader(globalConfigDir, workDir string) *Loader {
	return &Loader{GlobalConfigDir: globalConfigDir, WorkDir: workDir}
}

// Load returns all definitions sorted deterministically by name.
func (l *Loader) Load() ([]Definition, error) {
	byName := make(map[string]Definition)
	for _, def := range BuiltinDefinitions() {
		byName[def.Name] = def
	}

	dirs := make([]string, 0, 2)
	if strings.TrimSpace(l.GlobalConfigDir) != "" {
		dirs = append(dirs, filepath.Join(l.GlobalConfigDir, "agents"))
	}
	if strings.TrimSpace(l.WorkDir) != "" {
		dirs = append(dirs, filepath.Join(l.WorkDir, ".tern", "agents"))
	}

	for _, dir := range dirs {
		defs, err := loadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, def := range defs {
			byName[def.Name] = def
		}
	}

	out := make([]Definition, 0, len(byName))
	for _, def := range byName {
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// List is an alias for Load.
func (l *Loader) List() ([]Definition, error) { return l.Load() }

// Get returns one definition by name.
func (l *Loader) Get(name string) (Definition, bool, error) {
	defs, err := l.Load()
	if err != nil {
		return Definition{}, false, err
	}
	name = strings.TrimSpace(name)
	for _, def := range defs {
		if def.Name == name {
			return def, true, nil
		}
	}
	return Definition{}, false, nil
}

// LoadDefinitions is a package-level convenience wrapper.
func LoadDefinitions(globalConfigDir, workDir string) ([]Definition, error) {
	return NewLoader(globalConfigDir, workDir).Load()
}

func loadDir(dir string) ([]Definition, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("subagent: read agents dir %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	defs := make([]Definition, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		def, err := loadFile(path)
		if err != nil {
			return nil, err
		}
		defs = append(defs, def)
	}
	return defs, nil
}

type agentFrontMatter struct {
	name        string
	description string
	tools       []string
	model       string
	maxSteps    int
	permission  string
}

func loadFile(path string) (Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Definition{}, fmt.Errorf("subagent: read agent %s: %w", path, err)
	}
	fm, body := parseAgentFrontMatter(string(data))

	name := strings.TrimSpace(fm.name)
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if name == "" || strings.ContainsAny(name, " \t/\\") {
		return Definition{}, fmt.Errorf("subagent: invalid agent name %q in %s", name, path)
	}
	body = strings.TrimSpace(body)
	description := strings.TrimSpace(fm.description)
	if description == "" {
		description = firstParagraph(body)
	}
	if description == "" {
		description = "Custom sub-agent " + name
	}
	if body == "" {
		body = "You are the " + name + " sub-agent. " + description
	}

	tools := normalizeToolNames(fm.tools)
	permission := strings.TrimSpace(fm.permission)
	readOnly := permission == "plan"
	if len(tools) == 0 {
		// Custom agents default to the read-only exploration tool set unless
		// they explicitly ask for mutating tools.
		tools = []string{"bash", "glob", "grep", "ls", "read"}
	}
	for _, t := range tools {
		if t == "bash" && permission == "plan" {
			readOnly = true
		}
	}
	return Definition{
		Name:         name,
		Description:  description,
		SystemPrompt: body,
		Tools:        tools,
		Model:        strings.TrimSpace(fm.model),
		MaxSteps:     fm.maxSteps,
		Permission:   permission,
		ReadOnly:     readOnly,
		Builtin:      false,
		Source:       path,
	}, nil
}

func normalizeToolNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, raw := range names {
		for _, part := range strings.Split(raw, ",") {
			t := strings.ToLower(strings.TrimSpace(part))
			if t == "" {
				continue
			}
			if _, ok := seen[t]; ok {
				continue
			}
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// parseAgentFrontMatter understands the small frontmatter dialect used by
// custom agents. It intentionally does not depend on a YAML library.
func parseAgentFrontMatter(content string) (agentFrontMatter, string) {
	content = strings.TrimPrefix(content, "\ufeff")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return agentFrontMatter{}, content
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "---" || trimmed == "..." {
			end = i
			break
		}
	}
	if end < 0 {
		return agentFrontMatter{}, content
	}

	var fm agentFrontMatter
	listKey := ""
	for _, raw := range lines[1:end] {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if listKey != "" && strings.HasPrefix(trimmed, "-") {
			value := cleanYAML(strings.TrimPrefix(trimmed, "-"))
			switch listKey {
			case "tools":
				fm.tools = append(fm.tools, value)
			}
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			listKey = ""
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		listKey = ""
		switch key {
		case "name":
			fm.name = cleanYAML(value)
		case "description":
			fm.description = cleanYAML(value)
		case "tools":
			if value == "" {
				listKey = "tools"
				continue
			}
			fm.tools = append(fm.tools, parseYAMLList(value)...)
		case "model":
			fm.model = cleanYAML(value)
		case "max_steps":
			if n, err := strconv.Atoi(cleanYAML(value)); err == nil {
				fm.maxSteps = n
			}
		case "permission":
			fm.permission = cleanYAML(value)
		}
	}
	return fm, strings.Join(lines[end+1:], "\n")
}

func parseYAMLList(value string) []string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := cleanYAML(part); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func cleanYAML(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		switch {
		case value[0] == '"' && value[len(value)-1] == '"':
			value = value[1 : len(value)-1]
			value = strings.ReplaceAll(value, `\"`, `"`)
			value = strings.ReplaceAll(value, `\\`, `\`)
		case value[0] == '\'' && value[len(value)-1] == '\'':
			value = value[1 : len(value)-1]
			value = strings.ReplaceAll(value, `''`, `'`)
		}
	}
	return strings.TrimSpace(value)
}

func firstParagraph(body string) string {
	for _, para := range strings.Split(strings.TrimSpace(body), "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		line := strings.Split(para, "\n")[0]
		if len(line) > 160 {
			line = line[:160] + "…"
		}
		return strings.TrimSpace(line)
	}
	return ""
}
