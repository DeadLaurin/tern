package subagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tern-agent/tern/internal/tools"
	"github.com/tern-agent/tern/internal/web"
)

// Tool names provided by this package (the built-in tools themselves live in
// internal/tools).
const (
	ToolWebSearch = "web_search"
	ToolWebFetch  = "web_fetch"
	ToolTask      = "task"
)

// WebSearchTool exposes internal/web's pluggable searcher to a sub-agent.
type WebSearchTool struct {
	Searcher web.Searcher
	Backend  string
	Endpoint string
}

// NewWebSearchTool returns a search tool. A nil searcher is constructed lazily
// from Backend/Endpoint on first use.
func NewWebSearchTool(searcher web.Searcher, backend, endpoint string) *WebSearchTool {
	return &WebSearchTool{Searcher: searcher, Backend: backend, Endpoint: endpoint}
}

func (t *WebSearchTool) Name() string                { return ToolWebSearch }
func (t *WebSearchTool) IsReadOnly() bool            { return true }
func (t *WebSearchTool) IsWeb() bool                 { return true }
func (t *WebSearchTool) IsParallelSafe() bool        { return true }
func (t *WebSearchTool) Parameters() json.RawMessage { return webSearchSchema }
func (t *WebSearchTool) Description() string {
	return "Search the public web and return titles, URLs and snippets. Use focused queries; follow up with web_fetch on the most relevant URLs."
}

var webSearchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Search query."},
    "max_results": {"type": "integer", "description": "Maximum number of results (default 8).", "minimum": 1, "maximum": 20}
  },
  "required": ["query"],
  "additionalProperties": false
}`)

type webSearchArgs struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

func (t *WebSearchTool) Execute(ctx context.Context, _ tools.ExecEnv, raw json.RawMessage) (tools.Result, error) {
	var args webSearchArgs
	if err := strictJSON(raw, &args); err != nil {
		return tools.Result{}, fmt.Errorf("web_search: invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return tools.Result{}, fmt.Errorf(`web_search: missing required argument "query"`)
	}
	searcher := t.Searcher
	if searcher == nil {
		backend := strings.TrimSpace(t.Backend)
		if backend == "" {
			backend = web.BackendDuckDuckGo
		}
		var err error
		searcher, err = web.NewSearcher(backend, t.Endpoint, "", nil)
		if err != nil {
			return tools.Result{}, fmt.Errorf("web_search: %w", err)
		}
	}
	maxResults := args.MaxResults
	if maxResults <= 0 {
		maxResults = 8
	}
	results, err := searcher.Search(ctx, args.Query, maxResults)
	if err != nil {
		return tools.Result{}, fmt.Errorf("web_search: %w", err)
	}
	if len(results) == 0 {
		return tools.Result{Output: fmt.Sprintf("no results for %q", args.Query), External: true}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Search results for %q:\n", args.Query)
	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), r.URL)
		if snippet := strings.TrimSpace(r.Snippet); snippet != "" {
			fmt.Fprintf(&b, "   %s\n", snippet)
		}
	}
	return tools.Result{
		Output:   b.String(),
		External: true,
		Metadata: map[string]any{"query": args.Query, "results": len(results)},
	}, nil
}

// WebFetchTool fetches a URL and converts it to Markdown.
type WebFetchTool struct {
	Options web.FetchOptions
}

// NewWebFetchTool returns a fetch tool with default options.
func NewWebFetchTool() *WebFetchTool { return &WebFetchTool{} }

func (t *WebFetchTool) Name() string                { return ToolWebFetch }
func (t *WebFetchTool) IsReadOnly() bool            { return true }
func (t *WebFetchTool) IsWeb() bool                 { return true }
func (t *WebFetchTool) IsParallelSafe() bool        { return true }
func (t *WebFetchTool) Parameters() json.RawMessage { return webFetchSchema }
func (t *WebFetchTool) Description() string {
	return "Fetch a public URL and return its content converted to Markdown, together with the final URL, status and title."
}

var webFetchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Absolute http(s) URL to fetch."},
    "max_chars": {"type": "integer", "description": "Maximum characters of Markdown to return (default 40000).", "minimum": 1}
  },
  "required": ["url"],
  "additionalProperties": false
}`)

type webFetchArgs struct {
	URL      string `json:"url"`
	MaxChars int    `json:"max_chars"`
}

func (t *WebFetchTool) Execute(ctx context.Context, _ tools.ExecEnv, raw json.RawMessage) (tools.Result, error) {
	var args webFetchArgs
	if err := strictJSON(raw, &args); err != nil {
		return tools.Result{}, fmt.Errorf("web_fetch: invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.URL) == "" {
		return tools.Result{}, fmt.Errorf(`web_fetch: missing required argument "url"`)
	}
	opts := t.Options
	if args.MaxChars > 0 {
		opts.MaxOutputChars = args.MaxChars
	}
	result, err := web.Fetch(ctx, args.URL, opts)
	if err != nil {
		return tools.Result{}, fmt.Errorf("web_fetch: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "URL: %s\nStatus: %d\n", result.URL, result.StatusCode)
	if result.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", result.Title)
	}
	if result.Truncated {
		fmt.Fprintf(&b, "Truncated: yes (%d chars before truncation)\n", result.OriginalChars)
	}
	b.WriteString("\n")
	b.WriteString(result.Markdown)
	return tools.Result{
		Output:   b.String(),
		External: true,
		Metadata: map[string]any{
			"url":          result.URL,
			"status":       result.StatusCode,
			"title":        result.Title,
			"content_type": result.ContentType,
		},
	}, nil
}

func strictJSON(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
