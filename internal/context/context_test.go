package context

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func mustIndex(t *testing.T, haystack, needle string) int {
	t.Helper()
	index := strings.Index(haystack, needle)
	if index < 0 {
		t.Fatalf("expected %q in output:\n%s", needle, haystack)
	}
	return index
}

func TestLoadProjectInstructionsHierarchy(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	globalDir := t.TempDir()
	writeTestFile(t, filepath.Join(globalDir, "AGENTS.md"), "global agents\n")
	writeTestFile(t, filepath.Join(globalDir, "TERN.md"), "global tern\n")

	writeTestFile(t, filepath.Join(root, "AGENTS.md"), "root agents\n")
	writeTestFile(t, filepath.Join(root, "TERN.md"), "root tern\n")
	writeTestFile(t, filepath.Join(root, "sub", "AGENTS.md"), "sub agents\n")
	writeTestFile(t, filepath.Join(root, "sub", "deep", "TERN.md"), "deep tern\n")

	workDir := filepath.Join(root, "sub", "deep")
	files, err := LoadProjectInstructions(workDir, globalDir)
	if err != nil {
		t.Fatalf("LoadProjectInstructions: %v", err)
	}

	wantPaths := []string{
		filepath.Join(globalDir, "AGENTS.md"),
		filepath.Join(globalDir, "TERN.md"),
		filepath.Join(root, "AGENTS.md"),
		filepath.Join(root, "TERN.md"),
		filepath.Join(root, "sub", "AGENTS.md"),
		filepath.Join(root, "sub", "deep", "TERN.md"),
	}
	if len(files) != len(wantPaths) {
		t.Fatalf("got %d instruction files, want %d: %+v", len(files), len(wantPaths), files)
	}

	wantScopes := []string{
		ScopeGlobal, ScopeGlobal,
		ScopeProject, ScopeProject, ScopeProject, ScopeProject,
	}
	for i, file := range files {
		if file.Path != wantPaths[i] {
			t.Errorf("files[%d].Path = %q, want %q", i, file.Path, wantPaths[i])
		}
		if file.Scope != wantScopes[i] {
			t.Errorf("files[%d].Scope = %q, want %q", i, file.Scope, wantScopes[i])
		}
		if file.Dir != filepath.Dir(wantPaths[i]) {
			t.Errorf("files[%d].Dir = %q, want %q", i, file.Dir, filepath.Dir(wantPaths[i]))
		}
		if file.Name != filepath.Base(wantPaths[i]) {
			t.Errorf("files[%d].Name = %q, want %q", i, file.Name, filepath.Base(wantPaths[i]))
		}
		if strings.TrimSpace(file.Content) == "" {
			t.Errorf("files[%d] has empty content", i)
		}
	}

	formatted := FormatInstructions(files)
	if formatted == "" {
		t.Fatal("FormatInstructions returned empty output")
	}
	last := -1
	for _, needle := range []string{
		"global agents",
		"global tern",
		"root agents",
		"root tern",
		"sub agents",
		"deep tern",
	} {
		index := mustIndex(t, formatted, needle)
		if index <= last {
			t.Fatalf("instruction %q is out of order in:\n%s", needle, formatted)
		}
		last = index
	}
	mustIndex(t, formatted, "# Project Instructions")
	mustIndex(t, formatted, "("+ScopeGlobal+")")
	mustIndex(t, formatted, "("+ScopeProject+")")

	if again := FormatInstructions(files); again != formatted {
		t.Fatal("FormatInstructions is not deterministic")
	}

	// Loading again must produce the same order too.
	filesAgain, err := LoadProjectInstructions(workDir, globalDir)
	if err != nil {
		t.Fatalf("LoadProjectInstructions again: %v", err)
	}
	if got, want := FormatInstructions(filesAgain), formatted; got != want {
		t.Fatalf("second load differs:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestLoadProjectInstructionsOutsideGitRepo(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "AGENTS.md"), "outer agents\n")
	writeTestFile(t, filepath.Join(root, "child", "TERN.md"), "inner tern\n")

	files, err := LoadProjectInstructions(filepath.Join(root, "child"), "")
	if err != nil {
		t.Fatalf("LoadProjectInstructions: %v", err)
	}
	if len(files) < 2 {
		t.Fatalf("got %d files, want at least 2", len(files))
	}

	// The walk may pick up AGENTS.md files above the temporary directory.
	// The files inside root must nevertheless be present and ordered.
	outer := -1
	inner := -1
	for i, file := range files {
		if file.Path == filepath.Join(root, "AGENTS.md") {
			outer = i
		}
		if file.Path == filepath.Join(root, "child", "TERN.md") {
			inner = i
		}
	}
	if outer < 0 || inner < 0 {
		t.Fatalf("missing instruction files: %+v", files)
	}
	if outer >= inner {
		t.Fatalf("outer instruction at %d must precede inner at %d", outer, inner)
	}
	if files[len(files)-1].Path != filepath.Join(root, "child", "TERN.md") {
		t.Fatalf("last instruction = %q, want the closest TERN.md", files[len(files)-1].Path)
	}
}

func TestLoadProjectInstructionsErrorsAndEmptyFormat(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := LoadProjectInstructions(missing, ""); err == nil {
		t.Fatal("expected an error for a missing work directory")
	}

	if got := FormatInstructions(nil); got != "" {
		t.Fatalf("FormatInstructions(nil) = %q, want empty", got)
	}
	if got := FormatInstructions([]InstructionFile{{Path: "x/AGENTS.md", Content: "  \n\t"}}); got != "" {
		t.Fatalf("FormatInstructions with blank content = %q, want empty", got)
	}
}

func TestSkillsListingAndLoading(t *testing.T) {
	globalDir := t.TempDir()
	workDir := t.TempDir()

	writeTestFile(t, filepath.Join(globalDir, "skills", "foo.md"), `---
name: foo
description: Foo skill
tags: alpha, beta
---
# Foo

Foo body.
`)

	writeTestFile(t, filepath.Join(globalDir, "skills", "bar", "SKILL.md"), `---
description: Bar skill
tags:
  - yaml
  - list
---
Bar body
`)

	writeTestFile(t, filepath.Join(globalDir, "skills", "override.md"), `---
name: override
description: global override
tags: [global, shared]
---
Global body
`)

	writeTestFile(t, filepath.Join(workDir, ".tern", "skills", "override.md"), `---
name: override
description: project override
tags: [project]
---
Project body
`)

	writeTestFile(t, filepath.Join(workDir, ".tern", "skills", "plain.md"), "No frontmatter here.\n")

	metas, err := ListSkills(workDir, globalDir)
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}

	if got, want := len(metas), 4; got != want {
		t.Fatalf("got %d skills, want %d: %+v", got, want, metas)
	}
	wantNames := []string{"bar", "foo", "override", "plain"}
	for i, name := range wantNames {
		if metas[i].Name != name {
			t.Errorf("skills[%d].Name = %q, want %q", i, metas[i].Name, name)
		}
	}

	if metas[0].Description != "Bar skill" {
		t.Errorf("bar description = %q", metas[0].Description)
	}
	if !reflect.DeepEqual(metas[0].Tags, []string{"yaml", "list"}) {
		t.Errorf("bar tags = %#v", metas[0].Tags)
	}
	if metas[0].Scope != ScopeGlobal {
		t.Errorf("bar scope = %q", metas[0].Scope)
	}
	if !strings.HasSuffix(metas[0].Path, filepath.Join("bar", "SKILL.md")) {
		t.Errorf("bar path = %q", metas[0].Path)
	}

	if metas[1].Description != "Foo skill" {
		t.Errorf("foo description = %q", metas[1].Description)
	}
	if !reflect.DeepEqual(metas[1].Tags, []string{"alpha", "beta"}) {
		t.Errorf("foo tags = %#v", metas[1].Tags)
	}

	// Project override wins over the global skill with the same name.
	if metas[2].Description != "project override" {
		t.Errorf("override description = %q", metas[2].Description)
	}
	if metas[2].Scope != ScopeProject {
		t.Errorf("override scope = %q", metas[2].Scope)
	}
	if !reflect.DeepEqual(metas[2].Tags, []string{"project"}) {
		t.Errorf("override tags = %#v", metas[2].Tags)
	}
	if !strings.HasSuffix(metas[2].Path, filepath.Join(workDir, ".tern", "skills", "override.md")) {
		t.Errorf("override path = %q", metas[2].Path)
	}

	if metas[3].Description != "" || len(metas[3].Tags) != 0 {
		t.Errorf("plain metadata = %+v", metas[3])
	}

	foo, err := LoadSkill(workDir, globalDir, "foo")
	if err != nil {
		t.Fatalf("LoadSkill(foo): %v", err)
	}
	if foo.Name != "foo" || foo.Description != "Foo skill" {
		t.Errorf("foo = %+v", foo)
	}
	if foo.Body != "# Foo\n\nFoo body." {
		t.Errorf("foo body = %q", foo.Body)
	}

	bar, err := LoadSkill(workDir, globalDir, "bar")
	if err != nil {
		t.Fatalf("LoadSkill(bar): %v", err)
	}
	if bar.Body != "Bar body" {
		t.Errorf("bar body = %q", bar.Body)
	}

	override, err := LoadSkill(workDir, globalDir, "override")
	if err != nil {
		t.Fatalf("LoadSkill(override): %v", err)
	}
	if override.Body != "Project body" || override.Description != "project override" {
		t.Errorf("override = %+v", override)
	}

	if _, err := LoadSkill(workDir, globalDir, "missing"); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("LoadSkill(missing) error = %v, want ErrSkillNotFound", err)
	}
}

func TestSkillDirectoryLayoutPreferred(t *testing.T) {
	globalDir := t.TempDir()
	workDir := t.TempDir()

	writeTestFile(t, filepath.Join(globalDir, "skills", "dup.md"), `---
name: dup
description: flat
---
flat body
`)
	writeTestFile(t, filepath.Join(globalDir, "skills", "dup", "SKILL.md"), `---
name: dup
description: directory
---
directory body
`)

	skill, err := LoadSkill(workDir, globalDir, "dup")
	if err != nil {
		t.Fatalf("LoadSkill(dup): %v", err)
	}
	if skill.Description != "directory" || skill.Body != "directory body" {
		t.Fatalf("expected <name>/SKILL.md to win, got %+v", skill)
	}
}

func TestCommandsListingOverridesAndExpansion(t *testing.T) {
	globalDir := t.TempDir()
	workDir := t.TempDir()

	writeTestFile(t, filepath.Join(globalDir, "commands", "review.md"), `---
description: Global review
---
GLOBAL $ARGUMENTS | $1 | $2
`)
	writeTestFile(t, filepath.Join(globalDir, "commands", "ignored.txt"), "not a command\n")

	writeTestFile(t, filepath.Join(workDir, ".tern", "commands", "review.md"), `---
name: review
description: Project review
---
PROJECT $ARGUMENTS | $1 | $2
`)
	writeTestFile(t, filepath.Join(workDir, ".tern", "commands", "plain.md"), "Echo $ARGUMENTS\n")

	commands, err := LoadCommands(workDir, globalDir)
	if err != nil {
		t.Fatalf("LoadCommands: %v", err)
	}
	if got, want := len(commands), 2; got != want {
		t.Fatalf("got %d commands, want %d: %+v", got, want, commands)
	}
	if commands[0].Name != "plain" || commands[1].Name != "review" {
		t.Fatalf("commands not sorted by name: %+v", commands)
	}

	review := commands[1]
	if review.Description != "Project review" {
		t.Errorf("review description = %q", review.Description)
	}
	if review.Scope != ScopeProject {
		t.Errorf("review scope = %q", review.Scope)
	}
	if !strings.HasSuffix(review.Path, filepath.Join(workDir, ".tern", "commands", "review.md")) {
		t.Errorf("review path = %q", review.Path)
	}

	args := `"src/a b.go" second`
	want := `PROJECT "src/a b.go" second | src/a b.go | second`
	if got := review.Expand(args); got != want {
		t.Errorf("Expand(%q) = %q, want %q", args, got, want)
	}

	loaded, err := LoadCommand(workDir, globalDir, "review")
	if err != nil {
		t.Fatalf("LoadCommand(review): %v", err)
	}
	if loaded.Template != review.Template {
		t.Errorf("LoadCommand template = %q, want %q", loaded.Template, review.Template)
	}
	if _, err := LoadCommand(workDir, globalDir, "missing"); !errors.Is(err, ErrCommandNotFound) {
		t.Fatalf("LoadCommand(missing) error = %v, want ErrCommandNotFound", err)
	}
}

func TestCommandExpandPositionals(t *testing.T) {
	tests := []struct {
		name     string
		template string
		args     string
		want     string
	}{
		{
			name:     "missing positionals become empty",
			template: "A=$1 B=$2 C=$3",
			args:     "one",
			want:     "A=one B= C=",
		},
		{
			name:     "multi digit positional",
			template: "$1|$10",
			args:     "1 2 3 4 5 6 7 8 9 ten",
			want:     "1|ten",
		},
		{
			name:     "arguments are not re-expanded",
			template: "[$ARGUMENTS]",
			args:     "$1 $2",
			want:     "[$1 $2]",
		},
		{
			name:     "single quotes and escapes",
			template: "$1 / $2 / $3",
			args:     `'a b' c\ d "e \"f\""`,
			want:     `a b / c d / e "f"`,
		},
		{
			name:     "no placeholders",
			template: "plain command",
			args:     "ignored",
			want:     "plain command",
		},
		{
			name:     "single dollar character",
			template: "cost is $",
			args:     "x",
			want:     "cost is $",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			command := SlashCommand{Template: tc.template}
			if got := command.Expand(tc.args); got != tc.want {
				t.Fatalf("Expand(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestSplitArgs(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{`a "b c" 'd e'`, []string{"a", "b c", "d e"}},
		{`""`, []string{""}},
		{`a\ b`, []string{"a b"}},
		{`  `, nil},
	}
	for _, tc := range tests {
		if got := splitArgs(tc.input); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitArgs(%q) = %#v, want %#v", tc.input, got, tc.want)
		}
	}
}

func TestBuildInitialContextWithoutGit(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2025, 3, 4, 15, 16, 17, 0, time.UTC)

	got := BuildInitialContext(dir, now)
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	mustIndex(t, got, "<environment>")
	mustIndex(t, got, "Working directory: "+filepath.Clean(abs))
	mustIndex(t, got, "Platform: "+runtime.GOOS+"/"+runtime.GOARCH)
	mustIndex(t, got, "Date: 2025-03-04")
	mustIndex(t, got, "</environment>")

	if !insideGitRepo(abs) && strings.Contains(got, "Git branch:") {
		t.Fatalf("unexpected git context outside a repository:\n%s", got)
	}

	if again := BuildInitialContext(dir, now); again != got {
		t.Fatal("BuildInitialContext is not deterministic for a fixed time")
	}
}

func TestBuildInitialContextWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	runGitTest(t, dir, "init")
	runGitTest(t, dir, "config", "user.email", "test@example.com")
	runGitTest(t, dir, "config", "user.name", "Test")
	runGitTest(t, dir, "checkout", "-B", "main")
	writeTestFile(t, filepath.Join(dir, "untracked.txt"), "hello\n")

	now := time.Date(2025, 12, 31, 23, 59, 0, 0, time.UTC)
	got := BuildInitialContext(dir, now)

	mustIndex(t, got, "Git branch: main")
	mustIndex(t, got, "Git status:\n")
	mustIndex(t, got, "?? untracked.txt")
	mustIndex(t, got, "Date: 2025-12-31")

	// A clean repository reports "clean" instead of an empty status block.
	runGitTest(t, dir, "add", "untracked.txt")
	runGitTest(t, dir, "commit", "-m", "initial")
	clean := BuildInitialContext(dir, now)
	mustIndex(t, clean, "Git status: clean")
	if strings.Contains(clean, "?? untracked.txt") {
		t.Fatalf("clean repository still reports untracked file:\n%s", clean)
	}
}
