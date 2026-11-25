package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Search backend identifiers accepted by NewSearcher.
const (
	BackendBrave      = "brave"
	BackendTavily     = "tavily"
	BackendSearXNG    = "searxng"
	BackendDuckDuckGo = "duckduckgo"
)

// Default endpoints for the supported search backends.
const (
	DefaultBraveEndpoint      = "https://api.search.brave.com/res/v1/web/search"
	DefaultTavilyEndpoint     = "https://api.tavily.com/search"
	DefaultDuckDuckGoEndpoint = "https://html.duckduckgo.com/html/"
)

// Environment variables consulted for API keys when none is supplied.
const (
	EnvBraveAPIKey  = "BRAVE_API_KEY"
	EnvTavilyAPIKey = "TAVILY_API_KEY"
)

const (
	defaultSearchTimeout = 30 * time.Second
	defaultMaxResults    = 10
	maxSearchBodyBytes   = 8 * 1024 * 1024
)

// SearchResult is one web search hit.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// Searcher is a pluggable web search backend.
type Searcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error)
}

// NewSearcher constructs a Searcher for the named backend. endpoint is an
// optional override of the backend's default URL. apiKey is optional for
// backends that need credentials when the matching environment variable is
// set. A nil client selects a default client with a 30s timeout.
func NewSearcher(backend, endpoint, apiKey string, client *http.Client) (Searcher, error) {
	if client == nil {
		client = &http.Client{Timeout: defaultSearchTimeout}
	}
	backend = strings.ToLower(strings.TrimSpace(backend))
	endpoint = strings.TrimSpace(endpoint)
	apiKey = strings.TrimSpace(apiKey)

	switch backend {
	case BackendBrave:
		if apiKey == "" {
			apiKey = strings.TrimSpace(envOr(EnvBraveAPIKey, ""))
		}
		if apiKey == "" {
			return nil, fmt.Errorf("web: brave search requires an API key (set %s or pass one explicitly)", EnvBraveAPIKey)
		}
		if endpoint == "" {
			endpoint = DefaultBraveEndpoint
		}
		return &BraveSearcher{APIKey: apiKey, Endpoint: endpoint, Client: client}, nil
	case BackendTavily:
		if apiKey == "" {
			apiKey = strings.TrimSpace(envOr(EnvTavilyAPIKey, ""))
		}
		if apiKey == "" {
			return nil, fmt.Errorf("web: tavily search requires an API key (set %s or pass one explicitly)", EnvTavilyAPIKey)
		}
		if endpoint == "" {
			endpoint = DefaultTavilyEndpoint
		}
		return &TavilySearcher{APIKey: apiKey, Endpoint: endpoint, Client: client}, nil
	case BackendSearXNG:
		if endpoint == "" {
			return nil, fmt.Errorf("web: searxng search requires an endpoint URL")
		}
		return &SearXNGSearcher{Endpoint: endpoint, Client: client}, nil
	case BackendDuckDuckGo, "ddg":
		if endpoint == "" {
			endpoint = DefaultDuckDuckGoEndpoint
		}
		return &DuckDuckGoSearcher{Endpoint: endpoint, Client: client}, nil
	case "":
		return nil, fmt.Errorf("web: search backend is required (one of %s, %s, %s, %s)", BackendBrave, BackendTavily, BackendSearXNG, BackendDuckDuckGo)
	default:
		return nil, fmt.Errorf("web: unknown search backend %q (want %s, %s, %s, or %s)", backend, BackendBrave, BackendTavily, BackendSearXNG, BackendDuckDuckGo)
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// searchClient returns a usable HTTP client even for directly constructed
// searchers.
func searchClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return &http.Client{Timeout: defaultSearchTimeout}
}

func normalizedMaxResults(maxResults int) int {
	if maxResults <= 0 {
		return defaultMaxResults
	}
	return maxResults
}

func newSearchRequest(ctx context.Context, method, endpoint string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "application/json, text/html;q=0.9, */*;q=0.8")
	return req, nil
}

func doSearchRequest(client *http.Client, req *http.Request, backend string) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web: %s search: %w", backend, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSearchBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("web: %s search: read response: %w", backend, err)
	}
	if int64(len(body)) > maxSearchBodyBytes {
		return nil, fmt.Errorf("web: %s search: response exceeds %d bytes", backend, maxSearchBodyBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.Join(strings.Fields(string(body)), " ")
		if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		if snippet != "" {
			return nil, fmt.Errorf("web: %s search: HTTP %d %s: %s", backend, resp.StatusCode, http.StatusText(resp.StatusCode), snippet)
		}
		return nil, fmt.Errorf("web: %s search: HTTP %d %s", backend, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return body, nil
}

func searchSnippet(s string) string {
	return strings.Join(strings.Fields(stripHTML(s)), " ")
}

func limitResults(results []SearchResult, maxResults int) []SearchResult {
	if maxResults > 0 && len(results) > maxResults {
		return results[:maxResults]
	}
	return results
}

func trimEndpoint(endpoint string) string {
	return strings.TrimRight(strings.TrimSpace(endpoint), "/")
}

// ---------------------------------------------------------------------------
// Brave
// ---------------------------------------------------------------------------

// BraveSearcher uses the Brave Search API.
type BraveSearcher struct {
	APIKey   string
	Endpoint string
	Client   *http.Client
}

type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
	Error *struct {
		Code   int    `json:"code"`
		Detail string `json:"detail"`
	} `json:"error"`
}

// Search implements Searcher.
func (s *BraveSearcher) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("web: brave search: query is empty")
	}
	endpoint := strings.TrimSpace(s.Endpoint)
	if endpoint == "" {
		endpoint = DefaultBraveEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("web: brave search: invalid endpoint: %w", err)
	}
	n := normalizedMaxResults(maxResults)
	if n > 20 {
		n = 20
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("count", strconv.Itoa(n))
	u.RawQuery = q.Encode()

	req, err := newSearchRequest(ctx, http.MethodGet, u.String())
	if err != nil {
		return nil, fmt.Errorf("web: brave search: %w", err)
	}
	req.Header.Set("X-Subscription-Token", s.APIKey)
	req.Header.Set("Accept", "application/json")

	body, err := doSearchRequest(searchClient(s.Client), req, "brave")
	if err != nil {
		return nil, err
	}
	var decoded braveResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("web: brave search: decode response: %w", err)
	}
	if decoded.Error != nil && decoded.Error.Detail != "" {
		return nil, fmt.Errorf("web: brave search: %s", decoded.Error.Detail)
	}

	results := make([]SearchResult, 0, len(decoded.Web.Results))
	for _, item := range decoded.Web.Results {
		results = append(results, SearchResult{
			Title:   searchSnippet(item.Title),
			URL:     item.URL,
			Snippet: searchSnippet(item.Description),
		})
	}
	return limitResults(results, maxResults), nil
}

// ---------------------------------------------------------------------------
// Tavily
// ---------------------------------------------------------------------------

// TavilySearcher uses the Tavily Search API.
type TavilySearcher struct {
	APIKey   string
	Endpoint string
	Client   *http.Client
}

type tavilyResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
	Error   string `json:"error"`
	Detail  string `json:"detail"`
	Message string `json:"message"`
}

// Search implements Searcher.
func (s *TavilySearcher) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("web: tavily search: query is empty")
	}
	endpoint := strings.TrimSpace(s.Endpoint)
	if endpoint == "" {
		endpoint = DefaultTavilyEndpoint
	}
	n := normalizedMaxResults(maxResults)
	if n > 20 {
		n = 20
	}

	payload := map[string]any{
		"query":          query,
		"max_results":    n,
		"search_depth":   "basic",
		"include_answer": false,
		"include_images": false,
	}
	if s.APIKey != "" {
		payload["api_key"] = s.APIKey
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("web: tavily search: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("web: tavily search: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if s.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.APIKey)
	}

	body, err := doSearchRequest(searchClient(s.Client), req, "tavily")
	if err != nil {
		return nil, err
	}
	var decoded tavilyResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("web: tavily search: decode response: %w", err)
	}
	if decoded.Error != "" {
		return nil, fmt.Errorf("web: tavily search: %s", decoded.Error)
	}
	if decoded.Detail != "" {
		return nil, fmt.Errorf("web: tavily search: %s", decoded.Detail)
	}
	if decoded.Message != "" && len(decoded.Results) == 0 {
		return nil, fmt.Errorf("web: tavily search: %s", decoded.Message)
	}

	results := make([]SearchResult, 0, len(decoded.Results))
	for _, item := range decoded.Results {
		results = append(results, SearchResult{
			Title:   searchSnippet(item.Title),
			URL:     item.URL,
			Snippet: searchSnippet(item.Content),
		})
	}
	return limitResults(results, maxResults), nil
}

// ---------------------------------------------------------------------------
// SearXNG
// ---------------------------------------------------------------------------

// SearXNGSearcher uses a SearXNG instance's JSON API.
type SearXNGSearcher struct {
	Endpoint string
	Client   *http.Client
}

type searxResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
	Error string `json:"error"`
}

// Search implements Searcher.
func (s *SearXNGSearcher) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("web: searxng search: query is empty")
	}
	endpoint := trimEndpoint(s.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("web: searxng search: endpoint is required")
	}

	u, err := url.Parse(endpoint + "/search")
	if err != nil {
		return nil, fmt.Errorf("web: searxng search: invalid endpoint: %w", err)
	}
	q := u.Query()
	q.Set("format", "json")
	q.Set("q", query)
	u.RawQuery = q.Encode()

	req, err := newSearchRequest(ctx, http.MethodGet, u.String())
	if err != nil {
		return nil, fmt.Errorf("web: searxng search: %w", err)
	}
	body, err := doSearchRequest(searchClient(s.Client), req, "searxng")
	if err != nil {
		return nil, err
	}
	var decoded searxResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("web: searxng search: decode response: %w", err)
	}
	if decoded.Error != "" {
		return nil, fmt.Errorf("web: searxng search: %s", decoded.Error)
	}

	results := make([]SearchResult, 0, len(decoded.Results))
	for _, item := range decoded.Results {
		results = append(results, SearchResult{
			Title:   searchSnippet(item.Title),
			URL:     item.URL,
			Snippet: searchSnippet(item.Content),
		})
	}
	return limitResults(results, maxResults), nil
}

// ---------------------------------------------------------------------------
// DuckDuckGo (HTML endpoint)
// ---------------------------------------------------------------------------

// DuckDuckGoSearcher scrapes DuckDuckGo's no-JavaScript HTML endpoint.
type DuckDuckGoSearcher struct {
	Endpoint string
	Client   *http.Client
}

// Search implements Searcher.
func (s *DuckDuckGoSearcher) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("web: duckduckgo search: query is empty")
	}
	endpoint := strings.TrimSpace(s.Endpoint)
	if endpoint == "" {
		endpoint = DefaultDuckDuckGoEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("web: duckduckgo search: invalid endpoint: %w", err)
	}
	q := u.Query()
	q.Set("q", query)
	u.RawQuery = q.Encode()

	req, err := newSearchRequest(ctx, http.MethodGet, u.String())
	if err != nil {
		return nil, fmt.Errorf("web: duckduckgo search: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	body, err := doSearchRequest(searchClient(s.Client), req, "duckduckgo")
	if err != nil {
		return nil, err
	}
	results := parseDuckDuckGoHTML(string(body), u)
	return limitResults(results, maxResults), nil
}

// parseDuckDuckGoHTML extracts result titles, URLs, and snippets from the
// DuckDuckGo HTML endpoint. It uses the streaming tokenizer rather than
// regular expressions so nested markup in titles is handled correctly.
func parseDuckDuckGoHTML(document string, base *url.URL) []SearchResult {
	var results []SearchResult
	var current *SearchResult

	var titleDepth int
	var title strings.Builder
	var snippetDepth int
	var snippetTag string
	var snippet strings.Builder

	flush := func() {
		if current == nil {
			return
		}
		current.Title = strings.Join(strings.Fields(current.Title), " ")
		current.Snippet = strings.Join(strings.Fields(current.Snippet), " ")
		if current.URL != "" {
			results = append(results, *current)
		}
		current = nil
	}

	rest := document
	for len(rest) > 0 {
		tok, next := nextHTMLToken(rest)
		rest = next

		// Collecting a result title.
		if titleDepth > 0 {
			switch tok.kind {
			case tokHTMLStart:
				if tok.name == "a" && !tok.selfClosing {
					titleDepth++
				}
			case tokHTMLEnd:
				if tok.name == "a" {
					titleDepth--
					if titleDepth == 0 && current != nil {
						current.Title = html.UnescapeString(title.String())
					}
				}
			case tokHTMLText:
				title.WriteString(html.UnescapeString(tok.text))
			}
			continue
		}

		// Collecting a result snippet.
		if snippetDepth > 0 {
			switch tok.kind {
			case tokHTMLStart:
				if tok.name == snippetTag && !tok.selfClosing {
					snippetDepth++
				}
			case tokHTMLEnd:
				if tok.name == snippetTag {
					snippetDepth--
					if snippetDepth == 0 && current != nil {
						current.Snippet = html.UnescapeString(snippet.String())
						snippet.Reset()
					}
				}
			case tokHTMLText:
				snippet.WriteString(html.UnescapeString(tok.text))
			}
			continue
		}

		switch tok.kind {
		case tokHTMLStart:
			class := tok.attrs["class"]
			if tok.name == "a" && hasHTMLClass(class, "result__a") {
				flush()
				current = &SearchResult{URL: unwrapDuckDuckGoURL(tok.attrs["href"], base)}
				title.Reset()
				titleDepth = 1
				continue
			}
			if current != nil && hasHTMLClass(class, "result__snippet") {
				snippet.Reset()
				snippetTag = tok.name
				if tok.selfClosing {
					current.Snippet = ""
				} else {
					snippetDepth = 1
				}
			}
		}
	}
	flush()
	return results
}

func hasHTMLClass(classes, want string) bool {
	for _, class := range strings.Fields(classes) {
		if class == want {
			return true
		}
	}
	return false
}

// unwrapDuckDuckGoURL resolves DuckDuckGo redirect URLs of the form
// /l/?uddg=<encoded target> and protocol-relative links.
func unwrapDuckDuckGoURL(href string, base *url.URL) string {
	href = strings.TrimSpace(html.UnescapeString(href))
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	} else if base != nil {
		if ref, err := url.Parse(href); err == nil && !ref.IsAbs() {
			href = base.ResolveReference(ref).String()
		}
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if target := u.Query().Get("uddg"); target != "" {
		return target
	}
	return href
}
