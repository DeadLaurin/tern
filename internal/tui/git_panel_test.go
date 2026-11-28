package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tern-agent/tern/internal/integrations"
)

func TestSidebarChangesAndGitModalRender(t *testing.T) {
	m := New(Options{NoColor: true, WorkDir: t.TempDir()})
	m.width = 130
	m.height = 40
	m.gitStatus = integrations.GitStatus{
		IsRepo: true,
		Branch: "main",
		Ahead:  1,
		Files: []integrations.FileChange{
			{Path: "internal/tui/sidebar.go", Status: "M", Staged: true, Added: 12, Removed: 2},
			{Path: "README.md", Status: "M", Unstaged: true, Added: 3, Removed: 0},
		},
		StagedCount:  1,
		TotalAdded:   15,
		TotalRemoved: 2,
		RepoSlug:     "DeadLaurin/tern",
	}
	m.ghStatus = integrations.GitHubStatus{
		Available:   true,
		GHInstalled: true,
		Repo:        "DeadLaurin/tern",
		PR: &integrations.PullRequestInfo{
			Number:      42,
			Title:       "Add git sidebar",
			State:       "OPEN",
			ChecksState: "passing",
		},
	}
	m.cfStatus = integrations.CloudflareStatus{
		Detected:    true,
		Kind:        "pages",
		ProjectName: "tern-site",
		ConfigFile:  "wrangler.toml",
	}

	sb := m.sidebarView(38, 36)
	for _, want := range []string{"CHANGES (2)", "internal/tui/sidebar.go", "[Diff · Commit & Push]", "GH PR #42", "CF pages · tern-site"} {
		if !strings.Contains(sb, want) {
			t.Errorf("sidebar missing %q:\n%s", want, sb)
		}
	}

	m.openGitModal("README.md", false)
	if m.activeModal != modalGit {
		t.Fatalf("expected modalGit, got %v", m.activeModal)
	}
	if m.gitModal.fileIndex != 1 {
		t.Fatalf("expected fileIndex=1 for README.md, got %d", m.gitModal.fileIndex)
	}
	m.handleGitDiffMsg(gitDiffMsg{path: "README.md", diff: "@@ -1 +1,2 @@\n hello\n+world\n"})

	view := m.gitModalView()
	for _, want := range []string{"Git · main ↑1", "README.md", "+world", "Commit >"} {
		if !strings.Contains(view, want) {
			t.Errorf("git modal missing %q:\n%s", want, view)
		}
	}

	// Pressing Esc closes the modal.
	m.handleGitModalKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.activeModal != modalNone {
		t.Fatalf("expected modalNone after Esc, got %v", m.activeModal)
	}
}
