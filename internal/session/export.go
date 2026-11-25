package session

import (
	"fmt"
	"html/template"
	"io"
	"strings"

	"github.com/tern-agent/tern/internal/provider"
)

// exportData is the view model shared by the Markdown and HTML exporters.
type exportData struct {
	Meta       SessionMeta
	Name       string
	Messages   []exportMessage
	Subagents  []exportSubagent
	Usage      provider.Usage
	CostUSD    float64
	TotalUsage provider.Usage
	TotalCost  float64
	CreatedAt  string
}

type exportMessage struct {
	Role       string
	Content    string
	Reasoning  string
	ToolCallID string
	External   bool
	ToolCalls  []exportToolCall
}

type exportToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type exportSubagent struct {
	Link     SubagentLink
	Meta     SessionMeta
	Messages []exportMessage
	Usage    provider.Usage
	CostUSD  float64
}

// ExportMarkdown writes a readable Markdown transcript, including tool calls,
// fenced reasoning and linked sub-agent transcripts.
func (s *Session) ExportMarkdown(w io.Writer) error {
	data, err := s.exportData()
	if err != nil {
		return err
	}
	var b strings.Builder

	name := data.Name
	if name == "" {
		name = data.Meta.ID
	}
	fmt.Fprintf(&b, "# %s\n\n", name)
	fmt.Fprintf(&b, "- **Session:** `%s`\n", data.Meta.ID)
	if data.Meta.Model != "" {
		fmt.Fprintf(&b, "- **Model:** `%s`\n", data.Meta.Model)
	}
	if data.Meta.WorkDir != "" {
		fmt.Fprintf(&b, "- **Working directory:** `%s`\n", data.Meta.WorkDir)
	}
	if data.Meta.ParentSessionID != "" {
		fmt.Fprintf(&b, "- **Parent session:** `%s`\n", data.Meta.ParentSessionID)
	}
	if data.Meta.ParentToolCallID != "" {
		fmt.Fprintf(&b, "- **Parent tool call:** `%s`\n", data.Meta.ParentToolCallID)
	}
	if data.Meta.AgentType != "" {
		fmt.Fprintf(&b, "- **Agent type:** `%s`\n", data.Meta.AgentType)
	}
	if data.CreatedAt != "" {
		fmt.Fprintf(&b, "- **Created:** %s\n", data.CreatedAt)
	}
	fmt.Fprintf(&b, "- **Usage:** %d prompt / %d completion / %d total tokens (%.6f USD)\n",
		data.TotalUsage.PromptTokens, data.TotalUsage.CompletionTokens,
		data.TotalUsage.TotalTokens, data.TotalCost)
	b.WriteString("\n")

	if strings.TrimSpace(data.Meta.SystemPrompt) != "" {
		b.WriteString("## System prompt\n\n")
		b.WriteString(fence(data.Meta.SystemPrompt, "text"))
		b.WriteString("\n")
	}

	b.WriteString("## Transcript\n\n")
	for _, msg := range data.Messages {
		writeMarkdownMessage(&b, msg)
	}

	if len(data.Subagents) > 0 {
		b.WriteString("## Sub-agents\n\n")
		for _, sub := range data.Subagents {
			writeMarkdownSubagent(&b, sub)
		}
	}

	_, err = io.WriteString(w, b.String())
	return err
}

func writeMarkdownMessage(b *strings.Builder, msg exportMessage) {
	role := msg.Role
	if role == "" {
		role = "message"
	}
	label := strings.ToUpper(role[:1]) + role[1:]
	fmt.Fprintf(b, "### %s", label)
	if msg.External {
		b.WriteString(" (external)")
	}
	b.WriteString("\n\n")

	if strings.TrimSpace(msg.Reasoning) != "" {
		b.WriteString("<details>\n<summary>Reasoning</summary>\n\n")
		b.WriteString(fence(msg.Reasoning, "text"))
		b.WriteString("\n</details>\n\n")
	}
	if strings.TrimSpace(msg.Content) != "" {
		b.WriteString(msg.Content)
		b.WriteString("\n\n")
	}
	if msg.ToolCallID != "" {
		fmt.Fprintf(b, "_Tool call: `%s`_\n\n", msg.ToolCallID)
	}
	for _, call := range msg.ToolCalls {
		fmt.Fprintf(b, "**Tool call:** `%s`", call.Name)
		if call.ID != "" {
			fmt.Fprintf(b, " (`%s`)", call.ID)
		}
		b.WriteString("\n\n")
		args := call.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		b.WriteString(fence(args, "json"))
		b.WriteString("\n")
	}
}

func writeMarkdownSubagent(b *strings.Builder, sub exportSubagent) {
	title := sub.Link.Description
	if title == "" {
		title = sub.Link.AgentID
	}
	fmt.Fprintf(b, "### Sub-agent `%s` — %s\n\n", sub.Link.AgentID, title)
	if sub.Link.AgentType != "" {
		fmt.Fprintf(b, "- **Type:** `%s`\n", sub.Link.AgentType)
	}
	if sub.Link.ParentToolCallID != "" {
		fmt.Fprintf(b, "- **Parent tool call:** `%s`\n", sub.Link.ParentToolCallID)
	}
	if sub.Link.Status != "" {
		fmt.Fprintf(b, "- **Status:** %s\n", sub.Link.Status)
	}
	if sub.Link.DurationMs > 0 {
		fmt.Fprintf(b, "- **Duration:** %d ms\n", sub.Link.DurationMs)
	}
	fmt.Fprintf(b, "- **Usage:** %d total tokens (%.6f USD)\n", sub.Usage.TotalTokens, sub.CostUSD)
	if sub.Link.Summary != "" {
		fmt.Fprintf(b, "- **Summary:** %s\n", sub.Link.Summary)
	}
	b.WriteString("\n")
	if strings.TrimSpace(sub.Meta.SystemPrompt) != "" {
		b.WriteString("<details>\n<summary>System prompt</summary>\n\n")
		b.WriteString(fence(sub.Meta.SystemPrompt, "text"))
		b.WriteString("\n</details>\n\n")
	}
	for _, msg := range sub.Messages {
		writeMarkdownMessage(b, msg)
	}
}

// fence wraps text in a fenced code block, extending the fence when the body
// itself contains backticks.
func fence(text, lang string) string {
	f := "```"
	for strings.Contains(text, f) {
		f += "`"
	}
	return f + lang + "\n" + text + "\n" + f + "\n"
}

// ExportHTML writes a self-contained, escaped HTML transcript. Reasoning and
// sub-agent transcripts are collapsible <details> blocks.
func (s *Session) ExportHTML(w io.Writer) error {
	data, err := s.exportData()
	if err != nil {
		return err
	}
	return htmlTemplate.Execute(w, data)
}

func (s *Session) exportData() (*exportData, error) {
	meta := s.Meta()
	msgs, flags := s.ReplayMessagesWithFlags()
	usage, cost := s.Usage()
	totalUsage, totalCost := s.TotalUsage()

	data := &exportData{
		Meta:       meta,
		Name:       s.Name(),
		Messages:   toExportMessages(msgs, flags),
		Usage:      usage,
		CostUSD:    cost,
		TotalUsage: totalUsage,
		TotalCost:  totalCost,
	}
	if !meta.CreatedAt.IsZero() {
		data.CreatedAt = meta.CreatedAt.Format("2006-01-02 15:04:05 MST")
	}

	children, err := s.ListChildren()
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		childMsgs, childFlags := child.ReplayMessagesWithFlags()
		childUsage, childCost := child.Usage()
		var link SubagentLink
		agentID := strings.TrimPrefix(child.ID(), meta.ID+subMarker)
		for _, l := range s.Subagents() {
			if l.AgentID == agentID {
				link = l
				break
			}
		}
		if link.AgentID == "" {
			link.AgentID = agentID
		}
		data.Subagents = append(data.Subagents, exportSubagent{
			Link:     link,
			Meta:     child.Meta(),
			Messages: toExportMessages(childMsgs, childFlags),
			Usage:    childUsage,
			CostUSD:  childCost,
		})
	}
	return data, nil
}

func toExportMessages(msgs []provider.Message, flags []bool) []exportMessage {
	out := make([]exportMessage, 0, len(msgs))
	for i, msg := range msgs {
		em := exportMessage{
			Role:       string(msg.Role),
			Content:    messageContent(msg),
			Reasoning:  msg.ReasoningContent,
			ToolCallID: msg.ToolCallID,
		}
		if i < len(flags) {
			em.External = flags[i]
		}
		for _, call := range msg.ToolCalls {
			em.ToolCalls = append(em.ToolCalls, exportToolCall{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
		out = append(out, em)
	}
	return out
}

// messageContent renders text plus any multimodal text parts.
func messageContent(msg provider.Message) string {
	if len(msg.Parts) == 0 {
		return msg.Content
	}
	var parts []string
	for _, part := range msg.Parts {
		switch {
		case part.Text != "":
			parts = append(parts, part.Text)
		case part.ImageURL != nil && part.ImageURL.URL != "":
			parts = append(parts, "[image]("+part.ImageURL.URL+")")
		}
	}
	return strings.Join(parts, "\n\n")
}

const exportCSS = `
:root { color-scheme: light dark; }
* { box-sizing: border-box; }
body { margin: 0; padding: 2rem 1rem; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; line-height: 1.55; background: #0f1117; color: #e6e6e6; }
main { max-width: 860px; margin: 0 auto; }
h1 { font-size: 1.6rem; margin: 0 0 .25rem; }
h2 { font-size: 1.2rem; margin: 2rem 0 .75rem; border-bottom: 1px solid #333; padding-bottom: .3rem; }
h3 { font-size: 1rem; margin: 1.5rem 0 .5rem; color: #9ecbff; }
.meta { display: grid; grid-template-columns: max-content 1fr; gap: .2rem 1rem; margin: .75rem 0 1rem; font-size: .9rem; }
.meta dt { color: #888; }
.meta dd { margin: 0; }
.msg { border: 1px solid #2a2d36; border-radius: 8px; padding: .75rem 1rem; margin: .75rem 0; background: #161923; }
.msg.user { border-left: 3px solid #4c9aff; }
.msg.assistant { border-left: 3px solid #47c78a; }
.msg.tool { border-left: 3px solid #d4a24c; }
.msg.system { border-left: 3px solid #9b7bd4; }
.msg .role { font-size: .75rem; text-transform: uppercase; letter-spacing: .06em; color: #8b93a7; margin-bottom: .35rem; }
.external { display: inline-block; margin-left: .5rem; padding: .05rem .4rem; border-radius: 4px; background: #4a2f2f; color: #ffb4b4; font-size: .7rem; }
pre { background: #0b0d12; border: 1px solid #262a34; border-radius: 6px; padding: .7rem; overflow-x: auto; font-size: .85rem; white-space: pre-wrap; word-break: break-word; }
code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
details { margin: .5rem 0; border: 1px dashed #333846; border-radius: 6px; padding: .4rem .6rem; }
summary { cursor: pointer; color: #9ecbff; font-size: .85rem; }
.toolcall { margin: .5rem 0; }
.toolcall .name { font-weight: 600; }
.subagent { border: 1px solid #2a2d36; border-radius: 8px; padding: .75rem 1rem; margin: 1rem 0; background: #12151d; }
.subagent > details { border: none; padding: 0; margin: 0; }
`

var htmlTemplate = template.Must(template.New("session").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .Name}}{{.Name}}{{else}}{{.Meta.ID}}{{end}}</title>
<style>` + exportCSS + `</style>
</head>
<body>
<main>
<h1>{{if .Name}}{{.Name}}{{else}}{{.Meta.ID}}{{end}}</h1>
<dl class="meta">
<dt>Session</dt><dd><code>{{.Meta.ID}}</code></dd>
{{if .Meta.Model}}<dt>Model</dt><dd><code>{{.Meta.Model}}</code></dd>{{end}}
{{if .Meta.WorkDir}}<dt>Working directory</dt><dd><code>{{.Meta.WorkDir}}</code></dd>{{end}}
{{if .Meta.ParentSessionID}}<dt>Parent session</dt><dd><code>{{.Meta.ParentSessionID}}</code></dd>{{end}}
{{if .Meta.ParentToolCallID}}<dt>Parent tool call</dt><dd><code>{{.Meta.ParentToolCallID}}</code></dd>{{end}}
{{if .Meta.AgentType}}<dt>Agent type</dt><dd><code>{{.Meta.AgentType}}</code></dd>{{end}}
{{if .CreatedAt}}<dt>Created</dt><dd>{{.CreatedAt}}</dd>{{end}}
<dt>Usage</dt><dd>{{.TotalUsage.PromptTokens}} prompt / {{.TotalUsage.CompletionTokens}} completion / {{.TotalUsage.TotalTokens}} total tokens ({{printf "%.6f" .TotalCost}} USD)</dd>
</dl>
{{if .Meta.SystemPrompt}}
<h2>System prompt</h2>
<pre><code>{{.Meta.SystemPrompt}}</code></pre>
{{end}}
<h2>Transcript</h2>
{{range .Messages}}
<article class="msg {{.Role}}">
<div class="role">{{.Role}}{{if .External}}<span class="external">external</span>{{end}}</div>
{{if .Reasoning}}<details><summary>Reasoning</summary><pre><code>{{.Reasoning}}</code></pre></details>{{end}}
{{if .Content}}<div class="content">{{.Content}}</div>{{end}}
{{if .ToolCallID}}<div class="toolcall"><span class="name">Tool result</span> <code>{{.ToolCallID}}</code></div>{{end}}
{{range .ToolCalls}}<div class="toolcall"><span class="name">Tool call: {{.Name}}</span>{{if .ID}} <code>{{.ID}}</code>{{end}}<pre><code>{{.Arguments}}</code></pre></div>{{end}}
</article>
{{end}}
{{if .Subagents}}
<h2>Sub-agents</h2>
{{range .Subagents}}
<section class="subagent">
<details>
<summary>Sub-agent <code>{{.Link.AgentID}}</code>{{if .Link.Description}} — {{.Link.Description}}{{end}}</summary>
<dl class="meta">
{{if .Link.AgentType}}<dt>Type</dt><dd><code>{{.Link.AgentType}}</code></dd>{{end}}
{{if .Link.ParentToolCallID}}<dt>Parent tool call</dt><dd><code>{{.Link.ParentToolCallID}}</code></dd>{{end}}
{{if .Link.Status}}<dt>Status</dt><dd>{{.Link.Status}}</dd>{{end}}
{{if .Link.DurationMs}}<dt>Duration</dt><dd>{{.Link.DurationMs}} ms</dd>{{end}}
<dt>Usage</dt><dd>{{.Usage.TotalTokens}} total tokens ({{printf "%.6f" .CostUSD}} USD)</dd>
{{if .Link.Summary}}<dt>Summary</dt><dd>{{.Link.Summary}}</dd>{{end}}
</dl>
{{if .Meta.SystemPrompt}}<details><summary>System prompt</summary><pre><code>{{.Meta.SystemPrompt}}</code></pre></details>{{end}}
{{range .Messages}}
<article class="msg {{.Role}}">
<div class="role">{{.Role}}{{if .External}}<span class="external">external</span>{{end}}</div>
{{if .Reasoning}}<details><summary>Reasoning</summary><pre><code>{{.Reasoning}}</code></pre></details>{{end}}
{{if .Content}}<div class="content">{{.Content}}</div>{{end}}
{{if .ToolCallID}}<div class="toolcall"><span class="name">Tool result</span> <code>{{.ToolCallID}}</code></div>{{end}}
{{range .ToolCalls}}<div class="toolcall"><span class="name">Tool call: {{.Name}}</span>{{if .ID}} <code>{{.ID}}</code>{{end}}<pre><code>{{.Arguments}}</code></pre></div>{{end}}
</article>
{{end}}
</details>
</section>
{{end}}
{{end}}
</main>
</body>
</html>
`))
