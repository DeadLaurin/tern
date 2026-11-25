package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tern-agent/tern/internal/provider"
)

// canonicalBytes serializes a request with the provider's stable canonical
// encoder so byte-identical prefix behavior can be asserted.
func canonicalBytes(t *testing.T, model string, msgs []provider.Message, tools []provider.ToolDef) []byte {
	t.Helper()
	b, err := provider.MarshalCanonicalRequest(provider.ChatRequest{
		Model:    model,
		Messages: msgs,
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("MarshalCanonicalRequest: %v", err)
	}
	return b
}

func mustCreate(t *testing.T, store *Store, meta SessionMeta) *Session {
	t.Helper()
	s, err := store.Create(meta)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

func mustAppend(t *testing.T, s *Session, msg provider.Message) {
	t.Helper()
	if err := s.AppendMessage(msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
}

func testTools() []provider.ToolDef {
	return []provider.ToolDef{
		{
			Name:        "read_file",
			Description: "Read a file",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		},
		{
			Name:        "bash",
			Description: "Run a command",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}}}`),
		},
	}
}

func seedConversation(t *testing.T, s *Session) {
	t.Helper()
	mustAppend(t, s, provider.Message{
		Role:    provider.RoleSystem,
		Content: "You are tern, a careful coding agent.",
	})
	mustAppend(t, s, provider.Message{
		Role:    provider.RoleUser,
		Content: "Please inspect the repository.",
		Parts: []provider.ContentPart{
			{Type: "text", Text: "Please inspect the repository."},
			{Type: "image_url", ImageURL: &provider.ImageURL{URL: "https://example.com/a.png", Detail: "low"}},
		},
	})
	mustAppend(t, s, provider.Message{
		Role:             provider.RoleAssistant,
		Content:          "",
		ReasoningContent: "I should list the files first.",
		ToolCalls: []provider.ToolCall{{
			ID:   "call_1",
			Type: "function",
			Function: provider.ToolCallFunction{
				Name:      "bash",
				Arguments: `{"cmd":"ls -la"}`,
			},
		}},
	})
	if err := s.AppendMessageExternal(provider.Message{
		Role:       provider.RoleTool,
		Content:    "<script>alert('x')</script> total 0",
		ToolCallID: "call_1",
	}, true); err != nil {
		t.Fatalf("AppendMessageExternal: %v", err)
	}
	mustAppend(t, s, provider.Message{
		Role:             provider.RoleAssistant,
		Content:          "The repository is empty.",
		ReasoningContent: "Nothing to report beyond that.",
	})
}

func TestCreateAppendReopenByteIdentical(t *testing.T) {
	base := t.TempDir()
	store, err := NewStore(base)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s := mustCreate(t, store, SessionMeta{
		Name:    "parent",
		Model:   "deepseek-v4-pro",
		WorkDir: "/repo/a",
	})
	seedConversation(t, s)
	if err := s.AppendUsage(provider.Usage{
		PromptTokens:          100,
		CompletionTokens:      25,
		TotalTokens:           125,
		PromptCacheHitTokens:  80,
		PromptCacheMissTokens: 20,
		ReasoningTokens:       10,
	}, 0.0123, "abc123"); err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}

	before := canonicalBytes(t, "deepseek-v4-pro", s.ReplayMessages(), testTools())

	reopened, err := store.Open(s.ID())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	after := canonicalBytes(t, "deepseek-v4-pro", reopened.ReplayMessages(), testTools())
	if !bytes.Equal(before, after) {
		t.Fatalf("canonical request changed after resume\nbefore: %s\nafter:  %s", before, after)
	}

	usage, cost := reopened.Usage()
	if usage.TotalTokens != 125 || usage.PromptTokens != 100 || usage.CompletionTokens != 25 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.PromptCacheHitTokens != 80 || usage.PromptCacheMissTokens != 20 || usage.ReasoningTokens != 10 {
		t.Fatalf("cache/reasoning usage = %+v", usage)
	}
	if cost != 0.0123 {
		t.Fatalf("cost = %v", cost)
	}

	msgs, flags := reopened.ReplayMessagesWithFlags()
	if len(flags) != len(msgs) {
		t.Fatalf("flags/messages mismatch: %d vs %d", len(flags), len(msgs))
	}
	if !flags[3] {
		t.Fatalf("external flag not preserved on tool message: %v", flags)
	}
	if flags[0] || flags[1] || flags[2] || flags[4] {
		t.Fatalf("unexpected external flags: %v", flags)
	}
}

func TestChildTranscriptByteIdenticalAndUsageRollup(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	parent := mustCreate(t, store, SessionMeta{
		Name:    "parent",
		Model:   "deepseek-v4-pro",
		WorkDir: "/repo/a",
	})
	if err := parent.AppendUsage(provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, 0.01, "p"); err != nil {
		t.Fatalf("parent usage: %v", err)
	}

	child, err := parent.CreateChild("call_1", "agent-1", "explore", "Inspect files", "You are a scout.", "deepseek-flash")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	if child.ParentSessionID() != parent.ID() {
		t.Fatalf("child parent = %q, want %q", child.ParentSessionID(), parent.ID())
	}
	mustAppend(t, child, provider.Message{Role: provider.RoleSystem, Content: "You are a scout."})
	mustAppend(t, child, provider.Message{Role: provider.RoleUser, Content: "Find the entrypoint."})
	mustAppend(t, child, provider.Message{
		Role:             provider.RoleAssistant,
		Content:          "It is main.go.",
		ReasoningContent: "Search for func main.",
	})
	if err := child.AppendUsage(provider.Usage{PromptTokens: 20, CompletionTokens: 7, TotalTokens: 27}, 0.02, "c"); err != nil {
		t.Fatalf("child usage: %v", err)
	}

	before := canonicalBytes(t, "deepseek-flash", child.ReplayMessages(), testTools())

	reopenedParent, err := store.Open(parent.ID())
	if err != nil {
		t.Fatalf("Open parent: %v", err)
	}
	children, err := reopenedParent.ListChildren()
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("children = %d, want 1", len(children))
	}
	if children[0].ID() != parent.ID()+".sub.agent-1" {
		t.Fatalf("child id = %q", children[0].ID())
	}
	after := canonicalBytes(t, "deepseek-flash", children[0].ReplayMessages(), testTools())
	if !bytes.Equal(before, after) {
		t.Fatalf("child canonical request changed after resume\nbefore: %s\nafter:  %s", before, after)
	}
	if !strings.HasSuffix(children[0].Path(), ".sub.agent-1.jsonl") {
		t.Fatalf("child path = %q", children[0].Path())
	}

	links := reopenedParent.Subagents()
	if len(links) != 1 || links[0].ParentToolCallID != "call_1" || links[0].Status != "running" {
		t.Fatalf("subagent links = %+v", links)
	}

	total, cost := reopenedParent.TotalUsage()
	if total.TotalTokens != 42 {
		t.Fatalf("rolled up total tokens = %d, want 42", total.TotalTokens)
	}
	if cost < 0.029999 || cost > 0.030001 {
		t.Fatalf("rolled up cost = %v, want 0.03", cost)
	}
}

func TestCompactionReversibleAndByteIdentical(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s := mustCreate(t, store, SessionMeta{Name: "compact", Model: "deepseek-v4-pro", WorkDir: "/repo"})
	for i := 0; i < 6; i++ {
		role := provider.RoleUser
		if i%2 == 1 {
			role = provider.RoleAssistant
		}
		mustAppend(t, s, provider.Message{Role: role, Content: "message-" + string(rune('0'+i))})
	}

	rawBefore, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	rawLinesBefore := strings.Count(string(rawBefore), "\n")

	summary := provider.Message{Role: provider.RoleAssistant, Content: "Summary of the first four messages."}
	if err := s.AppendCompaction(CompactionEvent{
		SummaryMessage: summary,
		KeepFromIndex:  4,
		Reason:         "context budget",
	}); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}

	active := s.ReplayMessages()
	if len(active) != 3 {
		t.Fatalf("active messages = %d, want 3", len(active))
	}
	if active[0].Content != summary.Content {
		t.Fatalf("summary = %q", active[0].Content)
	}
	if active[1].Content != "message-4" || active[2].Content != "message-5" {
		t.Fatalf("kept messages = %q, %q", active[1].Content, active[2].Content)
	}

	before := canonicalBytes(t, "deepseek-v4-pro", active, testTools())
	reopened, err := store.Open(s.ID())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	after := canonicalBytes(t, "deepseek-v4-pro", reopened.ReplayMessages(), testTools())
	if !bytes.Equal(before, after) {
		t.Fatalf("compacted canonical request changed after resume\nbefore: %s\nafter:  %s", before, after)
	}

	// Compaction is reversible: originals are still on disk.
	rawAfter, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("read raw after: %v", err)
	}
	if got := strings.Count(string(rawAfter), "\n"); got <= rawLinesBefore {
		t.Fatalf("raw lines did not grow: before=%d after=%d", rawLinesBefore, got)
	}
	for i := 0; i < 6; i++ {
		want := `"content":"message-` + string(rune('0'+i)) + `"`
		if !strings.Contains(string(rawAfter), want) {
			t.Fatalf("original record %q missing from disk after compaction", want)
		}
	}
	if !strings.Contains(string(rawAfter), `"type":"compaction"`) {
		t.Fatal("compaction record missing on disk")
	}
	if comps := reopened.Compactions(); len(comps) != 1 || comps[0].KeepFromIndex != 4 {
		t.Fatalf("compactions = %+v", comps)
	}

	// A second compaction composes on the active view.
	if err := reopened.AppendCompaction(CompactionEvent{
		SummaryMessage: provider.Message{Role: provider.RoleAssistant, Content: "Second summary."},
		KeepFromIndex:  2,
		Reason:         "still too long",
	}); err != nil {
		t.Fatalf("second compaction: %v", err)
	}
	active = reopened.ReplayMessages()
	if len(active) != 2 || active[0].Content != "Second summary." || active[1].Content != "message-5" {
		t.Fatalf("second compaction active = %+v", active)
	}
	reopened2, err := store.Open(s.ID())
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	before = canonicalBytes(t, "deepseek-v4-pro", active, testTools())
	after = canonicalBytes(t, "deepseek-v4-pro", reopened2.ReplayMessages(), testTools())
	if !bytes.Equal(before, after) {
		t.Fatalf("second compaction not reproducible\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestLatestListRenameFork(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	older := mustCreate(t, store, SessionMeta{Name: "older", Model: "m", WorkDir: "/repo/a"})
	mustAppend(t, older, provider.Message{Role: provider.RoleUser, Content: "alpha needle"})
	time.Sleep(5 * time.Millisecond)

	newer := mustCreate(t, store, SessionMeta{Name: "newer", Model: "m", WorkDir: "/repo/b"})
	mustAppend(t, newer, provider.Message{Role: provider.RoleUser, Content: "beta haystack"})
	time.Sleep(5 * time.Millisecond)

	// Touch the older session so it becomes the most recently updated.
	mustAppend(t, older, provider.Message{Role: provider.RoleAssistant, Content: "alpha reply"})

	latest, err := store.Latest("/repo/a")
	if err != nil {
		t.Fatalf("Latest workDir: %v", err)
	}
	if latest.ID() != older.ID() {
		t.Fatalf("Latest(/repo/a) = %q, want %q", latest.ID(), older.ID())
	}
	latest, err = store.Latest("/does/not/exist")
	if err != nil {
		t.Fatalf("Latest fallback: %v", err)
	}
	if latest.ID() != older.ID() {
		t.Fatalf("Latest fallback = %q, want newest overall %q", latest.ID(), older.ID())
	}
	latest, err = store.Latest("")
	if err != nil || latest.ID() != older.ID() {
		t.Fatalf("Latest(\"\") = %v, %v", latest, err)
	}

	all, err := store.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 || all[0].ID != older.ID() || all[1].ID != newer.ID() {
		t.Fatalf("List order = %+v", all)
	}
	if all[0].Preview != "alpha reply" {
		t.Fatalf("preview = %q", all[0].Preview)
	}

	cases := []struct {
		query string
		want  string
	}{
		{"needle", older.ID()},
		{"haystack", newer.ID()},
		{"/repo/b", newer.ID()},
		{"OLDER", older.ID()},
		{"does-not-exist", ""},
	}
	for _, tc := range cases {
		got, err := store.List(tc.query)
		if err != nil {
			t.Fatalf("List(%q): %v", tc.query, err)
		}
		if tc.want == "" {
			if len(got) != 0 {
				t.Fatalf("List(%q) = %d results, want 0", tc.query, len(got))
			}
			continue
		}
		if len(got) != 1 || got[0].ID != tc.want {
			t.Fatalf("List(%q) = %+v, want %q", tc.query, got, tc.want)
		}
	}

	if err := store.Rename(older.ID(), "renamed session"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	reopened, err := store.Open(older.ID())
	if err != nil {
		t.Fatalf("Open renamed: %v", err)
	}
	if reopened.Name() != "renamed session" {
		t.Fatalf("Name = %q", reopened.Name())
	}
	renamedList, err := store.List("renamed")
	if err != nil || len(renamedList) != 1 || renamedList[0].Name != "renamed session" {
		t.Fatalf("List(renamed) = %+v, %v", renamedList, err)
	}

	// Fork copies the parent transcript and child transcripts.
	child, err := reopened.CreateChild("call_fork", "agent-fork", "explore", "Fork child", "sys", "m2")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	mustAppend(t, child, provider.Message{Role: provider.RoleUser, Content: "child-only content"})

	forked, err := store.Fork(older.ID(), "forked session")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if forked.ID() == older.ID() {
		t.Fatal("fork reused the source id")
	}
	if forked.Name() != "forked session" {
		t.Fatalf("fork name = %q", forked.Name())
	}
	if forked.Meta().ParentSessionID != "" {
		t.Fatalf("fork parent session = %q", forked.Meta().ParentSessionID)
	}
	before := canonicalBytes(t, "m", reopened.ReplayMessages(), testTools())
	after := canonicalBytes(t, "m", forked.ReplayMessages(), testTools())
	if !bytes.Equal(before, after) {
		t.Fatalf("forked messages differ\nbefore: %s\nafter:  %s", before, after)
	}

	forkChildren, err := forked.ListChildren()
	if err != nil {
		t.Fatalf("fork ListChildren: %v", err)
	}
	if len(forkChildren) != 1 {
		t.Fatalf("fork children = %d, want 1", len(forkChildren))
	}
	if forkChildren[0].ID() != forked.ID()+".sub.agent-fork" {
		t.Fatalf("fork child id = %q", forkChildren[0].ID())
	}
	if forkChildren[0].ParentSessionID() != forked.ID() {
		t.Fatalf("fork child parent = %q", forkChildren[0].ParentSessionID())
	}
	childBefore := canonicalBytes(t, "m2", child.ReplayMessages(), testTools())
	childAfter := canonicalBytes(t, "m2", forkChildren[0].ReplayMessages(), testTools())
	if !bytes.Equal(childBefore, childAfter) {
		t.Fatalf("forked child transcript differs\nbefore: %s\nafter:  %s", childBefore, childAfter)
	}

	if _, err := store.Latest("/repo/a"); err != nil {
		t.Fatalf("Latest after fork: %v", err)
	}
}

func TestLatestNoSessions(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.Latest("/x"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Latest error = %v, want ErrSessionNotFound", err)
	}
	if got, err := store.List(""); err != nil || len(got) != 0 {
		t.Fatalf("List = %v, %v", got, err)
	}
}

func TestExportMarkdownAndHTML(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s := mustCreate(t, store, SessionMeta{Name: "export me", Model: "deepseek-v4-pro", WorkDir: "/repo"})
	seedConversation(t, s)
	if err := s.AppendUsage(provider.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}, 0.001, "hash"); err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}
	child, err := s.CreateChild("call_1", "agent-export", "explore", "Child description", "Child system prompt", "deepseek-flash")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	mustAppend(t, child, provider.Message{Role: provider.RoleUser, Content: "child transcript body"})
	if err := child.AppendUsage(provider.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3}, 0.0005, "childhash"); err != nil {
		t.Fatalf("child usage: %v", err)
	}

	var md bytes.Buffer
	if err := s.ExportMarkdown(&md); err != nil {
		t.Fatalf("ExportMarkdown: %v", err)
	}
	mdStr := md.String()
	for _, want := range []string{
		"# export me",
		"I should list the files first.",
		"bash",
		"ls -la",
		"Sub-agents",
		"agent-export",
		"child transcript body",
		"<details>",
		"call_1",
	} {
		if !strings.Contains(mdStr, want) {
			t.Fatalf("markdown missing %q\n%s", want, mdStr)
		}
	}

	var htmlOut bytes.Buffer
	if err := s.ExportHTML(&htmlOut); err != nil {
		t.Fatalf("ExportHTML: %v", err)
	}
	htmlStr := htmlOut.String()
	for _, want := range []string{
		"<!DOCTYPE html>",
		"<details>",
		"export me",
		"I should list the files first.",
		"agent-export",
		"child transcript body",
		"<style>",
	} {
		if !strings.Contains(htmlStr, want) {
			t.Fatalf("html missing %q\n%s", want, htmlStr)
		}
	}
	if strings.Contains(htmlStr, "<script>alert") {
		t.Fatal("html contains unescaped script tag")
	}
	if !strings.Contains(htmlStr, "&lt;script&gt;") {
		t.Fatalf("html did not escape tool output\n%s", htmlStr)
	}
}

func TestInvalidIDsAndDuplicateCreate(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.Create(SessionMeta{ID: "../escape"}); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("Create escape error = %v", err)
	}
	meta := SessionMeta{ID: "fixed-id", Name: "n"}
	if _, err := store.Create(meta); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Create(meta); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("duplicate Create error = %v", err)
	}
	if _, err := store.Open("missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Open missing error = %v", err)
	}
}

func TestConcurrentAppends(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s := mustCreate(t, store, SessionMeta{Name: "concurrent", Model: "m"})

	const n = 20
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			done <- s.AppendMessage(provider.Message{Role: provider.RoleUser, Content: "concurrent"})
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}
	msgs := s.ReplayMessages()
	if len(msgs) != n {
		t.Fatalf("replayed %d messages, want %d", len(msgs), n)
	}
}
