// Package web provides network tools for tern: fetching web pages and
// converting them to Markdown, plus pluggable web search backends.
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Default fetch limits.
const (
	DefaultFetchTimeout   = 20 * time.Second
	DefaultMaxBytes       = 2 * 1024 * 1024
	DefaultMaxOutputChars = 40000
	DefaultMaxRedirects   = 5

	defaultUserAgent = "Mozilla/5.0 (compatible; tern/1.0; +https://github.com/tern-agent/tern)"
)

// FetchOptions configures Fetch. Zero values select the documented defaults.
type FetchOptions struct {
	// Timeout limits the whole request, including the body download.
	Timeout time.Duration
	// MaxBytes caps how many response body bytes are read.
	MaxBytes int64
	// MaxOutputChars caps the number of characters (runes) in FetchResult.
	MaxOutputChars int
	// MaxRedirects is the maximum number of redirects to follow.
	MaxRedirects int
	// UserAgent overrides the default User-Agent header.
	UserAgent string
}

// FetchResult is the outcome of fetching a URL.
type FetchResult struct {
	// URL is the final URL after redirects.
	URL string
	// StatusCode is the final HTTP status code.
	StatusCode int
	// ContentType is the media type reported by the server (or sniffed).
	ContentType string
	// Title is the HTML document title, when one exists.
	Title string
	// Markdown is the cleaned page content.
	Markdown string
	// Truncated reports whether Markdown was cut to MaxOutputChars.
	Truncated bool
	// OriginalChars is the number of characters before output truncation.
	OriginalChars int
}

func (o FetchOptions) withDefaults() FetchOptions {
	if o.Timeout <= 0 {
		o.Timeout = DefaultFetchTimeout
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxOutputChars <= 0 {
		o.MaxOutputChars = DefaultMaxOutputChars
	}
	if o.MaxRedirects <= 0 {
		o.MaxRedirects = DefaultMaxRedirects
	}
	if strings.TrimSpace(o.UserAgent) == "" {
		o.UserAgent = defaultUserAgent
	}
	return o
}

// Fetch downloads rawURL and returns its content converted for an LLM: HTML
// becomes Markdown, textual formats stay as text, and binary content becomes a
// short metadata description instead of raw bytes.
func Fetch(ctx context.Context, rawURL string, opts FetchOptions) (*FetchResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts = opts.withDefaults()

	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("web: invalid URL %q: %w", rawURL, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	case "":
		return nil, fmt.Errorf("web: invalid URL %q: missing scheme (want http or https)", rawURL)
	default:
		return nil, fmt.Errorf("web: invalid URL %q: unsupported scheme %q (want http or https)", rawURL, parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("web: invalid URL %q: missing host", rawURL)
	}

	maxRedirects := opts.MaxRedirects
	client := &http.Client{
		Timeout: opts.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("web: stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("web: build request: %w", err)
	}
	req.Header.Set("User-Agent", opts.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,text/markdown,application/json,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web: fetch %s: %w", parsed.String(), err)
	}
	defer resp.Body.Close()

	finalURL := parsed.String()
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read a small amount so the error can carry a useful server message.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		message := strings.Join(strings.Fields(string(snippet)), " ")
		if message != "" {
			return nil, fmt.Errorf("web: fetch %s: HTTP %d %s: %s", finalURL, resp.StatusCode, http.StatusText(resp.StatusCode), message)
		}
		return nil, fmt.Errorf("web: fetch %s: HTTP %d %s", finalURL, resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	limited := io.LimitReader(resp.Body, opts.MaxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("web: read %s: %w", finalURL, err)
	}
	downloadTruncated := int64(len(body)) > opts.MaxBytes
	if downloadTruncated {
		body = body[:opts.MaxBytes]
	}

	mediaType, contentType := responseMediaType(resp.Header.Get("Content-Type"), body)

	result := &FetchResult{
		URL:         finalURL,
		StatusCode:  resp.StatusCode,
		ContentType: contentType,
	}

	var content string
	switch {
	case isHTMLMediaType(mediaType):
		result.Title, content = HTMLToMarkdownBase(string(body), finalURL)
	case isJSONMediaType(mediaType):
		content = textBody(body, true)
	case isTextMediaType(mediaType):
		content = textBody(body, false)
	default:
		content = binaryMetadata(finalURL, resp.StatusCode, mediaType, len(body), downloadTruncated)
	}

	if downloadTruncated && !strings.HasPrefix(content, "[unsupported content type") {
		content += fmt.Sprintf("\n\n[... download truncated at %d bytes ...]", opts.MaxBytes)
	}

	result.Markdown, result.Truncated, result.OriginalChars = truncateOutput(content, opts.MaxOutputChars)
	return result, nil
}

// responseMediaType parses the Content-Type header, falling back to content
// sniffing when the header is absent or malformed. It returns both the bare
// media type and a display value.
func responseMediaType(header string, body []byte) (mediaType, display string) {
	header = strings.TrimSpace(header)
	if header != "" {
		if parsed, _, err := mime.ParseMediaType(header); err == nil && parsed != "" {
			return strings.ToLower(parsed), header
		}
		return strings.ToLower(strings.TrimSpace(strings.Split(header, ";")[0])), header
	}
	if len(body) > 512 {
		body = body[:512]
	}
	detected := http.DetectContentType(body)
	parsed, _, err := mime.ParseMediaType(detected)
	if err != nil || parsed == "" {
		parsed = detected
	}
	return strings.ToLower(parsed), detected
}

func isHTMLMediaType(mediaType string) bool {
	switch mediaType {
	case "text/html", "application/xhtml+xml":
		return true
	}
	return false
}

func isJSONMediaType(mediaType string) bool {
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func isTextMediaType(mediaType string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/xml", "application/xhtml+xml", "application/javascript",
		"application/x-javascript", "application/sql", "application/graphql",
		"application/yaml", "application/x-yaml", "application/toml":
		return true
	}
	return false
}

// textBody converts a textual response body to clean UTF-8 text, optionally
// pretty-printing small JSON documents.
func textBody(body []byte, prettyJSON bool) string {
	text := strings.ToValidUTF8(string(body), "\uFFFD")
	if prettyJSON {
		trimmed := bytes.TrimSpace([]byte(text))
		if len(trimmed) > 0 && len(trimmed) <= 512*1024 && json.Valid(trimmed) {
			var out bytes.Buffer
			if err := json.Indent(&out, trimmed, "", "  "); err == nil {
				text = out.String()
			}
		}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

func binaryMetadata(finalURL string, status int, mediaType string, bytesRead int, downloadTruncated bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[unsupported content type: %s]\n", mediaType)
	fmt.Fprintf(&b, "URL: %s\n", finalURL)
	fmt.Fprintf(&b, "Status: %d %s\n", status, http.StatusText(status))
	fmt.Fprintf(&b, "Content-Type: %s\n", mediaType)
	if downloadTruncated {
		fmt.Fprintf(&b, "Bytes: %d (download truncated)\n", bytesRead)
	} else {
		fmt.Fprintf(&b, "Bytes: %d\n", bytesRead)
	}
	b.WriteString("Binary or unsupported content was not decoded.")
	return b.String()
}

// truncateOutput cuts s to maxChars runes and appends a clear marker. It
// returns the resulting string, whether truncation occurred, and the original
// character count.
func truncateOutput(s string, maxChars int) (string, bool, int) {
	original := utf8.RuneCountInString(s)
	if maxChars <= 0 || original <= maxChars {
		return s, false, original
	}
	runes := []rune(s)
	shown := maxChars
	return fmt.Sprintf("%s\n\n[... truncated: showing %d of %d chars ...]", string(runes[:shown]), shown, original), true, original
}
