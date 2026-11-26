package context

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestIndexRepoRespectsGitignore(t *testing.T) {
	root := t.TempDir()

	writeTestFile(t, filepath.Join(root, ".gitignore"), `# dependencies
node_modules/

# logs and env files
*.log
.env.local
dist/
build/*
!keep.log
`)

	// Included files.
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(root, "README.md"), "# readme\n")
	writeTestFile(t, filepath.Join(root, "keep.log"), "keep me\n")
	writeTestFile(t, filepath.Join(root, ".env"), "A=1\n")
	writeTestFile(t, filepath.Join(root, "src", "main.go"), "package src\n")

	// Ignored files and directories.
	writeTestFile(t, filepath.Join(root, "app.log"), "ignored\n")
	writeTestFile(t, filepath.Join(root, ".env.local"), "SECRET=1\n")
	writeTestFile(t, filepath.Join(root, "node_modules", "pkg", "index.js"), "ignored\n")
	writeTestFile(t, filepath.Join(root, "src", "app.log"), "ignored\n")
	writeTestFile(t, filepath.Join(root, "build", "out.js"), "ignored\n")
	writeTestFile(t, filepath.Join(root, "dist", "bundle.js"), "ignored\n")

	// Always-skipped VCS and internal directories.
	writeTestFile(t, filepath.Join(root, ".git", "config"), "[core]\n")
	writeTestFile(t, filepath.Join(root, ".tern", "state.json"), "{}\n")
	writeTestFile(t, filepath.Join(root, ".hg", "store"), "ignored\n")

	index, err := IndexRepo(root, 0)
	if err != nil {
		t.Fatalf("IndexRepo: %v", err)
	}

	if index.Root != root {
		t.Errorf("Root = %q, want %q", index.Root, root)
	}
	if got, want := index.FileCount, 6; got != want {
		t.Errorf("FileCount = %d, want %d (entries: %v)", got, want, index.Entries)
	}
	if got, want := index.DirCount, 2; got != want {
		t.Errorf("DirCount = %d, want %d", got, want)
	}
	if got, want := index.IgnoredCount, 9; got != want {
		t.Errorf("IgnoredCount = %d, want %d", got, want)
	}
	if index.Truncated {
		t.Error("Truncated = true, want false")
	}
	if index.Duration <= 0 {
		t.Error("Duration must be positive")
	}

	wantEntries := []string{
		".env",
		".gitignore",
		"README.md",
		"keep.log",
		"main.go",
		"src/main.go",
	}
	if !reflect.DeepEqual(index.Entries, wantEntries) {
		t.Errorf("Entries = %v, want %v", index.Entries, wantEntries)
	}

	for _, unwanted := range []string{"app.log", ".env.local", "node_modules", "build/out.js", "dist/", ".tern", ".git/", ".hg/"} {
		for _, entry := range index.Entries {
			if strings.Contains(entry, unwanted) {
				t.Errorf("ignored path %q leaked into entries: %v", unwanted, index.Entries)
			}
		}
	}

	summary := index.Summary()
	mustIndex(t, summary, "indexed 6 files")
	mustIndex(t, summary, ".gitignore respected")

	formatted := index.Format()
	mustIndex(t, formatted, "<repo-map>")
	mustIndex(t, formatted, "</repo-map>")
	mustIndex(t, formatted, "keep.log")
	if strings.Contains(formatted, "app.log") {
		t.Fatalf("Format leaked an ignored log file:\n%s", formatted)
	}
}

func TestIndexRepoDeterministic(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".gitignore"), "*.tmp\n")
	for _, name := range []string{"b.go", "a.go", "z.go", "sub/c.go", "sub/a.go", "sub/deep/x.go", "drop.tmp"} {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(name)), "x\n")
	}

	var first *RepoIndex
	for i := 0; i < 5; i++ {
		index, err := IndexRepo(root, 80)
		if err != nil {
			t.Fatalf("IndexRepo run %d: %v", i, err)
		}
		if i == 0 {
			first = index
			continue
		}
		if !reflect.DeepEqual(index.Entries, first.Entries) {
			t.Fatalf("run %d entries = %v, want %v", i, index.Entries, first.Entries)
		}
		if index.FileCount != first.FileCount || index.DirCount != first.DirCount || index.IgnoredCount != first.IgnoredCount {
			t.Fatalf("run %d counts = (%d,%d,%d), want (%d,%d,%d)",
				i, index.FileCount, index.DirCount, index.IgnoredCount,
				first.FileCount, first.DirCount, first.IgnoredCount)
		}
	}

	sorted := append([]string(nil), first.Entries...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(first.Entries, sorted) {
		t.Fatalf("entries are not sorted: %v", first.Entries)
	}
	if first.FileCount != 7 {
		t.Fatalf("FileCount = %d, want 7 (entries: %v)", first.FileCount, first.Entries)
	}
}

func TestIndexRepoTruncationMarker(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		writeTestFile(t, filepath.Join(root, "src", "f"+string(rune('0'+i))+".go"), "x\n")
	}

	index, err := IndexRepo(root, 4)
	if err != nil {
		t.Fatalf("IndexRepo: %v", err)
	}
	if !index.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if got, want := index.FileCount, 10; got != want {
		t.Fatalf("FileCount = %d, want %d", got, want)
	}
	if got, want := len(index.Entries), 4; got != want {
		t.Fatalf("len(Entries) = %d, want %d: %v", got, want, index.Entries)
	}

	want := []string{"src/", "src/f0.go", "src/f1.go", "src/f2.go"}
	if !reflect.DeepEqual(index.Entries, want) {
		t.Fatalf("Entries = %v, want %v", index.Entries, want)
	}
	mustIndex(t, index.Format(), "src/")
	mustIndex(t, index.Format(), "10 files total")
}

func TestIndexRepoEmptyAndInvalid(t *testing.T) {
	index, err := IndexRepo(t.TempDir(), 80)
	if err != nil {
		t.Fatalf("IndexRepo(empty): %v", err)
	}
	if index.FileCount != 0 || index.DirCount != 0 || index.Truncated {
		t.Fatalf("empty index = %+v", index)
	}
	if got := index.Format(); got != "" {
		t.Fatalf("Format(empty) = %q, want empty", got)
	}

	if _, err := IndexRepo(filepath.Join(t.TempDir(), "missing"), 80); err == nil {
		t.Fatal("expected an error for a missing directory")
	}
}

func TestBuildInitialContextIncludesRepoMap(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".gitignore"), "ignored.log\n")
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(root, "src", "util.go"), "package src\n")
	writeTestFile(t, filepath.Join(root, "ignored.log"), "noise\n")
	writeTestFile(t, filepath.Join(root, "node_modules", "dep", "index.js"), "noise\n")

	now := time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC)
	out := BuildInitialContext(root, now)

	mustIndex(t, out, "<environment>")
	mustIndex(t, out, "<repo-map>")
	mustIndex(t, out, "indexed 3 files")
	mustIndex(t, out, ".gitignore respected")
	mustIndex(t, out, "main.go")
	mustIndex(t, out, "src/util.go")
	mustIndex(t, out, "</repo-map>")
	mustIndex(t, out, "</environment>")

	if strings.Contains(out, "ignored.log\n") {
		t.Fatalf("ignored file leaked into context:\n%s", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Fatalf("node_modules leaked into context:\n%s", out)
	}

	// The repo map must be inside the one-time environment block, which is
	// what keeps the system prompt stable and prefix-cache-safe.
	if strings.Index(out, "<repo-map>") > strings.Index(out, "</environment>") {
		t.Fatalf("repo map is outside the environment block:\n%s", out)
	}
}

func TestIndexRepoSkipMarkersAreNotEntries(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeTestFile(t, filepath.Join(root, "ok.txt"), "ok\n")

	index, err := IndexRepo(root, 80)
	if err != nil {
		t.Fatalf("IndexRepo: %v", err)
	}
	if !reflect.DeepEqual(index.Entries, []string{"ok.txt"}) {
		t.Fatalf("Entries = %v, want [ok.txt]", index.Entries)
	}
	if index.IgnoredCount != 1 {
		t.Fatalf("IgnoredCount = %d, want 1", index.IgnoredCount)
	}

	// Ensure we did not create or modify anything outside the temporary tree.
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root disappeared: %v", err)
	}
}
