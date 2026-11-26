package context

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestListSkillsAutoDetectsClaudeAndAgentsAndSymlinks(t *testing.T) {
	workDir := t.TempDir()
	globalDir := t.TempDir()

	// .agents/skills/shared
	writeSkill := func(root, name, desc string) {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n# " + name + "\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeSkill(filepath.Join(workDir, ".agents", "skills"), "shared", "from-agents")
	writeSkill(filepath.Join(workDir, ".claude", "skills"), "claude-only", "from-claude")
	writeSkill(filepath.Join(workDir, ".claude", "skills"), "shared", "from-claude")
	writeSkill(filepath.Join(workDir, ".tern", "skills"), "tern-only", "from-tern")

	// Symlinked skill folder inside .claude/skills (as created by `npx skills add`).
	extTarget := t.TempDir()
	writeSkill(extTarget, "linked-skill", "via-symlink")
	if err := os.Symlink(filepath.Join(extTarget, "linked-skill"), filepath.Join(workDir, ".claude", "skills", "linked-skill")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	metas, err := ListSkills(workDir, globalDir)
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	byName := make(map[string]SkillMeta, len(metas))
	for _, m := range metas {
		byName[m.Name] = m
	}

	if got := byName["shared"].Description; got != "from-claude" {
		t.Fatalf("shared skill description = %q, want from-claude (.claude overrides .agents)", got)
	}
	if _, ok := byName["claude-only"]; !ok {
		t.Fatalf("missing .claude/skills/claude-only: %+v", metas)
	}
	if _, ok := byName["tern-only"]; !ok {
		t.Fatalf("missing .tern/skills/tern-only: %+v", metas)
	}
	if _, ok := byName["linked-skill"]; !ok {
		t.Fatalf("missing symlinked skill linked-skill: %+v", metas)
	}
}

func TestInstallSkillsFromURLAndMultiSkillDir(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("---\nname: web-review\ndescription: Pulls from web\n---\nAlways check edge cases.\n"))
	}))
	defer srv.Close()

	target := t.TempDir()
	installed, err := InstallSkills(context.Background(), target, srv.URL+"/skills/web-review/SKILL.md", "", srv.Client())
	if err != nil {
		t.Fatalf("InstallSkills(URL): %v", err)
	}
	if len(installed) != 1 || installed[0].Name != "web-review" {
		t.Fatalf("installed = %+v", installed)
	}
	if _, err := os.Stat(filepath.Join(target, "web-review", "SKILL.md")); err != nil {
		t.Fatalf("installed SKILL.md missing: %v", err)
	}

	// RemoveSkill cleans it up.
	if err := RemoveSkill(target, "web-review"); err != nil {
		t.Fatalf("RemoveSkill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "web-review")); !os.IsNotExist(err) {
		t.Fatalf("skill directory still exists after RemoveSkill")
	}

	// Multi-skill local directory (like a cloned skills repo).
	repoDir := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		d := filepath.Join(repoDir, "skills", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: "+name+" skill\n---\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	multi, err := InstallSkills(context.Background(), target, repoDir, "", nil)
	if err != nil {
		t.Fatalf("InstallSkills(multi-dir): %v", err)
	}
	if len(multi) != 2 {
		t.Fatalf("installed %d skills, want 2: %+v", len(multi), multi)
	}

	// URL parser checks.
	rawURL, inferred, ok := githubBlobToRaw("https://github.com/anthropics/skills/blob/main/skills/pdf/SKILL.md")
	if !ok || rawURL != "https://raw.githubusercontent.com/anthropics/skills/main/skills/pdf/SKILL.md" || inferred != "pdf" {
		t.Fatalf("githubBlobToRaw = (%q, %q, %v)", rawURL, inferred, ok)
	}
	repoURL, branch, subdir, ok := parseGitHubRepoSource("https://github.com/anthropics/skills/tree/main/skills/pdf")
	if !ok || repoURL != "https://github.com/anthropics/skills.git" || branch != "main" || subdir != "skills/pdf" {
		t.Fatalf("parseGitHubRepoSource(tree) = (%q, %q, %q, %v)", repoURL, branch, subdir, ok)
	}
	repoURL, branch, subdir, ok = parseGitHubRepoSource("anthropics/skills/skills/pdf")
	if !ok || repoURL != "https://github.com/anthropics/skills.git" || branch != "" || subdir != "skills/pdf" {
		t.Fatalf("parseGitHubRepoSource(shorthand) = (%q, %q, %q, %v)", repoURL, branch, subdir, ok)
	}
}
