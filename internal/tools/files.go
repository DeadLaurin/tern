package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// resolvePath resolves p against env.WorkDir and returns an absolute, cleaned
// path. A leading "~/" is expanded to the user's home directory.
func resolvePath(env ExecEnv, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		p = "."
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if p == "~" {
			p = home
		} else {
			p = filepath.Join(home, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(env.WorkingDir(), p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", p, err)
	}
	return abs, nil
}

// displayPath renders abs relative to the workspace when possible.
func displayPath(env ExecEnv, abs string) string {
	base, err := filepath.Abs(env.WorkingDir())
	if err == nil {
		if rel, err := filepath.Rel(base, abs); err == nil && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return abs
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory, preserving the existing file mode when there is one.
func writeFileAtomic(path string, data []byte) error {
	perm := fs.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".tern-write-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return fmt.Errorf("set file mode: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	cleanup = false
	return nil
}

func looksBinary(data []byte) bool {
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	return bytes.IndexByte(probe, 0) >= 0
}

// splitLines splits text into lines, accepting both LF and CRLF endings and
// excluding a trailing empty line caused by a final newline.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

func fileUsesCRLF(s string) bool {
	return strings.Contains(s, "\r\n")
}

// normalizeEOL converts line endings in s to the file's convention.
func normalizeEOL(s string, crlf bool) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if crlf {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}

// matchGlob matches a slash-separated glob against a slash-separated path.
// "**" matches zero or more path segments; "*" and "?" match within one
// segment.
func matchGlob(pattern, path string) bool {
	p := strings.Split(filepath.ToSlash(pattern), "/")
	n := strings.Split(filepath.ToSlash(path), "/")
	// A leading "./" in either side is not significant.
	for len(p) > 0 && p[0] == "." {
		p = p[1:]
	}
	for len(n) > 0 && n[0] == "." {
		n = n[1:]
	}
	return matchGlobSegments(p, n)
}

func matchGlobSegments(pattern, path []string) bool {
	for {
		if len(pattern) == 0 {
			return len(path) == 0
		}
		if pattern[0] == "**" {
			// Collapse consecutive "**" segments.
			for len(pattern) > 0 && pattern[0] == "**" {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(path); i++ {
				if matchGlobSegments(pattern, path[i:]) {
					return true
				}
			}
			return false
		}
		if len(path) == 0 {
			return false
		}
		ok, err := filepath.Match(pattern[0], path[0])
		if err != nil || !ok {
			return false
		}
		pattern = pattern[1:]
		path = path[1:]
	}
}

var errBeforeEdit = errors.New("edit rejected by checkpoint hook")

// ---------------------------------------------------------------------------
// read
// ---------------------------------------------------------------------------

// ReadTool reads a slice of a text file with line numbers.
type ReadTool struct{}

var readSchema = schema(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, relative to the workspace or absolute."},
    "offset": {"type": "integer", "description": "1-based line number to start reading from.", "minimum": 1},
    "limit": {"type": "integer", "description": "Maximum number of lines to read (default 200).", "minimum": 1}
  },
  "required": ["path"],
  "additionalProperties": false
}`)

// NewReadTool returns the read tool.
func NewReadTool() *ReadTool { return &ReadTool{} }

func (t *ReadTool) Name() string                { return ToolRead }
func (t *ReadTool) IsReadOnly() bool            { return true }
func (t *ReadTool) IsWeb() bool                 { return false }
func (t *ReadTool) IsParallelSafe() bool        { return true }
func (t *ReadTool) Parameters() json.RawMessage { return readSchema }
func (t *ReadTool) Description() string {
	return "Read a text file and return its contents with line numbers. Use offset and limit to page through large files."
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// Execute implements Tool.
func (t *ReadTool) Execute(_ context.Context, env ExecEnv, raw json.RawMessage) (Result, error) {
	var args readArgs
	if err := decodeArgs(raw, &args); err != nil {
		return Result{}, fmt.Errorf("read: invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return Result{}, errors.New(`read: missing required argument "path"`)
	}
	if args.Limit < 0 {
		return Result{}, errors.New("read: limit must be positive")
	}
	abs, err := resolvePath(env, args.Path)
	if err != nil {
		return Result{}, fmt.Errorf("read: %w", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Result{}, fmt.Errorf("read: %w", err)
	}
	display := displayPath(env, abs)
	if looksBinary(data) {
		return Result{}, fmt.Errorf("read: %s appears to be a binary file", display)
	}

	lines := splitLines(string(data))
	total := len(lines)
	limit := args.Limit
	if limit == 0 {
		limit = DefaultReadLimit
	}
	offset := args.Offset
	if offset <= 0 {
		offset = 1
	}
	meta := map[string]any{
		"files_read":  []string{abs},
		"total_lines": total,
	}
	if offset > total {
		return Result{
			Output:   fmt.Sprintf("(%s has %d lines; offset %d is past end of file)", display, total, offset),
			Metadata: meta,
		}, nil
	}
	end := offset - 1 + limit
	if end > total {
		end = total
	}
	var b strings.Builder
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&b, "%d | %s", i+1, lines[i])
		if i+1 < end {
			b.WriteByte('\n')
		}
	}
	return Result{Output: env.Truncate(b.String()), Metadata: meta}, nil
}

// ---------------------------------------------------------------------------
// write
// ---------------------------------------------------------------------------

// WriteTool writes or creates a file.
type WriteTool struct{}

var writeSchema = schema(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, relative to the workspace or absolute."},
    "content": {"type": "string", "description": "Full file content to write."}
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`)

// NewWriteTool returns the write tool.
func NewWriteTool() *WriteTool { return &WriteTool{} }

func (t *WriteTool) Name() string                { return ToolWrite }
func (t *WriteTool) IsReadOnly() bool            { return false }
func (t *WriteTool) IsWeb() bool                 { return false }
func (t *WriteTool) IsParallelSafe() bool        { return false }
func (t *WriteTool) Parameters() json.RawMessage { return writeSchema }
func (t *WriteTool) Description() string {
	return "Write content to a file, creating parent directories as needed. Overwrites the existing file atomically."
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Execute implements Tool.
func (t *WriteTool) Execute(_ context.Context, env ExecEnv, raw json.RawMessage) (Result, error) {
	var args writeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return Result{}, fmt.Errorf("write: invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return Result{}, errors.New(`write: missing required argument "path"`)
	}
	abs, err := resolvePath(env, args.Path)
	if err != nil {
		return Result{}, fmt.Errorf("write: %w", err)
	}
	if env.BeforeEdit != nil {
		if err := env.BeforeEdit(abs); err != nil {
			return Result{}, fmt.Errorf("write: %w: %v", errBeforeEdit, err)
		}
	}
	if err := writeFileAtomic(abs, []byte(args.Content)); err != nil {
		return Result{}, fmt.Errorf("write: %w", err)
	}
	display := displayPath(env, abs)
	return Result{
		Output: fmt.Sprintf("wrote %d bytes to %s", len(args.Content), display),
		Metadata: map[string]any{
			"files_changed": []string{abs},
			"bytes_written": len(args.Content),
		},
	}, nil
}

// ---------------------------------------------------------------------------
// edit
// ---------------------------------------------------------------------------

// EditTool replaces one exact string in a file.
type EditTool struct{}

var editSchema = schema(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, relative to the workspace or absolute."},
    "old_string": {"type": "string", "description": "Exact text to replace. Must match uniquely unless replace_all is true."},
    "new_string": {"type": "string", "description": "Replacement text."},
    "replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring a unique match."}
  },
  "required": ["path", "old_string", "new_string"],
  "additionalProperties": false
}`)

// NewEditTool returns the edit tool.
func NewEditTool() *EditTool { return &EditTool{} }

func (t *EditTool) Name() string                { return ToolEdit }
func (t *EditTool) IsReadOnly() bool            { return false }
func (t *EditTool) IsWeb() bool                 { return false }
func (t *EditTool) IsParallelSafe() bool        { return false }
func (t *EditTool) Parameters() json.RawMessage { return editSchema }
func (t *EditTool) Description() string {
	return "Replace an exact string in a file. Fails when the text is missing or ambiguous unless replace_all is set. CRLF line endings are preserved."
}

type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// Execute implements Tool.
func (t *EditTool) Execute(_ context.Context, env ExecEnv, raw json.RawMessage) (Result, error) {
	var args editArgs
	if err := decodeArgs(raw, &args); err != nil {
		return Result{}, fmt.Errorf("edit: invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return Result{}, errors.New(`edit: missing required argument "path"`)
	}
	if args.OldString == "" {
		return Result{}, errors.New("edit: old_string must not be empty")
	}
	abs, err := resolvePath(env, args.Path)
	if err != nil {
		return Result{}, fmt.Errorf("edit: %w", err)
	}
	original, err := os.ReadFile(abs)
	if err != nil {
		return Result{}, fmt.Errorf("edit: %w", err)
	}
	content := string(original)
	crlf := fileUsesCRLF(content)
	old := normalizeEOL(args.OldString, crlf)
	newText := normalizeEOL(args.NewString, crlf)

	count := strings.Count(content, old)
	display := displayPath(env, abs)
	if count == 0 {
		return Result{}, fmt.Errorf("edit: old_string not found in %s", display)
	}
	if count > 1 && !args.ReplaceAll {
		return Result{}, fmt.Errorf("edit: old_string matches %d times in %s; add more context or set replace_all", count, display)
	}
	replaced := count
	replacements := 1
	if args.ReplaceAll {
		replacements = -1
	}
	updated := strings.Replace(content, old, newText, replacements)
	if updated == content {
		return Result{
			Output: fmt.Sprintf("no changes needed in %s", display),
			Metadata: map[string]any{
				"files_changed": []string{},
				"replacements":  0,
			},
		}, nil
	}
	if env.BeforeEdit != nil {
		if err := env.BeforeEdit(abs); err != nil {
			return Result{}, fmt.Errorf("edit: %w: %v", errBeforeEdit, err)
		}
	}
	if err := writeFileAtomic(abs, []byte(updated)); err != nil {
		return Result{}, fmt.Errorf("edit: %w", err)
	}
	return Result{
		Output: fmt.Sprintf("replaced %d occurrence(s) in %s", replaced, display),
		Metadata: map[string]any{
			"files_changed": []string{abs},
			"replacements":  replaced,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// multi_edit
// ---------------------------------------------------------------------------

// MultiEditTool applies an ordered list of exact replacements to one file.
type MultiEditTool struct{}

var multiEditSchema = schema(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, relative to the workspace or absolute."},
    "edits": {
      "type": "array",
      "description": "Ordered edits applied to the file content in memory before writing.",
      "items": {
        "type": "object",
        "properties": {
          "old_string": {"type": "string"},
          "new_string": {"type": "string"},
          "replace_all": {"type": "boolean"}
        },
        "required": ["old_string", "new_string"],
        "additionalProperties": false
      },
      "minItems": 1
    }
  },
  "required": ["path", "edits"],
  "additionalProperties": false
}`)

// NewMultiEditTool returns the multi_edit tool.
func NewMultiEditTool() *MultiEditTool { return &MultiEditTool{} }

func (t *MultiEditTool) Name() string                { return ToolMultiEdit }
func (t *MultiEditTool) IsReadOnly() bool            { return false }
func (t *MultiEditTool) IsWeb() bool                 { return false }
func (t *MultiEditTool) IsParallelSafe() bool        { return false }
func (t *MultiEditTool) Parameters() json.RawMessage { return multiEditSchema }
func (t *MultiEditTool) Description() string {
	return "Apply several exact string replacements to one file atomically. If any edit fails, the file is left untouched."
}

type multiEditArgs struct {
	Path  string `json:"path"`
	Edits []struct {
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	} `json:"edits"`
}

// Execute implements Tool.
func (t *MultiEditTool) Execute(_ context.Context, env ExecEnv, raw json.RawMessage) (Result, error) {
	var args multiEditArgs
	if err := decodeArgs(raw, &args); err != nil {
		return Result{}, fmt.Errorf("multi_edit: invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return Result{}, errors.New(`multi_edit: missing required argument "path"`)
	}
	if len(args.Edits) == 0 {
		return Result{}, errors.New("multi_edit: edits must contain at least one edit")
	}
	abs, err := resolvePath(env, args.Path)
	if err != nil {
		return Result{}, fmt.Errorf("multi_edit: %w", err)
	}
	original, err := os.ReadFile(abs)
	if err != nil {
		return Result{}, fmt.Errorf("multi_edit: %w", err)
	}
	content := string(original)
	crlf := fileUsesCRLF(content)
	display := displayPath(env, abs)
	totalReplacements := 0

	for i, edit := range args.Edits {
		if edit.OldString == "" {
			return Result{}, fmt.Errorf("multi_edit: edit %d: old_string must not be empty", i+1)
		}
		old := normalizeEOL(edit.OldString, crlf)
		newText := normalizeEOL(edit.NewString, crlf)
		count := strings.Count(content, old)
		if count == 0 {
			return Result{}, fmt.Errorf("multi_edit: edit %d: old_string not found in %s; file was not modified", i+1, display)
		}
		if count > 1 && !edit.ReplaceAll {
			return Result{}, fmt.Errorf("multi_edit: edit %d: old_string matches %d times in %s; add more context or set replace_all; file was not modified", i+1, count, display)
		}
		replacements := 1
		if edit.ReplaceAll {
			replacements = -1
			totalReplacements += count
		} else {
			totalReplacements++
		}
		content = strings.Replace(content, old, newText, replacements)
	}

	if content == string(original) {
		return Result{
			Output: fmt.Sprintf("no changes needed in %s", display),
			Metadata: map[string]any{
				"files_changed": []string{},
				"replacements":  0,
			},
		}, nil
	}
	if env.BeforeEdit != nil {
		if err := env.BeforeEdit(abs); err != nil {
			return Result{}, fmt.Errorf("multi_edit: %w: %v", errBeforeEdit, err)
		}
	}
	if err := writeFileAtomic(abs, []byte(content)); err != nil {
		return Result{}, fmt.Errorf("multi_edit: %w", err)
	}
	return Result{
		Output: fmt.Sprintf("applied %d edit(s) to %s", len(args.Edits), display),
		Metadata: map[string]any{
			"files_changed": []string{abs},
			"replacements":  totalReplacements,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// ls
// ---------------------------------------------------------------------------

// LSTool lists a directory.
type LSTool struct{}

var lsSchema = schema(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Directory (or file) to list. Defaults to the workspace root."},
    "all": {"type": "boolean", "description": "Include hidden entries (names starting with a dot)."},
    "include_git": {"type": "boolean", "description": "Include the .git directory (skipped by default)."}
  },
  "additionalProperties": false
}`)

// NewLSTool returns the ls tool.
func NewLSTool() *LSTool { return &LSTool{} }

func (t *LSTool) Name() string                { return ToolLS }
func (t *LSTool) IsReadOnly() bool            { return true }
func (t *LSTool) IsWeb() bool                 { return false }
func (t *LSTool) IsParallelSafe() bool        { return true }
func (t *LSTool) Parameters() json.RawMessage { return lsSchema }
func (t *LSTool) Description() string {
	return "List directory entries sorted by name. Directories end with '/', files show their size. Hidden files and .git are skipped unless requested."
}

type lsArgs struct {
	Path       string `json:"path"`
	All        bool   `json:"all"`
	IncludeGit bool   `json:"include_git"`
}

// Execute implements Tool.
func (t *LSTool) Execute(_ context.Context, env ExecEnv, raw json.RawMessage) (Result, error) {
	var args lsArgs
	if err := decodeArgs(raw, &args); err != nil {
		return Result{}, fmt.Errorf("ls: invalid arguments: %w", err)
	}
	abs, err := resolvePath(env, args.Path)
	if err != nil {
		return Result{}, fmt.Errorf("ls: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Result{}, fmt.Errorf("ls: %w", err)
	}
	display := displayPath(env, abs)
	if !info.IsDir() {
		return Result{
			Output: fmt.Sprintf("%s  %dB", display, info.Size()),
			Metadata: map[string]any{
				"path":    abs,
				"entries": 1,
			},
		}, nil
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return Result{}, fmt.Errorf("ls: %w", err)
	}
	type row struct {
		name  string
		isDir bool
		size  int64
	}
	rows := make([]row, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" && !args.IncludeGit {
			continue
		}
		if !args.All && strings.HasPrefix(name, ".") {
			continue
		}
		r := row{name: name, isDir: entry.IsDir()}
		if info, err := entry.Info(); err == nil {
			r.size = info.Size()
			if info.IsDir() {
				r.isDir = true
			}
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		if r.isDir {
			b.WriteString(r.name)
			b.WriteByte('/')
			continue
		}
		fmt.Fprintf(&b, "%s  %dB", r.name, r.size)
	}
	return Result{
		Output: env.Truncate(b.String()),
		Metadata: map[string]any{
			"path":    abs,
			"entries": len(rows),
		},
	}, nil
}

// ---------------------------------------------------------------------------
// glob
// ---------------------------------------------------------------------------

// GlobTool matches paths recursively.
type GlobTool struct{}

var globSchema = schema(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Glob pattern. '**' matches zero or more directories, '*' and '?' match within a path segment."},
    "path": {"type": "string", "description": "Directory to search under. Defaults to the workspace root."}
  },
  "required": ["pattern"],
  "additionalProperties": false
}`)

// NewGlobTool returns the glob tool.
func NewGlobTool() *GlobTool { return &GlobTool{} }

func (t *GlobTool) Name() string                { return ToolGlob }
func (t *GlobTool) IsReadOnly() bool            { return true }
func (t *GlobTool) IsWeb() bool                 { return false }
func (t *GlobTool) IsParallelSafe() bool        { return true }
func (t *GlobTool) Parameters() json.RawMessage { return globSchema }
func (t *GlobTool) Description() string {
	return "Find files by glob pattern under the workspace. Supports '**', '*' and '?', skips .git and node_modules, and returns sorted matches."
}

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

// Execute implements Tool.
func (t *GlobTool) Execute(_ context.Context, env ExecEnv, raw json.RawMessage) (Result, error) {
	var args globArgs
	if err := decodeArgs(raw, &args); err != nil {
		return Result{}, fmt.Errorf("glob: invalid arguments: %w", err)
	}
	pattern := strings.TrimSpace(filepath.ToSlash(args.Pattern))
	if pattern == "" {
		return Result{}, errors.New(`glob: missing required argument "pattern"`)
	}
	dirsOnly := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimSuffix(pattern, "/")
	if pattern == "" {
		return Result{}, errors.New("glob: pattern must not be empty")
	}
	base, err := resolvePath(env, args.Path)
	if err != nil {
		return Result{}, fmt.Errorf("glob: %w", err)
	}
	info, err := os.Stat(base)
	if err != nil {
		return Result{}, fmt.Errorf("glob: %w", err)
	}

	var matches []string
	truncated := false
	if !info.IsDir() {
		if matchGlob(pattern, filepath.Base(base)) && (!dirsOnly || info.IsDir()) {
			matches = append(matches, displayPath(env, base))
		}
	} else {
		walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if p != base {
					switch d.Name() {
					case ".git", "node_modules":
						return fs.SkipDir
					}
				}
				if p == base {
					return nil
				}
			}
			rel, err := filepath.Rel(base, p)
			if err != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			if !matchGlob(pattern, relSlash) {
				return nil
			}
			if dirsOnly && !d.IsDir() {
				return nil
			}
			if len(matches) >= MaxGlobMatches {
				truncated = true
				return fs.SkipAll
			}
			matches = append(matches, displayPath(env, p))
			if d.IsDir() {
				matches[len(matches)-1] += "/"
			}
			return nil
		})
		if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
			return Result{}, fmt.Errorf("glob: %w", walkErr)
		}
	}
	sort.Strings(matches)

	meta := map[string]any{
		"matches":   len(matches),
		"truncated": truncated,
	}
	if len(matches) == 0 {
		return Result{Output: fmt.Sprintf("no files matched pattern %q", args.Pattern), Metadata: meta}, nil
	}
	out := strings.Join(matches, "\n")
	if truncated {
		out += fmt.Sprintf("\n[... glob capped at %d matches ...]", MaxGlobMatches)
	}
	return Result{Output: env.Truncate(out), Metadata: meta}, nil
}

// ---------------------------------------------------------------------------
// grep
// ---------------------------------------------------------------------------

// GrepTool searches file contents with a regular expression.
type GrepTool struct {
	forcePureGo bool
}

var grepSchema = schema(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Go/ripgrep regular expression to search for."},
    "path": {"type": "string", "description": "File or directory to search. Defaults to the workspace root."},
    "glob": {"type": "string", "description": "Only search files matching this glob, for example '*.go' or 'src/**/*.ts'."},
    "ignore_case": {"type": "boolean", "description": "Case-insensitive matching."},
    "max_results": {"type": "integer", "description": "Maximum number of matching lines to return (default 200).", "minimum": 1}
  },
  "required": ["pattern"],
  "additionalProperties": false
}`)

// NewGrepTool returns the grep tool. When forcePureGo is true the tool always
// uses the built-in Go implementation instead of ripgrep, which is useful for
// tests and hosts without rg.
func NewGrepTool(forcePureGo bool) *GrepTool { return &GrepTool{forcePureGo: forcePureGo} }

func (t *GrepTool) Name() string                { return ToolGrep }
func (t *GrepTool) IsReadOnly() bool            { return true }
func (t *GrepTool) IsWeb() bool                 { return false }
func (t *GrepTool) IsParallelSafe() bool        { return true }
func (t *GrepTool) Parameters() json.RawMessage { return grepSchema }
func (t *GrepTool) Description() string {
	return "Search files for a regular expression, returning file:line:content matches. Uses ripgrep when available and a built-in fallback otherwise."
}

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignore_case"`
	MaxResults int    `json:"max_results"`
}

// Execute implements Tool.
func (t *GrepTool) Execute(ctx context.Context, env ExecEnv, raw json.RawMessage) (Result, error) {
	var args grepArgs
	if err := decodeArgs(raw, &args); err != nil {
		return Result{}, fmt.Errorf("grep: invalid arguments: %w", err)
	}
	if args.Pattern == "" {
		return Result{}, errors.New(`grep: missing required argument "pattern"`)
	}
	if args.MaxResults < 0 {
		return Result{}, errors.New("grep: max_results must be positive")
	}
	maxResults := args.MaxResults
	if maxResults == 0 {
		maxResults = DefaultGrepMaxResults
	}
	base, err := resolvePath(env, args.Path)
	if err != nil {
		return Result{}, fmt.Errorf("grep: %w", err)
	}

	var (
		lines []string
		used  string
	)
	if !t.forcePureGo {
		if rgPath, err := exec.LookPath("rg"); err == nil {
			lines, err = grepWithRipgrep(ctx, rgPath, base, args, maxResults)
			if err != nil {
				return Result{}, fmt.Errorf("grep: %w", err)
			}
			used = "ripgrep"
		}
	}
	if used == "" {
		lines, err = grepPureGo(ctx, base, args, maxResults)
		if err != nil {
			return Result{}, fmt.Errorf("grep: %w", err)
		}
		used = "go"
	}

	meta := map[string]any{
		"matches":     len(lines),
		"max_results": maxResults,
		"engine":      used,
	}
	if len(lines) == 0 {
		return Result{Output: fmt.Sprintf("no matches for pattern %q", args.Pattern), Metadata: meta}, nil
	}
	out := strings.Join(lines, "\n")
	if len(lines) >= maxResults {
		out += fmt.Sprintf("\n[... limited to %d results ...]", maxResults)
	}
	return Result{Output: env.Truncate(out), Metadata: meta}, nil
}

func grepWithRipgrep(ctx context.Context, rgPath, base string, args grepArgs, maxResults int) ([]string, error) {
	target := base
	dir := base
	info, err := os.Stat(base)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		dir = filepath.Dir(base)
		target = filepath.Base(base)
	}
	cmdArgs := []string{"--line-number", "--no-heading", "--color", "never", "--no-messages"}
	if args.IgnoreCase {
		cmdArgs = append(cmdArgs, "--ignore-case")
	}
	if args.Glob != "" {
		cmdArgs = append(cmdArgs, "--glob", args.Glob)
	}
	// "-e" protects patterns that start with a dash, "--" ends option parsing.
	cmdArgs = append(cmdArgs, "-e", args.Pattern, "--", target)

	cmd := exec.CommandContext(ctx, rgPath, cmdArgs...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(out) == 0 {
			return nil, nil // no matches
		}
		return nil, fmt.Errorf("ripgrep: %w", err)
	}
	return collectLines(string(out), maxResults), nil
}

func collectLines(out string, maxResults int) []string {
	raw := strings.Split(out, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSuffix(line, "\r")
		line = strings.TrimPrefix(line, "./")
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if len(lines) >= maxResults {
			break
		}
	}
	return lines
}

// grepPureGo is the portable fallback used when ripgrep is not installed.
func grepPureGo(ctx context.Context, base string, args grepArgs, maxResults int) ([]string, error) {
	pattern := args.Pattern
	if args.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}
	baseInfo, err := os.Stat(base)
	if err != nil {
		return nil, err
	}

	var files []string
	if !baseInfo.IsDir() {
		files = append(files, base)
	} else {
		err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if p != base {
					switch d.Name() {
					case ".git", "node_modules":
						return fs.SkipDir
					}
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(base, p)
			if err != nil {
				return nil
			}
			if !grepGlobMatch(args.Glob, filepath.ToSlash(rel)) {
				return nil
			}
			files = append(files, p)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	results := make([]string, 0, maxResults)
	for _, file := range files {
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		info, err := os.Stat(file)
		if err != nil || info.Size() > 2<<20 {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil || looksBinary(data) {
			continue
		}
		rel, err := filepath.Rel(base, file)
		if err != nil {
			rel = file
		}
		display := filepath.ToSlash(rel)
		if !baseInfo.IsDir() {
			display = filepath.Base(file)
		}
		for i, line := range splitLines(string(data)) {
			if !re.MatchString(line) {
				continue
			}
			results = append(results, fmt.Sprintf("%s:%d:%s", display, i+1, line))
			if len(results) >= maxResults {
				return results, nil
			}
		}
	}
	return results, nil
}

func grepGlobMatch(glob, rel string) bool {
	if glob == "" {
		return true
	}
	glob = filepath.ToSlash(glob)
	if strings.Contains(glob, "/") {
		return matchGlob(glob, rel)
	}
	return matchGlob(glob, filepath.Base(rel))
}
