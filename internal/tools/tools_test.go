package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func testEnv(t *testing.T, dir string) ExecEnv {
	t.Helper()
	return ExecEnv{WorkDir: dir, OutputCap: 1 << 20}
}

func run(t *testing.T, tool Tool, env ExecEnv, args any) (Result, error) {
	t.Helper()
	var raw json.RawMessage
	switch v := args.(type) {
	case json.RawMessage:
		raw = v
	case string:
		raw = json.RawMessage(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		raw = b
	}
	return tool.Execute(context.Background(), env, raw)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

// ---------------------------------------------------------------------------
// registry / truncation
// ---------------------------------------------------------------------------

func TestTruncateOnce(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		max      int
		want     string
		contains string
	}{
		{name: "short unchanged", input: "hello", max: 10, want: "hello"},
		{name: "exact unchanged", input: "12345", max: 5, want: "12345"},
		{name: "zero max unchanged", input: "hello", max: 0, want: "hello"},
		{name: "negative max unchanged", input: "hello", max: -1, want: "hello"},
		{name: "truncated", input: "abcdefghij", max: 4, want: "abcd\n[... truncated: showing 4 of 10 chars ...]"},
		{name: "multibyte boundary", input: "héllo wörld", max: 5, contains: "hél"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateOnce(tt.input, tt.max)
			if tt.want != "" && got != tt.want {
				t.Fatalf("TruncateOnce(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
			}
			if tt.contains != "" && !strings.Contains(got, tt.contains) {
				t.Fatalf("TruncateOnce(%q, %d) = %q, want substring %q", tt.input, tt.max, got, tt.contains)
			}
		})
	}
}

func TestRegistryToolDefsSortedAndStrict(t *testing.T) {
	reg := NewEmptyRegistry()
	// Deliberately non-alphabetical registration order.
	reg.Register(NewWriteTool())
	reg.Register(NewGlobTool())
	reg.Register(NewReadTool())
	reg.Register(NewLSTool())

	defs := reg.ToolDefs()
	got := make([]string, len(defs))
	for i, def := range defs {
		got[i] = def.Name
	}
	want := []string{ToolGlob, ToolLS, ToolRead, ToolWrite}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ToolDefs order = %v, want %v", got, want)
	}
	// Repeated calls must be stable.
	again := reg.ToolDefs()
	for i := range defs {
		if defs[i].Name != again[i].Name {
			t.Fatalf("ToolDefs is not deterministic at %d: %q vs %q", i, defs[i].Name, again[i].Name)
		}
	}
	for _, def := range defs {
		var params map[string]any
		if err := json.Unmarshal(def.Parameters, &params); err != nil {
			t.Fatalf("%s: invalid schema JSON: %v", def.Name, err)
		}
		if params["type"] != "object" {
			t.Fatalf("%s: schema type = %v, want object", def.Name, params["type"])
		}
		if params["additionalProperties"] != false {
			t.Fatalf("%s: additionalProperties must be false", def.Name)
		}
		if _, ok := params["properties"].(map[string]any); !ok {
			t.Fatalf("%s: schema properties missing", def.Name)
		}
		if def.Description == "" {
			t.Fatalf("%s: empty description", def.Name)
		}
	}
}

func TestRegistrySubsetWithoutAndGet(t *testing.T) {
	reg := NewEmptyRegistry()
	reg.Register(NewReadTool())
	reg.Register(NewWriteTool())
	reg.Register(NewLSTool())

	if _, ok := reg.Get(ToolRead); !ok {
		t.Fatal("Get(read) failed")
	}
	if _, ok := reg.Get("nope"); ok {
		t.Fatal("Get(nope) unexpectedly succeeded")
	}

	sub := reg.Subset([]string{ToolRead, "nope", ToolLS})
	if names := sub.Names(); strings.Join(names, ",") != ToolLS+","+ToolRead {
		t.Fatalf("Subset names = %v", names)
	}
	if _, ok := sub.Get(ToolWrite); ok {
		t.Fatal("Subset unexpectedly contains write")
	}

	without := reg.Without(ToolRead)
	if _, ok := without.Get(ToolRead); ok {
		t.Fatal("Without(read) still contains read")
	}
	if _, ok := without.Get(ToolWrite); !ok {
		t.Fatal("Without(read) dropped write")
	}
}

func TestNewRegistryClosesCleanly(t *testing.T) {
	reg := NewRegistry()
	if len(reg.Names()) != len(BuiltinTools()) {
		t.Fatalf("registry has %d tools, want %d", len(reg.Names()), len(BuiltinTools()))
	}
	if err := reg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := reg.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// read
// ---------------------------------------------------------------------------

func TestRead(t *testing.T) {
	dir := t.TempDir()
	env := testEnv(t, dir)
	writeFile(t, filepath.Join(dir, "lines.txt"), "alpha\nbeta\ngamma\ndelta\n")
	writeFile(t, filepath.Join(dir, "crlf.txt"), "one\r\ntwo\r\nthree\r\n")
	if err := os.WriteFile(filepath.Join(dir, "binary.bin"), []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	var many strings.Builder
	for i := 1; i <= 250; i++ {
		if i > 1 {
			many.WriteByte('\n')
		}
		fmt.Fprintf(&many, "line%d", i)
	}
	writeFile(t, filepath.Join(dir, "many.txt"), many.String())

	tool := NewReadTool()
	tests := []struct {
		name     string
		args     any
		want     string
		contains string
		wantErr  string
		lines    int
	}{
		{name: "whole file", args: map[string]any{"path": "lines.txt"}, want: "1 | alpha\n2 | beta\n3 | gamma\n4 | delta"},
		{name: "range", args: map[string]any{"path": "lines.txt", "offset": 2, "limit": 2}, want: "2 | beta\n3 | gamma"},
		{name: "last line", args: map[string]any{"path": "lines.txt", "offset": 4, "limit": 10}, want: "4 | delta"},
		{name: "crlf", args: map[string]any{"path": "crlf.txt"}, want: "1 | one\n2 | two\n3 | three"},
		{name: "eof", args: map[string]any{"path": "lines.txt", "offset": 99}, contains: "has 4 lines", lines: 1},
		{name: "binary", args: map[string]any{"path": "binary.bin"}, wantErr: "binary"},
		{name: "missing arg", args: map[string]any{}, wantErr: "path"},
		{name: "unknown arg", args: map[string]any{"path": "lines.txt", "bogus": true}, wantErr: "unknown field"},
		{name: "missing file", args: map[string]any{"path": "nope.txt"}, wantErr: "no such file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := run(t, tool, env, tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.want != "" && res.Output != tt.want {
				t.Fatalf("output = %q, want %q", res.Output, tt.want)
			}
			if tt.contains != "" && !strings.Contains(res.Output, tt.contains) {
				t.Fatalf("output = %q, want substring %q", res.Output, tt.contains)
			}
			if tt.lines > 0 && countLines(res.Output) != tt.lines {
				t.Fatalf("output has %d lines, want %d: %q", countLines(res.Output), tt.lines, res.Output)
			}
		})
	}

	t.Run("default limit", func(t *testing.T) {
		res, err := run(t, tool, env, map[string]any{"path": "many.txt"})
		if err != nil {
			t.Fatal(err)
		}
		if got := countLines(res.Output); got != DefaultReadLimit {
			t.Fatalf("default read returned %d lines, want %d", got, DefaultReadLimit)
		}
		if !strings.HasPrefix(res.Output, "1 | line1") {
			t.Fatalf("unexpected first line: %q", res.Output[:20])
		}
	})

	t.Run("metadata", func(t *testing.T) {
		res, err := run(t, tool, env, map[string]any{"path": "lines.txt"})
		if err != nil {
			t.Fatal(err)
		}
		files, ok := res.Metadata["files_read"].([]string)
		if !ok || len(files) != 1 || files[0] != filepath.Join(dir, "lines.txt") {
			t.Fatalf("files_read metadata = %#v", res.Metadata["files_read"])
		}
	})
}

// ---------------------------------------------------------------------------
// write / edit / multi_edit
// ---------------------------------------------------------------------------

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	env := testEnv(t, dir)
	var edited []string
	var mu sync.Mutex
	env.BeforeEdit = func(path string) error {
		mu.Lock()
		defer mu.Unlock()
		edited = append(edited, path)
		return nil
	}
	tool := NewWriteTool()
	res, err := run(t, tool, env, map[string]any{"path": "nested/dir/file.txt", "content": "hello\nworld\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "wrote") {
		t.Fatalf("unexpected output: %q", res.Output)
	}
	if got := readFile(t, filepath.Join(dir, "nested/dir/file.txt")); got != "hello\nworld\n" {
		t.Fatalf("file content = %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(edited) != 1 || !filepath.IsAbs(edited[0]) {
		t.Fatalf("BeforeEdit calls = %#v", edited)
	}
}

func TestEdit(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		args     map[string]any
		want     string
		wantErr  string
		hookErr  bool
		wantHook bool
	}{
		{
			name:     "exact replace",
			content:  "hello world",
			args:     map[string]any{"path": "f.txt", "old_string": "world", "new_string": "there"},
			want:     "hello there",
			wantHook: true,
		},
		{
			name:    "not found",
			content: "hello world",
			args:    map[string]any{"path": "f.txt", "old_string": "missing", "new_string": "x"},
			want:    "hello world",
			wantErr: "not found",
		},
		{
			name:    "non unique",
			content: "aaa",
			args:    map[string]any{"path": "f.txt", "old_string": "a", "new_string": "b"},
			want:    "aaa",
			wantErr: "matches 3 times",
		},
		{
			name:     "replace all",
			content:  "a a a",
			args:     map[string]any{"path": "f.txt", "old_string": "a", "new_string": "bb", "replace_all": true},
			want:     "bb bb bb",
			wantHook: true,
		},
		{
			name:     "crlf preserved",
			content:  "one\r\ntwo\r\nthree\r\n",
			args:     map[string]any{"path": "f.txt", "old_string": "two", "new_string": "TWO"},
			want:     "one\r\nTWO\r\nthree\r\n",
			wantHook: true,
		},
		{
			name:     "crlf multiline",
			content:  "one\r\ntwo\r\nthree\r\n",
			args:     map[string]any{"path": "f.txt", "old_string": "one\ntwo\n", "new_string": "uno\ndos\n"},
			want:     "uno\r\ndos\r\nthree\r\n",
			wantHook: true,
		},
		{
			name:    "empty old string",
			content: "abc",
			args:    map[string]any{"path": "f.txt", "old_string": "", "new_string": "x"},
			want:    "abc",
			wantErr: "must not be empty",
		},
		{
			name:    "hook aborts",
			content: "abc",
			args:    map[string]any{"path": "f.txt", "old_string": "abc", "new_string": "xyz"},
			want:    "abc",
			hookErr: true,
			wantErr: "rejected",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "f.txt")
			writeFile(t, path, tt.content)
			env := testEnv(t, dir)
			hookCalls := 0
			env.BeforeEdit = func(string) error {
				hookCalls++
				if tt.hookErr {
					return os.ErrPermission
				}
				return nil
			}
			_, err := run(t, NewEditTool(), env, tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := readFile(t, path); got != tt.want {
				t.Fatalf("content = %q, want %q", got, tt.want)
			}
			if tt.hookErr {
				if hookCalls != 1 {
					t.Fatalf("BeforeEdit calls = %d, want 1", hookCalls)
				}
			} else if tt.wantHook {
				if hookCalls != 1 {
					t.Fatalf("BeforeEdit calls = %d, want 1", hookCalls)
				}
			} else if hookCalls != 0 {
				t.Fatalf("BeforeEdit calls = %d, want 0", hookCalls)
			}
		})
	}
}

func TestMultiEdit(t *testing.T) {
	t.Run("ordered edits", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		writeFile(t, path, "alpha beta gamma")
		env := testEnv(t, dir)
		env.BeforeEdit = func(string) error { return nil }
		res, err := run(t, NewMultiEditTool(), env, map[string]any{
			"path": "f.txt",
			"edits": []map[string]any{
				{"old_string": "alpha", "new_string": "ALPHA"},
				{"old_string": "gamma", "new_string": "GAMMA"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, "applied 2 edit(s)") {
			t.Fatalf("output = %q", res.Output)
		}
		if got := readFile(t, path); got != "ALPHA beta GAMMA" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("atomic rollback", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		writeFile(t, path, "original content")
		env := testEnv(t, dir)
		hookCalls := 0
		env.BeforeEdit = func(string) error { hookCalls++; return nil }
		_, err := run(t, NewMultiEditTool(), env, map[string]any{
			"path": "f.txt",
			"edits": []map[string]any{
				{"old_string": "original", "new_string": "changed"},
				{"old_string": "does-not-exist", "new_string": "x"},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "old_string not found") {
			t.Fatalf("err = %v, want old_string not found", err)
		}
		if got := readFile(t, path); got != "original content" {
			t.Fatalf("file was modified: %q", got)
		}
		if hookCalls != 0 {
			t.Fatalf("BeforeEdit called %d times despite failed validation", hookCalls)
		}
	})

	t.Run("crlf preservation", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		writeFile(t, path, "a\r\nb\r\nc\r\n")
		env := testEnv(t, dir)
		_, err := run(t, NewMultiEditTool(), env, map[string]any{
			"path": "f.txt",
			"edits": []map[string]any{
				{"old_string": "a\nb", "new_string": "A\nB"},
				{"old_string": "c", "new_string": "C"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, path); got != "A\r\nB\r\nC\r\n" {
			t.Fatalf("content = %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// ls / glob / grep
// ---------------------------------------------------------------------------

func TestLS(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "b.txt"), "bbbbb")
	writeFile(t, filepath.Join(dir, "a.txt"), "aaa")
	writeFile(t, filepath.Join(dir, ".hidden"), "h")
	writeFile(t, filepath.Join(dir, "zdir", "nested.txt"), "n")
	writeFile(t, filepath.Join(dir, ".git", "config"), "git")
	env := testEnv(t, dir)
	tool := NewLSTool()

	res, err := run(t, tool, env, map[string]any{"path": "."})
	if err != nil {
		t.Fatal(err)
	}
	want := "a.txt  3B\nb.txt  5B\nzdir/"
	if res.Output != want {
		t.Fatalf("ls output = %q, want %q", res.Output, want)
	}

	res, err = run(t, tool, env, map[string]any{"path": ".", "all": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, ".hidden  1B") {
		t.Fatalf("hidden entry missing: %q", res.Output)
	}
	if strings.Contains(res.Output, ".git") {
		t.Fatalf(".git should be skipped: %q", res.Output)
	}

	res, err = run(t, tool, env, map[string]any{"path": ".", "all": true, "include_git": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, ".git/") {
		t.Fatalf(".git missing: %q", res.Output)
	}

	res, err = run(t, tool, env, map[string]any{"path": "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "a.txt  3B") {
		t.Fatalf("file listing = %q", res.Output)
	}

	if _, err := run(t, tool, env, map[string]any{"path": "missing"}); err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestGlob(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "")
	writeFile(t, filepath.Join(dir, "b.txt"), "")
	writeFile(t, filepath.Join(dir, "sub", "c.go"), "")
	writeFile(t, filepath.Join(dir, "sub", "deep", "d.go"), "")
	writeFile(t, filepath.Join(dir, "node_modules", "e.go"), "")
	writeFile(t, filepath.Join(dir, ".git", "f.go"), "")
	env := testEnv(t, dir)

	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "recursive", args: map[string]any{"pattern": "**/*.go"}, want: "a.go\nsub/c.go\nsub/deep/d.go"},
		{name: "single segment", args: map[string]any{"pattern": "*.go"}, want: "a.go"},
		{name: "nested pattern", args: map[string]any{"pattern": "sub/*.go"}, want: "sub/c.go"},
		{name: "path base", args: map[string]any{"pattern": "**/*.go", "path": "sub"}, want: "sub/c.go\nsub/deep/d.go"},
		{name: "directories", args: map[string]any{"pattern": "**/"}, want: "sub/\nsub/deep/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := run(t, NewGlobTool(), env, tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if res.Output != tt.want {
				t.Fatalf("glob output = %q, want %q", res.Output, tt.want)
			}
		})
	}

	res, err := run(t, NewGlobTool(), env, map[string]any{"pattern": "**/*.rs"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "no files matched") {
		t.Fatalf("no-match output = %q", res.Output)
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.go", "a.go", true},
		{"*.go", "sub/a.go", false},
		{"**/*.go", "a.go", true},
		{"**/*.go", "sub/deep/a.go", true},
		{"**", "sub/deep/a.go", true},
		{"sub/**/a.go", "sub/a.go", true},
		{"sub/**/a.go", "sub/x/y/a.go", true},
		{"sub/**/a.go", "other/a.go", false},
		{"a?c", "abc", true},
		{"abc", "abd", false},
	}
	for _, tt := range tests {
		if got := matchGlob(tt.pattern, tt.path); got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestGrepPureGo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\nfunc Hello() {}\n")
	writeFile(t, filepath.Join(dir, "sub", "other.go"), "func hello() {} // TODO\n")
	writeFile(t, filepath.Join(dir, "sub", "notes.txt"), "TODO nothing\n")
	writeFile(t, filepath.Join(dir, ".git", "config"), "TODO hidden\n")
	env := testEnv(t, dir)
	tool := NewGrepTool(true)

	res, err := run(t, tool, env, map[string]any{"pattern": "TODO"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "sub/other.go:1:func hello() {} // TODO") {
		t.Fatalf("missing go match: %q", res.Output)
	}
	if !strings.Contains(res.Output, "sub/notes.txt:1:TODO nothing") {
		t.Fatalf("missing txt match: %q", res.Output)
	}
	if strings.Contains(res.Output, ".git") {
		t.Fatalf("searched .git: %q", res.Output)
	}

	res, err = run(t, tool, env, map[string]any{"pattern": "TODO", "glob": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "sub/other.go") || strings.Contains(res.Output, "notes.txt") {
		t.Fatalf("glob filter failed: %q", res.Output)
	}

	res, err = run(t, tool, env, map[string]any{"pattern": "hello", "ignore_case": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "func Hello()") || !strings.Contains(res.Output, "func hello()") {
		t.Fatalf("ignore_case failed: %q", res.Output)
	}

	res, err = run(t, tool, env, map[string]any{"pattern": "TODO", "max_results": 1})
	if err != nil {
		t.Fatal(err)
	}
	if countLines(strings.TrimSuffix(res.Output, "\n[... limited to 1 results ...]")) != 1 {
		t.Fatalf("max_results failed: %q", res.Output)
	}

	res, err = run(t, tool, env, map[string]any{"pattern": "ZZZ"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "no matches") {
		t.Fatalf("no-match output = %q", res.Output)
	}

	res, err = run(t, tool, env, map[string]any{"pattern": "func Hello", "path": "main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "main.go:2:func Hello() {}") {
		t.Fatalf("single file grep = %q", res.Output)
	}

	if _, err := run(t, tool, env, map[string]any{"pattern": "("}); err == nil {
		t.Fatal("expected invalid regex error")
	}
}

func TestGrepWithRipgrepWhenAvailable(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\nfunc Hello() {}\n")
	env := testEnv(t, dir)
	res, err := run(t, NewGrepTool(false), env, map[string]any{"pattern": "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "main.go:2:func Hello() {}") {
		t.Fatalf("rg output = %q", res.Output)
	}
	if res.Metadata["engine"] != "ripgrep" {
		t.Fatalf("engine = %v, want ripgrep", res.Metadata["engine"])
	}
}

// ---------------------------------------------------------------------------
// bash
// ---------------------------------------------------------------------------

func TestBashForeground(t *testing.T) {
	dir := t.TempDir()
	env := testEnv(t, dir)
	env.Shell = "sh"
	tool := NewBashTool()
	defer tool.Close()

	res, err := run(t, tool, env, map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Output) != "hello" {
		t.Fatalf("output = %q", res.Output)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %#v", res)
	}

	res, err = run(t, tool, env, map[string]any{"command": "echo oops; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError for non-zero exit: %#v", res)
	}
	if !strings.Contains(res.Output, "oops") || !strings.Contains(res.Output, "status 3") {
		t.Fatalf("nonzero output = %q", res.Output)
	}
	if res.Metadata["exit_code"] != 3 {
		t.Fatalf("exit_code = %v", res.Metadata["exit_code"])
	}

	if _, err := run(t, tool, env, map[string]any{"command": ""}); err == nil {
		t.Fatal("expected error for empty command")
	}
	if _, err := run(t, tool, env, map[string]any{"action": "bogus"}); err == nil {
		t.Fatal("expected error for unknown action")
	}
}

func TestBashTimeout(t *testing.T) {
	env := testEnv(t, t.TempDir())
	tool := NewBashTool()
	defer tool.Close()

	start := time.Now()
	res, err := run(t, tool, env, map[string]any{"command": "sleep 5", "timeout_seconds": 1})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
	if !res.IsError || !strings.Contains(res.Output, "timed out") {
		t.Fatalf("timeout output = %q (IsError=%v)", res.Output, res.IsError)
	}
	if res.Metadata["timed_out"] != true {
		t.Fatalf("timed_out metadata = %v", res.Metadata["timed_out"])
	}
}

func TestBashContextCancellation(t *testing.T) {
	env := testEnv(t, t.TempDir())
	tool := NewBashTool()
	defer tool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	raw, _ := json.Marshal(map[string]any{"command": "sleep 5"})
	start := time.Now()
	res, err := tool.Execute(ctx, env, raw)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("cancellation took too long: %v", elapsed)
	}
	if !res.IsError || !strings.Contains(res.Output, "cancelled") {
		t.Fatalf("cancel output = %q (IsError=%v)", res.Output, res.IsError)
	}
}

func TestBashBackgroundPollKill(t *testing.T) {
	env := testEnv(t, t.TempDir())
	tool := NewBashTool()
	defer tool.Close()

	res, err := run(t, tool, env, map[string]any{
		"command":    "echo started; sleep 30",
		"background": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := res.Metadata["job_id"].(string)
	if jobID == "" || !strings.Contains(res.Output, "job-1") {
		t.Fatalf("start output = %q, metadata = %#v", res.Output, res.Metadata)
	}

	var polled string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pres, err := run(t, tool, env, map[string]any{"action": "poll", "job_id": jobID})
		if err != nil {
			t.Fatal(err)
		}
		polled = pres.Output
		if strings.Contains(polled, "started") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(polled, "started") {
		t.Fatalf("background job output missing 'started': %q", polled)
	}
	if !strings.Contains(polled, "status: running") && !strings.Contains(polled, "status: completed") {
		t.Fatalf("unexpected poll output: %q", polled)
	}

	kres, err := run(t, tool, env, map[string]any{"action": "kill", "job_id": jobID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(kres.Output, "status: killed") {
		t.Fatalf("kill output = %q", kres.Output)
	}
	if kres.Metadata["status"] != string(JobKilled) {
		t.Fatalf("kill status metadata = %v", kres.Metadata["status"])
	}

	if _, err := run(t, tool, env, map[string]any{"action": "poll", "job_id": "nope"}); err == nil {
		t.Fatal("expected error for unknown job")
	}
	if _, err := run(t, tool, env, map[string]any{"action": "poll"}); err == nil {
		t.Fatal("expected error for missing job_id")
	}
}

func TestBashJobManagerCloseCleansUp(t *testing.T) {
	env := testEnv(t, t.TempDir())
	tool := NewBashTool()
	res, err := run(t, tool, env, map[string]any{"command": "exec sleep 30", "background": true})
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := res.Metadata["job_id"].(string)

	if err := tool.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	snapshot, ok := tool.Jobs().Poll(jobID)
	if !ok {
		t.Fatal("job disappeared after Close")
	}
	if snapshot.Status == JobRunning {
		t.Fatalf("job still running after Close: %#v", snapshot)
	}
	if snapshot.Status != JobKilled && snapshot.Status != JobCompleted {
		t.Fatalf("unexpected final status: %s", snapshot.Status)
	}
	if _, err := tool.Jobs().Start([]string{"sh", "-c"}, "echo nope", env.WorkDir, 1024); err == nil {
		t.Fatal("expected Start to fail after Close")
	}
}

func TestBashTruncatesLargeOutput(t *testing.T) {
	env := testEnv(t, t.TempDir())
	env.OutputCap = 50
	tool := NewBashTool()
	defer tool.Close()
	res, err := run(t, tool, env, map[string]any{"command": "printf 'abcdefghij%.0s' 1 2 3 4 5 6 7 8 9 10"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "[... truncated: showing") {
		t.Fatalf("output not truncated: %q", res.Output)
	}
}

// ---------------------------------------------------------------------------
// todo / ask_user
// ---------------------------------------------------------------------------

func TestTodo(t *testing.T) {
	env := testEnv(t, t.TempDir())
	var updates [][]TodoItem
	env.OnTodoUpdate = func(items []TodoItem) {
		updates = append(updates, items)
	}
	store := NewTodoStore()
	tool := NewTodoToolWithStore(store)

	res, err := run(t, tool, env, map[string]any{"todos": []map[string]any{
		{"id": "1", "content": "write code", "status": "in_progress"},
		{"id": "2", "content": "run tests", "status": "pending"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "[in_progress] write code (id: 1)") ||
		!strings.Contains(res.Output, "[pending] run tests (id: 2)") {
		t.Fatalf("todo output = %q", res.Output)
	}
	if len(updates) != 1 || len(updates[0]) != 2 {
		t.Fatalf("updates = %#v", updates)
	}
	if items := store.Items(); len(items) != 2 || items[0].Status != TodoInProgress {
		t.Fatalf("store items = %#v", items)
	}

	// Re-sending the same list does not trigger a second callback.
	_, err = run(t, tool, env, map[string]any{"todos": []map[string]any{
		{"id": "1", "content": "write code", "status": "in_progress"},
		{"id": "2", "content": "run tests", "status": "pending"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 {
		t.Fatalf("duplicate update callback fired: %d", len(updates))
	}

	// Completing the list triggers an update.
	_, err = run(t, tool, env, map[string]any{"todos": []map[string]any{
		{"id": "1", "content": "write code", "status": "completed"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 2 || updates[1][0].Status != TodoCompleted {
		t.Fatalf("completion update = %#v", updates)
	}

	if _, err := run(t, tool, env, map[string]any{"todos": []map[string]any{
		{"id": "1", "content": "x", "status": "bogus"},
	}}); err == nil || !strings.Contains(err.Error(), "invalid status") {
		t.Fatalf("invalid status err = %v", err)
	}
	if _, err := run(t, tool, env, map[string]any{"todos": []map[string]any{
		{"id": "1", "content": "x", "status": "pending"},
		{"id": "1", "content": "y", "status": "pending"},
	}}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate id err = %v", err)
	}

	res, err = run(t, tool, env, map[string]any{"todos": []map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "cleared") {
		t.Fatalf("clear output = %q", res.Output)
	}
}

func TestAskUser(t *testing.T) {
	env := testEnv(t, t.TempDir())
	var gotQuestion string
	var gotOptions []string
	env.AskUser = func(_ context.Context, question string, options []string) (string, error) {
		gotQuestion = question
		gotOptions = append([]string(nil), options...)
		return "yes", nil
	}
	res, err := run(t, NewAskUserTool(), env, map[string]any{
		"question": "Proceed?",
		"options":  []string{"yes", "no"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "yes" {
		t.Fatalf("output = %q", res.Output)
	}
	if gotQuestion != "Proceed?" || len(gotOptions) != 2 {
		t.Fatalf("callback got %q %#v", gotQuestion, gotOptions)
	}

	if _, err := run(t, NewAskUserTool(), env, map[string]any{"question": "  "}); err == nil {
		t.Fatal("expected error for empty question")
	}
	nonInteractive := testEnv(t, t.TempDir())
	if _, err := run(t, NewAskUserTool(), nonInteractive, map[string]any{"question": "hi"}); err == nil ||
		!strings.Contains(err.Error(), "non-interactive") {
		t.Fatalf("non-interactive err = %v", err)
	}
}
