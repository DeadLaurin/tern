package web

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite golden test fixtures")

// ---------------------------------------------------------------------------
// HTMLToMarkdown
// ---------------------------------------------------------------------------

func TestHTMLToMarkdownGolden(t *testing.T) {
	fixtures, err := filepath.Glob("testdata/*.html")
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no HTML fixtures found in testdata")
	}

	wantTitles := map[string]string{
		"article": "Go Concurrency & You",
		"inline":  "",
		"tables":  "",
	}

	for _, htmlPath := range fixtures {
		name := strings.TrimSuffix(filepath.Base(htmlPath), ".html")
		t.Run(name, func(t *testing.T) {
			source, err := os.ReadFile(htmlPath)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			title, got := HTMLToMarkdown(string(source))

			if want := wantTitles[name]; title != want {
				t.Errorf("title = %q, want %q", title, want)
			}

			goldenPath := strings.TrimSuffix(htmlPath, ".html") + ".golden.md"
			if *updateGolden {
				content := got
				if !strings.HasSuffix(content, "\n") {
					content += "\n"
				}
				if err := os.WriteFile(goldenPath, []byte(content), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
			}

			golden, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (run go test -run TestHTMLToMarkdownGolden -update): %v", err)
			}
			want := strings.TrimSuffix(string(golden), "\n")
			if got != want {
				t.Errorf("markdown mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

func TestHTMLToMarkdownRelativeLinks(t *testing.T) {
	doc := `<html><body><article>
		<p><a href="channels">channels</a></p>
		<p><a href="/docs/intro">intro</a></p>
		<p><img src="../img/logo.png" alt="Logo"></p>
		<p><a href="https://other.example/x">external</a></p>
	</article></body></html>`

	_, got := HTMLToMarkdownBase(doc, "https://example.com/articles/post")
	for _, want := range []string{
		"[channels](https://example.com/articles/channels)",
		"[intro](https://example.com/docs/intro)",
		"![Logo](https://example.com/img/logo.png)",
		"[external](https://other.example/x)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q:\n%s", want, got)
		}
	}
}

func TestHTMLToMarkdownSkipsNonContent(t *testing.T) {
	doc := `<html><head><title>Skip &amp; Test</title><style>.x{}</style></head>
	<body>
	<nav>navigation here</nav>
	<header>header here</header>
	<aside>aside here</aside>
	<footer>footer here</footer>
	<noscript>noscript here</noscript>
	<svg><text>svg here</text></svg>
	<!-- a comment that must not appear -->
	<script>var x = "script here";</script>
	<main><p>real content</p></main>
	</body></html>`

	title, got := HTMLToMarkdown(doc)
	if title != "Skip & Test" {
		t.Errorf("title = %q, want %q", title, "Skip & Test")
	}
	if !strings.Contains(got, "real content") {
		t.Errorf("missing real content: %q", got)
	}
	for _, unwanted := range []string{"navigation here", "header here", "aside here", "footer here", "noscript here", "svg here", "script here", "a comment"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("markdown contains skipped content %q: %q", unwanted, got)
		}
	}
	if strings.Contains(got, "Skip & Test") {
		t.Errorf("title leaked into body: %q", got)
	}
}

func TestHTMLToMarkdownNestedListsAndCodeFence(t *testing.T) {
	doc := `<ul><li>one<ul><li>two<ol><li>three</li></ol></li></ul></li></ul>
	<pre><code class="language-md">outer
` + "```" + `
inner fence
` + "```" + `
end</code></pre>`

	_, got := HTMLToMarkdown(doc)
	want := "- one\n  - two\n    1. three\n\n````md\nouter\n```\ninner fence\n```\nend\n````"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestStripHTML(t *testing.T) {
	got := stripHTML(`Hello <b>world</b> &amp; <i>friends</i><br>next`)
	want := "Hello world & friends next"
	if got != want {
		t.Errorf("stripHTML = %q, want %q", got, want)
	}
}

func TestTruncateOutput(t *testing.T) {
	got, truncated, original := truncateOutput("héllo wörld", 5)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if original != 11 {
		t.Errorf("original = %d, want 11", original)
	}
	want := "héllo\n\n[... truncated: showing 5 of 11 chars ...]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	got, truncated, original = truncateOutput("short", 10)
	if truncated || original != 5 || got != "short" {
		t.Errorf("unexpected non-truncated result: %q %v %d", got, truncated, original)
	}
}

// ---------------------------------------------------------------------------
// Fetch
// ---------------------------------------------------------------------------

func TestFetchHTML(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got == "" {
			t.Errorf("missing User-Agent")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Hello &amp; Welcome</title></head><body>
			<nav>skip me</nav>
			<article><h1>Hello</h1><p>World &amp; friends</p><script>bad()</script></article>
			</body></html>`)
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), server.URL, FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if result.URL != server.URL {
		t.Errorf("URL = %q, want %q", result.URL, server.URL)
	}
	if result.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d", result.StatusCode)
	}
	if !strings.HasPrefix(result.ContentType, "text/html") {
		t.Errorf("ContentType = %q", result.ContentType)
	}
	if result.Title != "Hello & Welcome" {
		t.Errorf("Title = %q, want %q", result.Title, "Hello & Welcome")
	}
	for _, want := range []string{"# Hello", "World & friends"} {
		if !strings.Contains(result.Markdown, want) {
			t.Errorf("Markdown missing %q:\n%s", want, result.Markdown)
		}
	}
	for _, unwanted := range []string{"skip me", "bad()"} {
		if strings.Contains(result.Markdown, unwanted) {
			t.Errorf("Markdown contains skipped %q:\n%s", unwanted, result.Markdown)
		}
	}
	if result.Truncated {
		t.Error("unexpected truncation")
	}
}

func TestFetchJSONPrettyPrinted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"tern","tags":["a","b"],"nested":{"ok":true}}`)
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), server.URL, FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, want := range []string{"{\n  \"name\": \"tern\",", "\n  \"nested\": {", "\n    \"ok\": true"} {
		if !strings.Contains(result.Markdown, want) {
			t.Errorf("pretty JSON missing %q:\n%s", want, result.Markdown)
		}
	}
}

func TestFetchPlainText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "line one\r\nline two  \r\n\r\nline four")
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), server.URL, FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := "line one\nline two\n\nline four"
	if result.Markdown != want {
		t.Errorf("Markdown = %q, want %q", result.Markdown, want)
	}
	if result.Title != "" {
		t.Errorf("Title = %q", result.Title)
	}
}

func TestFetchBinaryRejected(t *testing.T) {
	body := []byte{0x00, 0x01, 0xff, 0xfe, 0x42}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(body)
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), server.URL, FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(result.Markdown, "unsupported content type: application/octet-stream") {
		t.Errorf("missing metadata: %q", result.Markdown)
	}
	if !strings.Contains(result.Markdown, "not decoded") {
		t.Errorf("missing binary explanation: %q", result.Markdown)
	}
	if strings.ContainsRune(result.Markdown, rune(0x00)) || strings.Contains(result.Markdown, "\xff") {
		t.Errorf("binary garbage leaked: %q", result.Markdown)
	}
}

func TestFetchRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new", http.StatusFound)
	})
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>Final</title></head><body><h1>Arrived</h1></body></html>`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result, err := Fetch(context.Background(), server.URL+"/old", FetchOptions{MaxRedirects: 3})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if want := server.URL + "/new"; result.URL != want {
		t.Errorf("URL = %q, want %q", result.URL, want)
	}
	if result.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d", result.StatusCode)
	}
	if result.Title != "Final" {
		t.Errorf("Title = %q", result.Title)
	}
}

func TestFetchRedirectLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer server.Close()

	_, err := Fetch(context.Background(), server.URL, FetchOptions{MaxRedirects: 2})
	if err == nil {
		t.Fatal("expected redirect limit error")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error = %v, want redirect mention", err)
	}
}

func TestFetchTruncationMarker(t *testing.T) {
	body := strings.Repeat("abcdefghij", 10) // 100 chars
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, body)
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), server.URL, FetchOptions{MaxOutputChars: 20})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !result.Truncated {
		t.Fatal("Truncated = false")
	}
	if result.OriginalChars != 100 {
		t.Errorf("OriginalChars = %d, want 100", result.OriginalChars)
	}
	if !strings.HasPrefix(result.Markdown, "abcdefghijabcdefghij") {
		t.Errorf("unexpected prefix: %q", result.Markdown)
	}
	if !strings.Contains(result.Markdown, "[... truncated: showing 20 of 100 chars ...]") {
		t.Errorf("missing truncation marker: %q", result.Markdown)
	}
}

func TestFetchDownloadCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("x", 100))
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), server.URL, FetchOptions{MaxBytes: 10})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.HasPrefix(result.Markdown, strings.Repeat("x", 10)) {
		t.Errorf("expected exactly 10 body bytes, got %q", result.Markdown)
	}
	if !strings.Contains(result.Markdown, "[... download truncated at 10 bytes ...]") {
		t.Errorf("missing download truncation note: %q", result.Markdown)
	}
}

func TestFetchTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Fetch(ctx, server.URL, FetchOptions{Timeout: 5 * time.Second})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestFetchCancelledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Fetch(ctx, server.URL, FetchOptions{})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestFetchInvalidURL(t *testing.T) {
	for _, raw := range []string{"", "ftp://example.com/file", "file:///etc/passwd", "example.com", "https://"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := Fetch(context.Background(), raw, FetchOptions{}); err == nil {
				t.Fatalf("expected error for %q", raw)
			}
		})
	}
}

func TestFetchHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not here", http.StatusNotFound)
	}))
	defer server.Close()

	_, err := Fetch(context.Background(), server.URL, FetchOptions{})
	if err == nil {
		t.Fatal("expected HTTP error")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "not here") {
		t.Errorf("error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

func TestNewSearcher(t *testing.T) {
	client := &http.Client{}

	t.Run("brave requires key", func(t *testing.T) {
		t.Setenv(EnvBraveAPIKey, "")
		if _, err := NewSearcher("brave", "", "", client); err == nil {
			t.Fatal("expected missing key error")
		}
	})
	t.Run("brave env key", func(t *testing.T) {
		t.Setenv(EnvBraveAPIKey, "env-key")
		s, err := NewSearcher("brave", "", "", client)
		if err != nil {
			t.Fatalf("NewSearcher: %v", err)
		}
		b, ok := s.(*BraveSearcher)
		if !ok {
			t.Fatalf("type = %T", s)
		}
		if b.APIKey != "env-key" || b.Endpoint != DefaultBraveEndpoint {
			t.Errorf("unexpected searcher: %+v", b)
		}
	})
	t.Run("tavily requires key", func(t *testing.T) {
		t.Setenv(EnvTavilyAPIKey, "")
		if _, err := NewSearcher("tavily", "", "", client); err == nil {
			t.Fatal("expected missing key error")
		}
	})
	t.Run("tavily env key", func(t *testing.T) {
		t.Setenv(EnvTavilyAPIKey, "tvly-env")
		s, err := NewSearcher("Tavily", "", "", client)
		if err != nil {
			t.Fatalf("NewSearcher: %v", err)
		}
		if s.(*TavilySearcher).APIKey != "tvly-env" {
			t.Errorf("unexpected key")
		}
	})
	t.Run("searxng requires endpoint", func(t *testing.T) {
		if _, err := NewSearcher("searxng", "", "", client); err == nil {
			t.Fatal("expected endpoint error")
		}
	})
	t.Run("searxng", func(t *testing.T) {
		s, err := NewSearcher("searxng", "https://searx.example/", "", client)
		if err != nil {
			t.Fatalf("NewSearcher: %v", err)
		}
		if s.(*SearXNGSearcher).Endpoint != "https://searx.example/" {
			t.Errorf("unexpected endpoint")
		}
	})
	t.Run("duckduckgo default", func(t *testing.T) {
		s, err := NewSearcher("duckduckgo", "", "", client)
		if err != nil {
			t.Fatalf("NewSearcher: %v", err)
		}
		if s.(*DuckDuckGoSearcher).Endpoint != DefaultDuckDuckGoEndpoint {
			t.Errorf("unexpected endpoint")
		}
	})
	t.Run("unknown", func(t *testing.T) {
		if _, err := NewSearcher("google", "", "", client); err == nil {
			t.Fatal("expected unknown backend error")
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := NewSearcher("", "", "", client); err == nil {
			t.Fatal("expected empty backend error")
		}
	})
	t.Run("nil client gets default", func(t *testing.T) {
		s, err := NewSearcher("duckduckgo", "", "", nil)
		if err != nil {
			t.Fatalf("NewSearcher: %v", err)
		}
		if s.(*DuckDuckGoSearcher).Client == nil {
			t.Error("expected non-nil default client")
		}
	})
}

func TestBraveSearcher(t *testing.T) {
	var gotQuery, gotCount, gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		gotCount = r.URL.Query().Get("count")
		gotToken = r.Header.Get("X-Subscription-Token")
		if got := r.Header.Get("Accept"); !strings.Contains(got, "application/json") {
			t.Errorf("Accept = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"web":{"results":[
			{"title":"Go <b>docs</b>","url":"https://go.dev/doc/","description":"The <b>Go</b> language &amp; docs."},
			{"title":"Second","url":"https://example.com/2","description":"Two"}
		]}}`)
	}))
	defer server.Close()

	s, err := NewSearcher("brave", server.URL, "secret", server.Client())
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	results, err := s.Search(context.Background(), "golang", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotQuery != "golang" || gotCount != "5" || gotToken != "secret" {
		t.Errorf("query=%q count=%q token=%q", gotQuery, gotCount, gotToken)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0] != (SearchResult{Title: "Go docs", URL: "https://go.dev/doc/", Snippet: "The Go language & docs."}) {
		t.Errorf("result[0] = %+v", results[0])
	}
}

func TestBraveSearcherErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer server.Close()

	s := &BraveSearcher{APIKey: "bad", Endpoint: server.URL, Client: server.Client()}
	if _, err := s.Search(context.Background(), "x", 1); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want HTTP 401", err)
	}
}

func TestTavilySearcher(t *testing.T) {
	var payload map[string]any
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[
			{"title":"Tern","url":"https://tern.example","content":"An <b>agent</b> &amp; tools."},
			{"title":"Other","url":"https://other.example","content":"More"}
		]}`)
	}))
	defer server.Close()

	s, err := NewSearcher("tavily", server.URL, "tvly-key", server.Client())
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	results, err := s.Search(context.Background(), "tern agent", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if payload["query"] != "tern agent" {
		t.Errorf("query = %v", payload["query"])
	}
	if payload["max_results"] != float64(1) {
		t.Errorf("max_results = %v", payload["max_results"])
	}
	if payload["api_key"] != "tvly-key" || auth != "Bearer tvly-key" {
		t.Errorf("api_key=%v auth=%q", payload["api_key"], auth)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (maxResults)", len(results))
	}
	if results[0].Snippet != "An agent & tools." {
		t.Errorf("snippet = %q", results[0].Snippet)
	}
}

func TestSearXNGSearcher(t *testing.T) {
	var gotPath, gotQuery, gotFormat string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("q")
		gotFormat = r.URL.Query().Get("format")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[
			{"title":"First <i>hit</i>","url":"https://one.example","content":"Snippet &amp; one"},
			{"title":"Second","url":"https://two.example","content":"Snippet two"}
		]}`)
	}))
	defer server.Close()

	s, err := NewSearcher("searxng", server.URL+"/", "", server.Client())
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	results, err := s.Search(context.Background(), "hello world", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/search" || gotQuery != "hello world" || gotFormat != "json" {
		t.Errorf("path=%q query=%q format=%q", gotPath, gotQuery, gotFormat)
	}
	if len(results) != 2 || results[0].Title != "First hit" || results[0].Snippet != "Snippet & one" {
		t.Errorf("results = %+v", results)
	}
}

func TestDuckDuckGoSearcher(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body>
			<div class="result results_links">
				<h2><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F&amp;rut=abc">Go <b>Documentation</b></a></h2>
				<a class="result__snippet" href="#">The <b>Go</b> programming language &amp; docs.</a>
			</div>
			<div class="result results_links">
				<h2><a class="result__a" href="https://example.com/direct">Direct &amp; Simple</a></h2>
				<div class="result__snippet">A direct result with <span>markup</span>.</div>
			</div>
		</body></html>`)
	}))
	defer server.Close()

	s, err := NewSearcher("duckduckgo", server.URL, "", server.Client())
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	results, err := s.Search(context.Background(), "golang docs", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotQuery != "golang docs" {
		t.Errorf("query = %q", gotQuery)
	}
	want := []SearchResult{
		{Title: "Go Documentation", URL: "https://go.dev/doc/", Snippet: "The Go programming language & docs."},
		{Title: "Direct & Simple", URL: "https://example.com/direct", Snippet: "A direct result with markup."},
	}
	if len(results) != len(want) {
		t.Fatalf("len(results) = %d, want %d: %+v", len(results), len(want), results)
	}
	for i := range want {
		if results[i] != want[i] {
			t.Errorf("result[%d] = %+v, want %+v", i, results[i], want[i])
		}
	}
}

func TestDuckDuckGoSearcherLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, `<a class="result__a" href="https://example.com/%d">Result %d</a>
				<div class="result__snippet">Snippet %d</div>`, i, i, i)
		}
	}))
	defer server.Close()

	s := &DuckDuckGoSearcher{Endpoint: server.URL, Client: server.Client()}
	results, err := s.Search(context.Background(), "x", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called")
	}))
	defer server.Close()

	searchers := []Searcher{
		&BraveSearcher{APIKey: "k", Endpoint: server.URL, Client: server.Client()},
		&TavilySearcher{APIKey: "k", Endpoint: server.URL, Client: server.Client()},
		&SearXNGSearcher{Endpoint: server.URL, Client: server.Client()},
		&DuckDuckGoSearcher{Endpoint: server.URL, Client: server.Client()},
	}
	for _, s := range searchers {
		if _, err := s.Search(context.Background(), "  ", 5); err == nil {
			t.Errorf("%T: expected empty query error", s)
		}
	}
}
