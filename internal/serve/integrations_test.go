package serve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initServeGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@tern.local")
	run("config", "user.name", "Tern Test")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "hello.txt")
	run("commit", "-m", "initial")
	return dir
}

func TestServeGitEndpoints(t *testing.T) {
	dir := initServeGitRepo(t)
	// Modify file + add wrangler.toml to test both Git and Cloudflare endpoints.
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wrangler.toml"), []byte("name = \"demo\"\npages_build_output_dir = \"dist\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := New(Options{WorkDir: dir})
	defer srv.Close()
	h := srv.Handler()

	// 1. GET /api/git/status?remote=0
	req := httptest.NewRequest(http.MethodGet, "/api/git/status?remote=0", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var stResp struct {
		Git struct {
			IsRepo bool   `json:"is_repo"`
			Branch string `json:"branch"`
			Files  []struct {
				Path string `json:"path"`
			} `json:"files"`
		} `json:"git"`
		Cloudflare struct {
			Detected    bool   `json:"detected"`
			Kind        string `json:"kind"`
			ProjectName string `json:"project_name"`
		} `json:"cloudflare"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stResp); err != nil {
		t.Fatal(err)
	}
	if !stResp.Git.IsRepo || stResp.Git.Branch != "main" || len(stResp.Git.Files) != 2 {
		t.Fatalf("unexpected git status: %+v", stResp.Git)
	}
	if !stResp.Cloudflare.Detected || stResp.Cloudflare.Kind != "pages" || stResp.Cloudflare.ProjectName != "demo" {
		t.Fatalf("unexpected cloudflare status: %+v", stResp.Cloudflare)
	}

	// 2. GET /api/git/diff?path=hello.txt
	req = httptest.NewRequest(http.MethodGet, "/api/git/diff?path=hello.txt", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "+world") {
		t.Fatalf("unexpected diff response: %d %s", rec.Code, rec.Body.String())
	}

	// 3. POST /api/git/commit
	body := bytes.NewBufferString(`{"message":"test: update hello","stage_all":true}`)
	req = httptest.NewRequest(http.MethodPost, "/api/git/commit", body)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("commit failed: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"files":[]`) {
		t.Fatalf("expected clean tree after commit, got %s", rec.Body.String())
	}
}
