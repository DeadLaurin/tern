package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLatestReleaseDecodesTagAndBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]string{
			"tag_name": "v1.2.3",
			"body":     "## What's changed\n\n- a thing\n- another thing\n",
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("encode payload: %v", err)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	release, err := latestRelease(t.Context(), server.Client(), server.URL+"/api", "o/r")
	if err != nil {
		t.Fatalf("latestRelease: %v", err)
	}
	if release.Tag != "v1.2.3" {
		t.Fatalf("tag = %q, want v1.2.3", release.Tag)
	}
	if !strings.Contains(release.Body, "- another thing") {
		t.Fatalf("body = %q, want release notes", release.Body)
	}

	tag, err := latestTag(t.Context(), server.Client(), server.URL+"/api", "o/r")
	if err != nil {
		t.Fatalf("latestTag: %v", err)
	}
	if tag != "v1.2.3" {
		t.Fatalf("latestTag = %q, want v1.2.3", tag)
	}
}

func TestLatestReleaseRequiresTag(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"","body":"orphan notes"}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	if _, err := latestRelease(t.Context(), server.Client(), server.URL+"/api", "o/r"); err == nil {
		t.Fatal("expected error for empty tag_name")
	}
}

func TestFormatReleaseNotes(t *testing.T) {
	t.Run("trims and indents", func(t *testing.T) {
		got := formatReleaseNotes("\n\n  \n- first\n- second\n\n\n\n", 16)
		want := "  - first\n  - second"
		if got != want {
			t.Fatalf("formatReleaseNotes = %q, want %q", got, want)
		}
	})

	t.Run("collapses blank runs", func(t *testing.T) {
		got := formatReleaseNotes("one\n\n\n\n two", 16)
		want := "  one\n\n   two"
		if got != want {
			t.Fatalf("formatReleaseNotes = %q, want %q", got, want)
		}
	})

	t.Run("normalizes CRLF", func(t *testing.T) {
		got := formatReleaseNotes("one\r\n\r\ntwo\r\n", 16)
		want := "  one\n\n  two"
		if got != want {
			t.Fatalf("formatReleaseNotes = %q, want %q", got, want)
		}
	})

	t.Run("truncates with ellipsis", func(t *testing.T) {
		body := "line1\nline2\nline3\nline4"
		got := formatReleaseNotes(body, 2)
		want := "  line1\n  line2\n  ..."
		if got != want {
			t.Fatalf("formatReleaseNotes = %q, want %q", got, want)
		}

		exact := formatReleaseNotes("line1\nline2", 2)
		if strings.Contains(exact, "...") {
			t.Fatalf("unexpected ellipsis for exact fit: %q", exact)
		}
	})

	t.Run("truncation does not end on blank line", func(t *testing.T) {
		got := formatReleaseNotes("a\nb\n\nc", 3)
		want := "  a\n  b\n  ..."
		if got != want {
			t.Fatalf("formatReleaseNotes = %q, want %q", got, want)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		if got := formatReleaseNotes(" \n\n\t\n", 16); got != "" {
			t.Fatalf("formatReleaseNotes = %q, want empty", got)
		}
		if got := printNotesForTest("", "v1.2.3"); got != "" {
			t.Fatalf("printReleaseNotes wrote %q for empty body", got)
		}
	})
}

func printNotesForTest(body, tag string) string {
	var buf bytes.Buffer
	printReleaseNotes(&buf, tag, body)
	return buf.String()
}

func TestUpdateCheckPrintsReleaseNotes(t *testing.T) {
	body := "## Highlights\n\n- shiny new thing\n- squashed a bug\n"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"tag_name": "v1.2.3",
			"body":     body,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("TERN_UPDATE_REPO", "o/r")

	oldAPI := githubAPIBase
	githubAPIBase = server.URL + "/api"
	t.Cleanup(func() { githubAPIBase = oldAPI })

	oldVersion := version
	t.Cleanup(func() { version = oldVersion })

	version = "v1.2.2"
	o := testRunOptions(t)
	o.Args = []string{"update", "--check"}
	if code := runWithOptions(o); code != 0 {
		t.Fatalf("update --check exit %d, stderr: %s", code, o.Err.(*bytes.Buffer).String())
	}
	out := o.Out.(*bytes.Buffer).String()
	for _, want := range []string{
		"Update available: v1.2.3",
		"What's new in v1.2.3:",
		"  - shiny new thing",
		"  - squashed a bug",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("update --check output missing %q:\n%s", want, out)
		}
	}

	version = "v1.2.3"
	upToDate := testRunOptions(t)
	upToDate.Args = []string{"update", "--check"}
	if code := runWithOptions(upToDate); code != 0 {
		t.Fatalf("update --check (latest) exit %d, stderr: %s", code, upToDate.Err.(*bytes.Buffer).String())
	}
	upToDateOut := upToDate.Out.(*bytes.Buffer).String()
	if !strings.Contains(upToDateOut, "tern is already up to date (v1.2.3)") {
		t.Fatalf("update --check (latest) output = %s", upToDateOut)
	}
	if strings.Contains(upToDateOut, "What's new") {
		t.Fatalf("up-to-date check printed release notes:\n%s", upToDateOut)
	}
}

func TestUpdateInstallPrintsReleaseNotes(t *testing.T) {
	asset, archive := testReleaseArchive(t)
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), asset)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"tag_name": "v1.2.3",
			"body":     "- fresh binary\n",
		})
	})
	mux.HandleFunc("/dl/o/r/releases/download/v1.2.3/"+asset, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/dl/o/r/releases/download/v1.2.3/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, checksums)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("TERN_UPDATE_REPO", "o/r")

	oldAPI, oldDownload := githubAPIBase, githubDownloadBase
	githubAPIBase = server.URL + "/api"
	githubDownloadBase = server.URL + "/dl"
	t.Cleanup(func() { githubAPIBase, githubDownloadBase = oldAPI, oldDownload })

	oldVersion := version
	version = "dev"
	t.Cleanup(func() { version = oldVersion })

	target := filepath.Join(t.TempDir(), "tern")
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExecutable := executablePath
	executablePath = func() (string, error) { return target, nil }
	t.Cleanup(func() { executablePath = oldExecutable })

	o := testRunOptions(t)
	o.Args = []string{"update"}
	if code := runWithOptions(o); code != 0 {
		t.Fatalf("update exit %d, stderr: %s", code, o.Err.(*bytes.Buffer).String())
	}
	out := o.Out.(*bytes.Buffer).String()
	updated := strings.Index(out, "Updated tern to v1.2.3.")
	if updated < 0 {
		t.Fatalf("update output missing confirmation:\n%s", out)
	}
	notes := strings.Index(out, "What's new in v1.2.3:")
	if notes < updated {
		t.Fatalf("release notes not printed after update confirmation:\n%s", out)
	}
	if !strings.Contains(out, "  - fresh binary") {
		t.Fatalf("update output missing release notes:\n%s", out)
	}
}
