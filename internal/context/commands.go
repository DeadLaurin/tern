package context

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrCommandNotFound is returned by LoadCommand when no command with the
// requested name exists.
var ErrCommandNotFound = errors.New("context: command not found")

// SlashCommand is a custom markdown prompt template discovered in the global
// or project commands directory.
type SlashCommand struct {
	Name        string
	Description string
	// Path is the absolute path of the markdown file.
	Path string
	// Scope is ScopeGlobal or ScopeProject.
	Scope string
	// Template is the markdown body, including argument placeholders.
	Template string
}

// Command is an alias for SlashCommand.
type Command = SlashCommand

// LoadCommands discovers commands in the global and project command
// directories. Project commands override global commands with the same name.
// The result is sorted by name.
func LoadCommands(workDir, globalConfigDir string) ([]SlashCommand, error) {
	workDir, err := resolveDir(workDir)
	if err != nil {
		return nil, err
	}

	merged := make(map[string]SlashCommand)

	if strings.TrimSpace(globalConfigDir) != "" {
		globalDir, err := resolveDir(globalConfigDir)
		if err != nil {
			return nil, err
		}
		globalCommands, err := discoverCommands(filepath.Join(globalDir, "commands"))
		if err != nil {
			return nil, err
		}
		for name, command := range globalCommands {
			command.Scope = ScopeGlobal
			merged[name] = command
		}
	}

	projectCommands, err := discoverCommands(filepath.Join(workDir, ".tern", "commands"))
	if err != nil {
		return nil, err
	}
	for name, command := range projectCommands {
		command.Scope = ScopeProject
		merged[name] = command
	}

	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == names[j] {
			return merged[names[i]].Path < merged[names[j]].Path
		}
		return names[i] < names[j]
	})

	commands := make([]SlashCommand, 0, len(names))
	for _, name := range names {
		commands = append(commands, merged[name])
	}
	return commands, nil
}

// LoadCommand finds and returns a single command by name.
func LoadCommand(workDir, globalConfigDir, name string) (*SlashCommand, error) {
	name = strings.TrimSpace(name)
	commands, err := LoadCommands(workDir, globalConfigDir)
	if err != nil {
		return nil, err
	}
	for i := range commands {
		if commands[i].Name == name {
			return &commands[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrCommandNotFound, name)
}

// discoverCommands scans a commands directory. A missing directory is not an
// error.
func discoverCommands(dir string) (map[string]SlashCommand, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]SlashCommand{}, nil
		}
		return nil, fmt.Errorf("context: read commands directory %s: %w", dir, err)
	}

	found := make(map[string]SlashCommand)
	for _, entry := range entries {
		entryName := entry.Name()
		if entry.IsDir() || strings.HasPrefix(entryName, ".") {
			continue
		}
		if !strings.EqualFold(filepath.Ext(entryName), ".md") {
			continue
		}

		path := filepath.Join(dir, entryName)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("context: read command %s: %w", path, err)
		}
		fm, body := splitFrontMatter(string(data))

		name := strings.TrimSpace(fm.Name)
		if name == "" {
			name = strings.TrimSuffix(entryName, filepath.Ext(entryName))
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		command := SlashCommand{
			Name:        name,
			Description: fm.Description,
			Path:        path,
			Template:    strings.TrimSpace(body),
		}
		if previous, ok := found[name]; ok && previous.Path < command.Path {
			// Deterministic tie-break if two files declare the same name.
			continue
		}
		found[name] = command
	}

	return found, nil
}

// Expand substitutes command placeholders in the template:
//
//   - $ARGUMENTS is replaced with the complete argument string.
//   - $1, $2, ... are replaced with whitespace-separated positional arguments.
//     Single and double quotes group words containing spaces.
//
// Missing positional arguments expand to the empty string. Placeholders in the
// argument text itself are never re-expanded.
func (c SlashCommand) Expand(args string) string {
	words := splitArgs(args)

	var b strings.Builder
	for i := 0; i < len(c.Template); {
		if c.Template[i] != '$' {
			b.WriteByte(c.Template[i])
			i++
			continue
		}

		if strings.HasPrefix(c.Template[i:], "$ARGUMENTS") {
			b.WriteString(args)
			i += len("$ARGUMENTS")
			continue
		}

		j := i + 1
		for j < len(c.Template) && c.Template[j] >= '0' && c.Template[j] <= '9' {
			j++
		}
		if j == i+1 {
			b.WriteByte('$')
			i++
			continue
		}

		index, err := strconv.Atoi(c.Template[i+1 : j])
		if err == nil && index >= 1 && index <= len(words) {
			b.WriteString(words[index-1])
		}
		i = j
	}

	return b.String()
}

// splitArgs splits an argument string into words, honouring single and double
// quotes and backslash escapes. Empty quoted words are preserved.
func splitArgs(input string) []string {
	var (
		args    []string
		current strings.Builder
		started bool

		inSingle bool
		inDouble bool
		escaped  bool
	)

	flush := func() {
		if !started {
			return
		}
		args = append(args, current.String())
		current.Reset()
		started = false
	}

	for _, r := range input {
		if escaped {
			current.WriteRune(r)
			started = true
			escaped = false
			continue
		}

		switch {
		case r == '\\' && !inSingle:
			escaped = true
			started = true
		case inSingle:
			if r == '\'' {
				inSingle = false
			} else {
				current.WriteRune(r)
			}
		case inDouble:
			if r == '"' {
				inDouble = false
			} else {
				current.WriteRune(r)
			}
		case r == '\'':
			inSingle = true
			started = true
		case r == '"':
			inDouble = true
			started = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			current.WriteRune(r)
			started = true
		}
	}

	if escaped {
		// A trailing backslash is literal rather than an escape.
		current.WriteRune('\\')
		started = true
	}
	flush()

	return args
}
