package session

import (
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tern-agent/tern/internal/provider"
)

func approxEqual(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func mustAppendUsageWithDuration(t *testing.T, s *Session, u provider.Usage, cost float64, d time.Duration) {
	t.Helper()
	if err := s.AppendUsageWithDuration(u, cost, "hash", d); err != nil {
		t.Fatalf("AppendUsageWithDuration: %v", err)
	}
}

func TestStatsEmptyStore(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.SessionCount != 0 || stats.SubagentCount != 0 || stats.TurnCount != 0 || stats.MessageCount != 0 {
		t.Fatalf("unexpected counts: %+v", stats)
	}
	if len(stats.Models) != 0 || len(stats.Sessions) != 0 {
		t.Fatalf("expected no rows: %+v", stats)
	}
	if stats.TotalUsage != (provider.Usage{}) || stats.TotalCostUSD != 0 || stats.TotalDuration != 0 {
		t.Fatalf("expected zero totals: %+v", stats)
	}
	if !stats.FirstSessionAt.IsZero() || !stats.LastSessionAt.IsZero() {
		t.Fatalf("expected zero timestamps: %+v", stats)
	}
}

func TestStatsAggregatesSessionsAndChildren(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	base := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)

	older := mustCreate(t, store, SessionMeta{ID: "session-older", Name: "Older", Model: "model-a", WorkDir: "/w", CreatedAt: base})
	mustAppend(t, older, provider.Message{Role: provider.RoleUser, Content: "hello"})
	mustAppend(t, older, provider.Message{Role: provider.RoleAssistant, Content: "hi"})
	mustAppendUsageWithDuration(t, older, provider.Usage{
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
		PromptCacheHitTokens: 60, PromptCacheMissTokens: 40,
	}, 0.5, 10*time.Second)
	// Legacy record without DurationMs inherits the file's explicit duration.
	if err := older.AppendUsage(provider.Usage{
		PromptTokens: 50, CompletionTokens: 10, TotalTokens: 60,
		PromptCacheHitTokens: 20, PromptCacheMissTokens: 30,
	}, 0.25, "legacy"); err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}

	child, err := older.CreateChild("call-1", "agent-1", "explore", "Child", "prompt", "model-b")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	mustAppend(t, child, provider.Message{Role: provider.RoleUser, Content: "child"})
	mustAppendUsageWithDuration(t, child, provider.Usage{
		PromptTokens: 40, CompletionTokens: 20, TotalTokens: 60,
		PromptCacheHitTokens: 10, PromptCacheMissTokens: 30,
	}, 0.2, 5*time.Second)

	// Ensure the newer session really is newer on disk despite coarse clocks.
	time.Sleep(2 * time.Millisecond)
	newer := mustCreate(t, store, SessionMeta{ID: "session-newer", Name: "Newer", Model: "model-a", WorkDir: "/w", CreatedAt: base.Add(time.Hour)})
	mustAppend(t, newer, provider.Message{Role: provider.RoleUser, Content: "second"})
	mustAppendUsageWithDuration(t, newer, provider.Usage{
		PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12,
		PromptCacheHitTokens: 2, PromptCacheMissTokens: 8,
	}, 0.05, 4*time.Second)

	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if stats.SessionCount != 2 || stats.SubagentCount != 1 {
		t.Fatalf("counts = sessions %d subagents %d, want 2/1", stats.SessionCount, stats.SubagentCount)
	}
	if stats.TurnCount != 4 {
		t.Fatalf("TurnCount = %d, want 4", stats.TurnCount)
	}
	if stats.MessageCount != 4 {
		t.Fatalf("MessageCount = %d, want 4", stats.MessageCount)
	}
	wantTotal := provider.Usage{
		PromptTokens: 200, CompletionTokens: 82, TotalTokens: 282,
		PromptCacheHitTokens: 92, PromptCacheMissTokens: 108,
	}
	if stats.TotalUsage != wantTotal {
		t.Fatalf("TotalUsage = %+v, want %+v", stats.TotalUsage, wantTotal)
	}
	approxEqual(t, "TotalCostUSD", stats.TotalCostUSD, 1.0)
	if stats.TotalDuration != 19*time.Second {
		t.Fatalf("TotalDuration = %v, want 19s", stats.TotalDuration)
	}
	approxEqual(t, "CacheHitRate", stats.CacheHitRate, 92.0/200.0)
	approxEqual(t, "AvgTokensPerSec", stats.AvgTokensPerSec, 82.0/19.0)
	if !stats.FirstSessionAt.Equal(base) {
		t.Fatalf("FirstSessionAt = %v, want %v", stats.FirstSessionAt, base)
	}
	if !stats.LastSessionAt.After(base) {
		t.Fatalf("LastSessionAt = %v, want after first session", stats.LastSessionAt)
	}

	if len(stats.Models) != 2 {
		t.Fatalf("Models = %+v, want 2 entries", stats.Models)
	}
	if stats.Models[0].Model != "model-a" || stats.Models[1].Model != "model-b" {
		t.Fatalf("Models order = %q, %q", stats.Models[0].Model, stats.Models[1].Model)
	}
	a := stats.Models[0]
	if a.Sessions != 2 || a.Turns != 3 {
		t.Fatalf("model-a sessions/turns = %d/%d, want 2/3", a.Sessions, a.Turns)
	}
	if a.Usage != (provider.Usage{
		PromptTokens: 160, CompletionTokens: 62, TotalTokens: 222,
		PromptCacheHitTokens: 82, PromptCacheMissTokens: 78,
	}) {
		t.Fatalf("model-a usage = %+v", a.Usage)
	}
	approxEqual(t, "model-a cost", a.CostUSD, 0.8)
	if a.Duration != 14*time.Second {
		t.Fatalf("model-a duration = %v, want 14s", a.Duration)
	}
	approxEqual(t, "model-a tok/s", a.AvgTokensPerSec, 62.0/14.0)
	b := stats.Models[1]
	if b.Sessions != 0 || b.Turns != 1 || b.Duration != 5*time.Second {
		t.Fatalf("model-b = %+v", b)
	}

	if len(stats.Sessions) != 2 {
		t.Fatalf("Sessions = %+v", stats.Sessions)
	}
	if stats.Sessions[0].ID != newer.ID() || stats.Sessions[1].ID != older.ID() {
		t.Fatalf("Sessions order = %q, %q", stats.Sessions[0].ID, stats.Sessions[1].ID)
	}
	row := stats.Sessions[1]
	if row.Name != "Older" || row.Model != "model-a" || row.WorkDir != "/w" {
		t.Fatalf("row meta = %+v", row)
	}
	if row.Turns != 3 || row.Messages != 3 || row.Subagents != 1 {
		t.Fatalf("row counts = %d/%d/%d, want 3/3/1", row.Turns, row.Messages, row.Subagents)
	}
	approxEqual(t, "row cache hit", row.CacheHitRate, 90.0/190.0)
	approxEqual(t, "row cost", row.CostUSD, 0.95)
	if row.Duration != 15*time.Second {
		t.Fatalf("row duration = %v, want 15s", row.Duration)
	}
	approxEqual(t, "row tok/s", row.AvgTokensPerSec, 80.0/15.0)
}

func TestAppendUsageWithDurationPersists(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	sess := mustCreate(t, store, SessionMeta{ID: "dur-session", Model: "model-x"})
	if err := sess.AppendUsageWithDuration(provider.Usage{
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
	}, 0.1, "p", 2500*time.Millisecond); err != nil {
		t.Fatalf("AppendUsageWithDuration: %v", err)
	}
	// A zero duration must not be serialized as a field.
	if err := sess.AppendUsage(provider.Usage{PromptTokens: 1, TotalTokens: 1}, 0, "p"); err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}

	raw, err := os.ReadFile(sess.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	if !strings.Contains(lines[1], `"duration_ms":2500`) {
		t.Fatalf("usage record missing duration_ms: %s", lines[1])
	}
	if strings.Contains(lines[2], "duration_ms") {
		t.Fatalf("zero-duration record should omit duration_ms: %s", lines[2])
	}

	if _, err := store.Open(sess.ID()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.Sessions) != 1 {
		t.Fatalf("Sessions = %+v", stats.Sessions)
	}
	if stats.Sessions[0].Duration != 2500*time.Millisecond {
		t.Fatalf("Duration = %v, want 2.5s", stats.Sessions[0].Duration)
	}
	if stats.Sessions[0].Turns != 2 {
		t.Fatalf("Turns = %d, want 2", stats.Sessions[0].Turns)
	}
}

func TestStatsEstimatesDurationFromTimestamps(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	sess := mustCreate(t, store, SessionMeta{ID: "legacy", Model: "m", CreatedAt: base})
	// Simulate a legacy transcript: monotonic-ish records with no DurationMs,
	// including an idle gap that must be capped at maxGapEstimate.
	timestamps := []time.Time{
		base,
		base.Add(time.Minute),
		base.Add(31 * time.Minute),
	}
	for i, ts := range timestamps {
		usage := provider.Usage{PromptTokens: 1, TotalTokens: 1}
		rec := record{Type: RecordUsage, Timestamp: ts, Usage: &usage}
		if err := appendRecord(sess.Path(), rec); err != nil {
			t.Fatalf("appendRecord %d: %v", i, err)
		}
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	want := time.Minute + maxGapEstimate
	if got := stats.Sessions[0].Duration; got != want {
		t.Fatalf("Duration = %v, want %v", got, want)
	}
}

func TestDeleteRemovesParentAndChildren(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	parent := mustCreate(t, store, SessionMeta{ID: "to-delete", Model: "m"})
	if _, err := parent.CreateChild("call-1", "agent-1", "explore", "one", "", "m"); err != nil {
		t.Fatalf("CreateChild 1: %v", err)
	}
	if _, err := parent.CreateChild("call-2", "agent-2", "plan", "two", "", "m"); err != nil {
		t.Fatalf("CreateChild 2: %v", err)
	}
	other := mustCreate(t, store, SessionMeta{ID: "keep-me", Model: "m"})

	children, err := store.childIDs(parent.ID())
	if err != nil {
		t.Fatalf("childIDs: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("children = %v, want 2", children)
	}

	if err := store.Delete(parent.ID()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, id := range append(children, parent.ID()) {
		if _, err := os.Stat(store.Path(id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists (err %v)", id, err)
		}
	}
	if _, err := os.Stat(store.Path(other.ID())); err != nil {
		t.Fatalf("unrelated session removed: %v", err)
	}
	if err := store.Delete(parent.ID()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("second Delete err = %v, want ErrSessionNotFound", err)
	}
}

func TestDeleteRejectsPathTraversal(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Delete("../escape"); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("Delete traversal err = %v, want ErrInvalidSessionID", err)
	}
	if err := store.Delete("a/b"); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("Delete slash err = %v, want ErrInvalidSessionID", err)
	}
	if err := store.Delete(""); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("Delete empty err = %v, want ErrInvalidSessionID", err)
	}
	if err := store.Delete(".."); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("Delete dotdot err = %v, want ErrInvalidSessionID", err)
	}
}
