package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tern-agent/tern/internal/provider"
)

// maxGapEstimate bounds the active-time contribution of one gap between
// records when a transcript predates DurationMs. Idle time (a session left
// open overnight) must not inflate the reported duration.
const maxGapEstimate = 5 * time.Minute

// ModelStats aggregates usage for one model across all transcripts.
type ModelStats struct {
	Model           string
	Sessions        int
	Turns           int
	Usage           provider.Usage
	CostUSD         float64
	Duration        time.Duration
	AvgTokensPerSec float64
}

// SessionStatRow is the per-session row of the usage/activity report. Usage
// and cost include the session's linked child sub-agent transcripts.
type SessionStatRow struct {
	ID              string
	Name            string
	Model           string
	WorkDir         string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Turns           int
	Messages        int
	Subagents       int
	Usage           provider.Usage
	CacheHitRate    float64
	CostUSD         float64
	Duration        time.Duration
	AvgTokensPerSec float64
}

// AggregateStats is the historical usage/activity rollup for a store.
type AggregateStats struct {
	SessionCount    int
	SubagentCount   int
	TurnCount       int
	MessageCount    int
	TotalUsage      provider.Usage
	CacheHitRate    float64
	TotalCostUSD    float64
	TotalDuration   time.Duration
	AvgTokensPerSec float64
	FirstSessionAt  time.Time
	LastSessionAt   time.Time
	Models          []ModelStats
	Sessions        []SessionStatRow
}

// transcriptStats is one parsed transcript's contribution.
type transcriptStats struct {
	meta      SessionMeta
	hasMeta   bool
	name      string
	usage     provider.Usage
	costUSD   float64
	explicit  time.Duration
	estimated time.Duration
	turns     int
	messages  int
	created   time.Time
	updated   time.Time
}

// Stats scans every parent session in the store and rolls up linked child
// sub-agent transcripts into their parent row.
func (s *Store) Stats() (*AggregateStats, error) {
	out := &AggregateStats{
		Models:   []ModelStats{},
		Sessions: []SessionStatRow{},
	}
	entries, err := os.ReadDir(s.sessionsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("session: read dir: %w", err)
	}

	type modelAgg struct {
		sessions int
		turns    int
		usage    provider.Usage
		cost     float64
		duration time.Duration
	}
	models := map[string]*modelAgg{}
	addModel := func(model string, session bool, turns int, usage provider.Usage, cost float64, duration time.Duration) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		m := models[model]
		if m == nil {
			m = &modelAgg{}
			models[model] = m
		}
		if session {
			m.sessions++
		}
		m.turns += turns
		m.usage = addUsage(m.usage, usage)
		m.cost += cost
		m.duration += duration
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, sessionsExt) || strings.Contains(name, subMarker) {
			continue
		}
		id := strings.TrimSuffix(name, sessionsExt)
		if validateID(id) != nil {
			continue
		}
		parent, err := parseTranscriptStats(s.Path(id))
		if err != nil {
			continue
		}
		if !parent.hasMeta || parent.meta.IsChild() {
			continue
		}

		children, err := s.childIDs(id)
		if err != nil {
			return nil, err
		}
		row := SessionStatRow{
			ID:        parent.meta.ID,
			Name:      parent.displayName(),
			Model:     parent.meta.Model,
			WorkDir:   parent.meta.WorkDir,
			CreatedAt: parent.meta.CreatedAt,
			UpdatedAt: parent.updated,
			Usage:     parent.usage,
			CostUSD:   parent.costUSD,
			Duration:  parent.duration(),
			Turns:     parent.turns,
			Messages:  parent.messages,
			Subagents: len(children),
		}
		addModel(parent.meta.Model, true, parent.turns, parent.usage, parent.costUSD, parent.duration())

		for _, childID := range children {
			child, err := parseTranscriptStats(s.Path(childID))
			if err != nil || !child.hasMeta {
				continue
			}
			row.Usage = addUsage(row.Usage, child.usage)
			row.CostUSD += child.costUSD
			row.Duration += child.duration()
			row.Turns += child.turns
			row.Messages += child.messages
			if child.updated.After(row.UpdatedAt) {
				row.UpdatedAt = child.updated
			}
			if child.created.Before(row.CreatedAt) || row.CreatedAt.IsZero() {
				row.CreatedAt = child.created
			}
			addModel(child.meta.Model, false, child.turns, child.usage, child.costUSD, child.duration())
		}

		row.CacheHitRate = cacheHitRate(row.Usage)
		row.AvgTokensPerSec = tokensPerSecond(row.Usage, row.Duration)
		out.Sessions = append(out.Sessions, row)

		out.SessionCount++
		out.SubagentCount += len(children)
		out.TurnCount += row.Turns
		out.MessageCount += row.Messages
		out.TotalUsage = addUsage(out.TotalUsage, row.Usage)
		out.TotalCostUSD += row.CostUSD
		out.TotalDuration += row.Duration
		if out.FirstSessionAt.IsZero() || row.CreatedAt.Before(out.FirstSessionAt) {
			out.FirstSessionAt = row.CreatedAt
		}
		if row.UpdatedAt.After(out.LastSessionAt) {
			out.LastSessionAt = row.UpdatedAt
		}
	}

	out.CacheHitRate = cacheHitRate(out.TotalUsage)
	out.AvgTokensPerSec = tokensPerSecond(out.TotalUsage, out.TotalDuration)

	out.Models = make([]ModelStats, 0, len(models))
	for name, m := range models {
		out.Models = append(out.Models, ModelStats{
			Model:           name,
			Sessions:        m.sessions,
			Turns:           m.turns,
			Usage:           m.usage,
			CostUSD:         m.cost,
			Duration:        m.duration,
			AvgTokensPerSec: tokensPerSecond(m.usage, m.duration),
		})
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Model < out.Models[j].Model })

	sort.SliceStable(out.Sessions, func(i, j int) bool {
		a, b := out.Sessions[i], out.Sessions[j]
		if a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.ID > b.ID
		}
		return a.UpdatedAt.After(b.UpdatedAt)
	})

	return out, nil
}

func (t transcriptStats) duration() time.Duration {
	if t.explicit > 0 {
		return t.explicit
	}
	return t.estimated
}

func (t transcriptStats) displayName() string {
	if t.name != "" {
		return t.name
	}
	return t.meta.Name
}

// parseTranscriptStats reads one JSONL transcript for the stats rollup. Read
// errors on individual files are reported so callers can skip corrupt files.
func parseTranscriptStats(path string) (transcriptStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return transcriptStats{}, err
	}
	defer f.Close()

	info, statErr := f.Stat()
	t := transcriptStats{}
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<16))
	for {
		var rec record
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return transcriptStats{}, fmt.Errorf("session: decode %s: %w", path, err)
		}
		if rec.Type == RecordMeta && rec.Meta != nil {
			t.meta = *rec.Meta
			t.hasMeta = true
			t.name = rec.Meta.Name
			if !rec.Meta.CreatedAt.IsZero() && (t.created.IsZero() || rec.Meta.CreatedAt.Before(t.created)) {
				t.created = rec.Meta.CreatedAt
			}
		}
		if rec.Type == RecordRename {
			t.name = rec.Rename
		}
		if rec.Type == RecordUsage && rec.Usage != nil {
			t.turns++
			t.usage = addUsage(t.usage, *rec.Usage)
			t.costUSD += rec.CostUSD
			if rec.DurationMs > 0 {
				t.explicit += time.Duration(rec.DurationMs) * time.Millisecond
			}
		}
		if rec.Type == RecordMessage {
			t.messages++
		}
		if !rec.Timestamp.IsZero() {
			if t.created.IsZero() || rec.Timestamp.Before(t.created) {
				t.created = rec.Timestamp
			}
			if rec.Timestamp.After(t.updated) {
				t.updated = rec.Timestamp
			}
		}
	}
	if t.created.IsZero() && t.hasMeta {
		t.created = t.meta.CreatedAt
	}
	if t.updated.IsZero() && statErr == nil {
		t.updated = info.ModTime()
	}
	if t.created.IsZero() {
		t.created = t.updated
	}
	if t.explicit == 0 {
		t.estimated = estimateActiveSpan(path)
	}
	return t, nil
}

// estimateActiveSpan re-reads timestamps and sums capped gaps between
// consecutive records. It is only used when no record carried DurationMs.
func estimateActiveSpan(path string) time.Duration {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	var total time.Duration
	var prev time.Time
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<16))
	for {
		var rec record
		if err := dec.Decode(&rec); err != nil {
			break
		}
		ts := rec.Timestamp
		if ts.IsZero() {
			continue
		}
		if !prev.IsZero() {
			gap := ts.Sub(prev)
			if gap > 0 {
				if gap > maxGapEstimate {
					gap = maxGapEstimate
				}
				total += gap
			}
		}
		prev = ts
	}
	return total
}

// cacheHitRate returns the prompt cache hit ratio in [0,1], preferring the
// explicit hit/miss split and falling back to hit/prompt tokens.
func cacheHitRate(u provider.Usage) float64 {
	if denom := u.PromptCacheHitTokens + u.PromptCacheMissTokens; denom > 0 {
		return float64(u.PromptCacheHitTokens) / float64(denom)
	}
	if u.PromptTokens > 0 {
		return float64(u.PromptCacheHitTokens) / float64(u.PromptTokens)
	}
	return 0
}

// tokensPerSecond is completion throughput over the aggregated active time.
func tokensPerSecond(u provider.Usage, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(u.CompletionTokens) / d.Seconds()
}
