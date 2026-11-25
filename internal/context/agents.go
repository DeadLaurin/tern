// Package context loads the contextual material injected into a tern session:
// hierarchical AGENTS.md / TERN.md instruction files, on-demand skills, custom
// slash commands, and the small dynamic environment block used in the first
// user message.
//
// Instruction files and the dynamic environment block are intentionally kept
// separate. The system prompt must remain byte-stable so that DeepSeek prefix
// caching keeps working across turns; volatile data such as the date, git
// branch, or command output belongs in the first user message instead.
package context

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Scope values identify where a context file came from.
const (
	ScopeGlobal  = "global"
	ScopeProject = "project"
)

// Instruction file names recognised at every directory level, in the order in
// which they are included.
var instructionFileNames = []string{"AGENTS.md", "TERN.md"}

// InstructionFile is one loaded AGENTS.md or TERN.md file.
type InstructionFile struct {
	// Path is the absolute path of the file.
	Path string
	// Name is the base file name (AGENTS.md or TERN.md).
	Name string
	// Dir is the absolute directory containing the file.
	Dir string
	// Scope is ScopeGlobal or ScopeProject.
	Scope string
	// Content is the raw file contents.
	Content string
}

// LoadProjectInstructions loads global instructions and then every AGENTS.md /
// TERN.md file between the git root and workDir.
//
// Files are returned outermost-first: global config, then the repository root,
// then each nested directory down to workDir. Later files therefore refine or
// override earlier ones when rendered by FormatInstructions.
func LoadProjectInstructions(workDir, globalConfigDir string) ([]InstructionFile, error) {
	workDir, err := resolveDir(workDir)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(workDir)
	if err != nil {
		return nil, fmt.Errorf("context: stat work directory %s: %w", workDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("context: work directory %s is not a directory", workDir)
	}

	var files []InstructionFile
	seen := make(map[string]struct{})

	appendFile := func(path, scope string) error {
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("context: stat instruction file %s: %w", path, err)
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("context: read instruction file %s: %w", path, err)
		}
		seen[path] = struct{}{}
		files = append(files, InstructionFile{
			Path:    path,
			Name:    filepath.Base(path),
			Dir:     filepath.Dir(path),
			Scope:   scope,
			Content: string(data),
		})
		return nil
	}

	if strings.TrimSpace(globalConfigDir) != "" {
		globalDir, err := resolveDir(globalConfigDir)
		if err == nil {
			for _, name := range instructionFileNames {
				if err := appendFile(filepath.Join(globalDir, name), ScopeGlobal); err != nil {
					return nil, err
				}
			}
		}
	}

	for _, dir := range ancestorDirs(workDir) {
		for _, name := range instructionFileNames {
			if err := appendFile(filepath.Join(dir, name), ScopeProject); err != nil {
				return nil, err
			}
		}
	}

	return files, nil
}

// FormatInstructions renders instruction files for injection into the first
// user message. The supplied order is preserved (outermost first) so that the
// repository's own hierarchy is not flattened.
func FormatInstructions(files []InstructionFile) string {
	type entry struct {
		label   string
		content string
	}

	entries := make([]entry, 0, len(files))
	for _, file := range files {
		content := strings.TrimSpace(file.Content)
		if content == "" {
			continue
		}

		label := file.Path
		if label == "" {
			label = file.Name
		}
		if file.Scope != "" {
			label += " (" + file.Scope + ")"
		}
		entries = append(entries, entry{label: label, content: content})
	}
	if len(entries) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("# Project Instructions\n\n")
	b.WriteString("These instruction files are listed from outermost to innermost. ")
	b.WriteString("Later files refine and take precedence over earlier ones when they conflict.\n")

	for _, item := range entries {
		b.WriteString("\n## ")
		b.WriteString(item.label)
		b.WriteString("\n\n")
		b.WriteString(item.content)
		b.WriteString("\n")
	}

	return b.String()
}

// ancestorDirs returns workDir and its ancestors up to and including the git
// root, or the filesystem root when workDir is not inside a git repository.
// The result is ordered root-to-leaf.
func ancestorDirs(workDir string) []string {
	var dirs []string
	dir := filepath.Clean(workDir)
	for {
		dirs = append(dirs, dir)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	return dirs
}

// resolveDir returns an absolute, cleaned directory path, defaulting to the
// current working directory when dir is empty.
func resolveDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("context: determine working directory: %w", err)
		}
		dir = wd
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("context: resolve path %s: %w", dir, err)
	}
	return filepath.Clean(abs), nil
}
