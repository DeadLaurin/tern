package integrations

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tern-agent/tern/internal/provider"
)

func initTestRepo(t *testing.T) (repoDir, bareDir string) {
	t.Helper()
	ctx := context.Background()
	bareDir = t.TempDir()
	if _, err := runCmd(ctx, defaultGitTimeout, bareDir, "git", "init", "--bare"); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	repoDir = t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@tern.local"},
		{"config", "user.name", "Tern Test"},
		{"remote", "add", "origin", bareDir},
	} {
		if _, err := runCmd(ctx, defaultGitTimeout, repoDir, "git", args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(ctx, defaultGitTimeout, repoDir, "git", "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(ctx, defaultGitTimeout, repoDir, "git", "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(ctx, defaultGitTimeout, repoDir, "git", "push", "-u", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	return repoDir, bareDir
}

func TestGitStatusDiffStageCommitPush(t *testing.T) {
	ctx := context.Background()
	repoDir, _ := initTestRepo(t)

	// Clean repo.
	st := Status(ctx, repoDir)
	if !st.IsRepo {
		t.Fatal("expected IsRepo=true")
	}
	if st.Branch != "main" {
		t.Fatalf("branch = %q, want main", st.Branch)
	}
	if len(st.Files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(st.Files))
	}

	// Modify tracked file + add untracked file.
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "new.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st = Status(ctx, repoDir)
	if len(st.Files) != 2 {
		t.Fatalf("expected 2 changed files, got %d: %+v", len(st.Files), st.Files)
	}
	if st.TotalAdded < 3 {
		t.Fatalf("expected TotalAdded >= 3, got %d", st.TotalAdded)
	}

	// Diff tracked and untracked files.
	diffTracked, err := Diff(ctx, repoDir, "README.md", false)
	if err != nil || !strings.Contains(diffTracked, "+world") {
		t.Fatalf("unexpected tracked diff: err=%v diff=%q", err, diffTracked)
	}
	diffUntracked, err := Diff(ctx, repoDir, "new.txt", false)
	if err != nil || !strings.Contains(diffUntracked, "+line1") {
		t.Fatalf("unexpected untracked diff: err=%v diff=%q", err, diffUntracked)
	}

	// Stage and unstage.
	if err := Stage(ctx, repoDir, "README.md"); err != nil {
		t.Fatal(err)
	}
	st = Status(ctx, repoDir)
	if st.StagedCount != 1 {
		t.Fatalf("StagedCount = %d, want 1", st.StagedCount)
	}
	if err := Unstage(ctx, repoDir, "README.md"); err != nil {
		t.Fatal(err)
	}
	st = Status(ctx, repoDir)
	if st.StagedCount != 0 {
		t.Fatalf("StagedCount = %d, want 0", st.StagedCount)
	}

	// Commit and push.
	res, err := CommitAndPush(ctx, repoDir, "feat: add new.txt and update readme", true)
	if err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}
	if res.SHA == "" || !res.Pushed {
		t.Fatalf("unexpected CommitResult: %+v", res)
	}

	st = Status(ctx, repoDir)
	if len(st.Files) != 0 || st.Ahead != 0 {
		t.Fatalf("expected clean synced repo after push, got %+v", st)
	}
}

type stubProvider struct {
	text string
}

func (s *stubProvider) Stream(_ context.Context, _ provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	ch := make(chan provider.StreamEvent, 2)
	ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: s.text}
	ch <- provider.StreamEvent{Type: provider.EventDone}
	close(ch)
	return ch, nil
}

func (s *stubProvider) ModelInfo(model string) provider.ModelCapabilities {
	return provider.DefaultModelCapabilities(model)
}

func TestDraftCommitMessageWithProvider(t *testing.T) {
	ctx := context.Background()
	repoDir, _ := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &stubProvider{text: "`docs: update readme title`\n"}
	msg, err := DraftCommitMessage(ctx, repoDir, mock, "deepseek-flash")
	if err != nil {
		t.Fatalf("DraftCommitMessage: %v", err)
	}
	if msg != "docs: update readme title" {
		t.Fatalf("got %q, want %q", msg, "docs: update readme title")
	}
}

func TestParseGitHubSlug(t *testing.T) {
	cases := map[string]string{
		"git@github.com:DeadLaurin/tern.git":         "DeadLaurin/tern",
		"https://github.com/DeadLaurin/tern.git":     "DeadLaurin/tern",
		"https://github.com/DeadLaurin/tern":         "DeadLaurin/tern",
		"https://gitlab.com/someone/other.git":       "",
		"":                                           "",
	}
	for in, want := range cases {
		if got := ParseGitHubSlug(in); got != want {
			t.Errorf("ParseGitHubSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectCloudflare(t *testing.T) {
	ctx := context.Background()

	// Empty dir: not detected.
	if st := DetectCloudflare(ctx, t.TempDir()); st.Detected {
		t.Fatalf("expected Detected=false, got %+v", st)
	}

	// wrangler.toml for Pages.
	dir1 := t.TempDir()
	toml := `name = "tern-site"
pages_build_output_dir = "docs"
`
	if err := os.WriteFile(filepath.Join(dir1, "wrangler.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	st1 := DetectCloudflare(ctx, dir1)
	if !st1.Detected || st1.Kind != "pages" || st1.ProjectName != "tern-site" || st1.PagesDir != "docs" {
		t.Fatalf("unexpected pages status: %+v", st1)
	}

	// wrangler.jsonc for Workers.
	dir2 := t.TempDir()
	jsonc := `{
  // worker config
  "name": "tern-worker",
  "main": "src/index.ts"
}
`
	if err := os.WriteFile(filepath.Join(dir2, "wrangler.jsonc"), []byte(jsonc), 0o644); err != nil {
		t.Fatal(err)
	}
	st2 := DetectCloudflare(ctx, dir2)
	if !st2.Detected || st2.Kind != "workers" || st2.ProjectName != "tern-worker" {
		t.Fatalf("unexpected workers status: %+v", st2)
	}
}
