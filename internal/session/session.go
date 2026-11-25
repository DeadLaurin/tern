// Package session implements append-only JSONL session storage for tern.
//
// Every conversation is a JSONL file below <baseDir>/sessions/. The first
// record is always a "meta" header; subsequent records append messages, usage
// accounting, compaction markers, sub-agent links and renames. Nothing is ever
// rewritten in place, so a compaction only hides older messages from the
// active replay while every original record remains readable on disk.
//
// Sub-agent transcripts live next to their parent:
//
//	<sessionID>.jsonl                       parent transcript
//	<sessionID>.sub.<agentID>.jsonl         child transcript
//
// Child headers carry ParentSessionID and ParentToolCallID so the transcript
// graph can be reconstructed from disk alone.
package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tern-agent/tern/internal/provider"
)

const (
	sessionsDirName = "sessions"
	sessionsExt     = ".jsonl"
	subMarker       = ".sub."
)

// Record type discriminators used in the JSONL stream.
const (
	RecordMeta         = "meta"
	RecordMessage      = "message"
	RecordUsage        = "usage"
	RecordCompaction   = "compaction"
	RecordSubagentLink = "subagent_link"
	RecordRename       = "rename"
)

// Sentinel errors returned by the store.
var (
	// ErrSessionNotFound is returned when a transcript does not exist.
	ErrSessionNotFound = errors.New("session: not found")
	// ErrInvalidSessionID is returned for ids that would escape the store or
	// produce ambiguous filenames.
	ErrInvalidSessionID = errors.New("session: invalid session id")
	// ErrInvalidMeta is returned when a header is missing required fields.
	ErrInvalidMeta = errors.New("session: invalid meta")
	// ErrSessionExists is returned when creating a session id that is taken.
	ErrSessionExists = errors.New("session: already exists")
)

// SessionMeta is the session header stored in the first JSONL record.
type SessionMeta struct {
	ID               string    `json:"id"`
	Name             string    `json:"name,omitempty"`
	Model            string    `json:"model,omitempty"`
	WorkDir          string    `json:"work_dir,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	ParentSessionID  string    `json:"parent_session_id,omitempty"`
	ParentToolCallID string    `json:"parent_tool_call_id,omitempty"`
	AgentType        string    `json:"agent_type,omitempty"`
	SystemPrompt     string    `json:"system_prompt,omitempty"`
}

// IsChild reports whether the header describes a sub-agent transcript.
func (m SessionMeta) IsChild() bool { return m.ParentSessionID != "" }

// CompactionEvent records a reversible context compaction.
//
// SummaryMessage replaces the compacted history and KeepFromIndex is the index
// into the active message slice at the time of compaction from which messages
// are retained verbatim. Applying the event to replay yields:
//
//	active = append([]provider.Message{SummaryMessage}, active[KeepFromIndex:]...)
//
// The original message records are never deleted.
type CompactionEvent struct {
	SummaryMessage provider.Message `json:"summary_message"`
	KeepFromIndex  int              `json:"keep_from_index"`
	Reason         string           `json:"reason,omitempty"`
	Timestamp      time.Time        `json:"timestamp"`
}

// SubagentLink links a parent tool call to the child transcript it spawned.
type SubagentLink struct {
	ParentToolCallID string         `json:"parent_tool_call_id"`
	AgentID          string         `json:"agent_id"`
	AgentType        string         `json:"agent_type,omitempty"`
	Description      string         `json:"description,omitempty"`
	Status           string         `json:"status,omitempty"`
	Summary          string         `json:"summary,omitempty"`
	Usage            provider.Usage `json:"usage"`
	CostUSD          float64        `json:"cost_usd,omitempty"`
	DurationMs       int64          `json:"duration_ms,omitempty"`
}

// SessionSummary is the lightweight listing entry returned by Store.List.
type SessionSummary struct {
	ID           string         `json:"id"`
	Name         string         `json:"name,omitempty"`
	Model        string         `json:"model,omitempty"`
	WorkDir      string         `json:"work_dir,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	MessageCount int            `json:"message_count"`
	Usage        provider.Usage `json:"usage"`
	CostUSD      float64        `json:"cost_usd"`
	Preview      string         `json:"preview,omitempty"`
}

// record is the on-disk envelope for every JSONL line.
type record struct {
	Type       string            `json:"type"`
	Timestamp  time.Time         `json:"timestamp,omitempty"`
	Meta       *SessionMeta      `json:"meta,omitempty"`
	Message    *provider.Message `json:"message,omitempty"`
	External   bool              `json:"external,omitempty"`
	Usage      *provider.Usage   `json:"usage,omitempty"`
	CostUSD    float64           `json:"cost_usd,omitempty"`
	PrefixHash string            `json:"prefix_hash,omitempty"`
	DurationMs int64             `json:"duration_ms,omitempty"`
	Compaction *CompactionEvent  `json:"compaction,omitempty"`
	Subagent   *SubagentLink     `json:"subagent_link,omitempty"`
	Rename     string            `json:"rename,omitempty"`
}

// Store owns the sessions directory.
type Store struct {
	baseDir     string
	sessionsDir string
}

// NewStore creates (if needed) and opens the session store below baseDir.
func NewStore(baseDir string) (*Store, error) {
	baseDir = strings.TrimSpace(baseDir)
	if baseDir == "" {
		return nil, fmt.Errorf("session: empty base dir")
	}
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("session: resolve base dir: %w", err)
	}
	dir := filepath.Join(abs, sessionsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create sessions dir: %w", err)
	}
	return &Store{baseDir: abs, sessionsDir: dir}, nil
}

// Dir returns the absolute sessions directory.
func (s *Store) Dir() string { return s.sessionsDir }

// Path returns the transcript path for a session id.
func (s *Store) Path(sessionID string) string {
	return filepath.Join(s.sessionsDir, sessionID+sessionsExt)
}

func validateID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." {
		return fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	if strings.ContainsAny(id, `/\`) || filepath.Base(id) != id {
		return fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	if strings.Contains(id, string(filepath.Separator)) {
		return fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	return nil
}

func generateID(now time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is extraordinary; fall back to nanoseconds.
		return fmt.Sprintf("%s-%08x", now.UTC().Format("20060102T150405"), now.UnixNano()&0xffffffff)
	}
	return fmt.Sprintf("%s-%s", now.UTC().Format("20060102T150405"), hex.EncodeToString(b[:]))
}

// Create writes a new parent or child transcript and returns it open.
func (s *Store) Create(meta SessionMeta) (*Session, error) {
	now := time.Now().UTC()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	if strings.TrimSpace(meta.ID) == "" {
		meta.ID = generateID(now)
	}
	meta.ID = strings.TrimSpace(meta.ID)
	if err := validateID(meta.ID); err != nil {
		return nil, err
	}
	path := s.Path(meta.ID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: %s", ErrSessionExists, meta.ID)
		}
		return nil, fmt.Errorf("session: create %s: %w", path, err)
	}
	rec := record{Type: RecordMeta, Timestamp: meta.CreatedAt, Meta: &meta}
	enc, err := json.Marshal(rec)
	if err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if _, err := f.Write(append(enc, '\n')); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("session: write meta: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, fmt.Errorf("session: sync meta: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return &Session{
		store:   s,
		meta:    meta,
		path:    path,
		name:    meta.Name,
		created: meta.CreatedAt,
		updated: meta.CreatedAt,
	}, nil
}

// Open reads an existing transcript from disk.
func (s *Store) Open(sessionID string) (*Session, error) {
	if err := validateID(sessionID); err != nil {
		return nil, err
	}
	path := s.Path(sessionID)
	parsed, err := parseFile(path)
	if err != nil {
		return nil, err
	}
	if !parsed.hasMeta {
		return nil, fmt.Errorf("%w: %s", ErrInvalidMeta, sessionID)
	}
	sess := &Session{
		store:       s,
		meta:        parsed.meta,
		path:        path,
		name:        parsed.meta.Name,
		created:     parsed.meta.CreatedAt,
		updated:     parsed.updated,
		active:      parsed.active,
		usage:       parsed.usage,
		costUSD:     parsed.costUSD,
		subagents:   parsed.subagents,
		compactions: parsed.compactions,
	}
	if parsed.name != "" {
		sess.name = parsed.name
	}
	return sess, nil
}

// Latest finds the most recently updated parent session for workDir. When no
// session matches workDir it falls back to the newest parent session overall,
// which is what --continue wants.
func (s *Store) Latest(workDir string) (*Session, error) {
	metas, err := s.scanParents()
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, ErrSessionNotFound
	}
	workDir = strings.TrimSpace(workDir)
	if workDir != "" {
		for _, p := range metas {
			if p.meta.WorkDir == workDir {
				return s.Open(p.meta.ID)
			}
		}
	}
	return s.Open(metas[0].meta.ID)
}

// List returns parent sessions newest-first. A non-empty query filters on a
// case-insensitive substring of id, name, work dir and message text.
func (s *Store) List(query string) ([]SessionSummary, error) {
	metas, err := s.scanParents()
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	out := make([]SessionSummary, 0, len(metas))
	for _, p := range metas {
		if query != "" && !p.matches(query) {
			continue
		}
		out = append(out, SessionSummary{
			ID:           p.meta.ID,
			Name:         p.displayName(),
			Model:        p.meta.Model,
			WorkDir:      p.meta.WorkDir,
			CreatedAt:    p.meta.CreatedAt,
			UpdatedAt:    p.updated,
			MessageCount: len(p.active),
			Usage:        p.usage,
			CostUSD:      p.costUSD,
			Preview:      p.preview(),
		})
	}
	return out, nil
}

// Fork copies a transcript (and its child transcripts) to a fresh session id.
func (s *Store) Fork(sessionID string, newName string) (*Session, error) {
	if err := validateID(sessionID); err != nil {
		return nil, err
	}
	src := s.Path(sessionID)
	parsed, err := parseFile(src)
	if err != nil {
		return nil, err
	}
	if !parsed.hasMeta {
		return nil, fmt.Errorf("%w: %s", ErrInvalidMeta, sessionID)
	}

	now := time.Now().UTC()
	newID := generateID(now)
	for {
		if _, err := os.Stat(s.Path(newID)); errors.Is(err, os.ErrNotExist) {
			break
		}
		newID = generateID(time.Now().UTC())
	}

	parentMeta := parsed.meta
	parentMeta.ID = newID
	parentMeta.CreatedAt = now
	parentMeta.ParentSessionID = ""
	parentMeta.ParentToolCallID = ""
	parentMeta.AgentType = ""
	parentMeta.SystemPrompt = ""
	if strings.TrimSpace(newName) != "" {
		parentMeta.Name = newName
	} else if parentMeta.Name != "" {
		parentMeta.Name = parentMeta.Name + " (fork)"
	}

	if err := copyTranscript(src, s.Path(newID), func(m SessionMeta) SessionMeta {
		_ = m
		return parentMeta
	}); err != nil {
		return nil, err
	}
	// A copied rename record would otherwise win over the fork header name.
	if strings.TrimSpace(newName) != "" {
		if err := appendRecord(s.Path(newID), record{Type: RecordRename, Timestamp: now, Rename: newName}); err != nil {
			return nil, err
		}
	}

	// Copy child transcripts, renaming ids so they stay linked to the fork.
	children, err := s.childIDs(sessionID)
	if err != nil {
		return nil, err
	}
	for _, childID := range children {
		agentID := strings.TrimPrefix(childID, sessionID+subMarker)
		newChildID := newID + subMarker + agentID
		srcChild := s.Path(childID)
		childMeta := SessionMeta{}
		if p, perr := parseFile(srcChild); perr == nil && p.hasMeta {
			childMeta = p.meta
		}
		childMeta.ID = newChildID
		childMeta.ParentSessionID = newID
		childMeta.CreatedAt = now
		if err := copyTranscript(srcChild, s.Path(newChildID), func(SessionMeta) SessionMeta {
			return childMeta
		}); err != nil {
			return nil, err
		}
	}

	return s.Open(newID)
}

// Delete removes a parent transcript and every linked child transcript. The
// session id is validated first so a crafted id cannot escape the store.
func (s *Store) Delete(sessionID string) error {
	if err := validateID(sessionID); err != nil {
		return err
	}
	path := s.Path(sessionID)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
		}
		return fmt.Errorf("session: stat %s: %w", path, err)
	}
	children, err := s.childIDs(sessionID)
	if err != nil {
		return err
	}
	for _, childID := range children {
		if err := os.Remove(s.Path(childID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session: delete %s: %w", childID, err)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: delete %s: %w", sessionID, err)
	}
	return nil
}

// Rename appends a rename record to the transcript.
func (s *Store) Rename(sessionID, name string) error {
	sess, err := s.Open(sessionID)
	if err != nil {
		return err
	}
	return sess.Rename(name)
}

// Session is one open transcript. All mutations are safe for concurrent use.
type Session struct {
	mu    sync.Mutex
	store *Store
	meta  SessionMeta
	path  string

	name    string
	created time.Time
	updated time.Time

	active      []provider.Message
	usage       provider.Usage
	costUSD     float64
	subagents   []SubagentLink
	compactions []CompactionEvent
}

// ID returns the session identifier.
func (s *Session) ID() string { return s.meta.ID }

// Meta returns a copy of the session header.
func (s *Session) Meta() SessionMeta { return s.meta }

// Path returns the transcript path.
func (s *Session) Path() string { return s.path }

// Name returns the current display name (including rename records).
func (s *Session) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

// ParentSessionID returns the parent session id for child transcripts.
func (s *Session) ParentSessionID() string { return s.meta.ParentSessionID }

// Rename appends a rename record and updates the in-memory display name.
func (s *Session) Rename(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := record{Type: RecordRename, Timestamp: time.Now().UTC(), Rename: name}
	if err := appendRecord(s.path, rec); err != nil {
		return err
	}
	s.name = name
	s.updated = rec.Timestamp
	return nil
}

// AppendMessage appends a message record.
func (s *Session) AppendMessage(msg provider.Message) error {
	return s.AppendMessageExternal(msg, false)
}

// AppendMessageExternal appends a message and marks it as untrusted external
// output (tool/web/sub-agent results). The flag is kept on disk next to the
// message because provider.Message has no such field.
func (s *Session) AppendMessageExternal(msg provider.Message, external bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	m := msg
	rec := record{Type: RecordMessage, Timestamp: now, Message: &m, External: external}
	if err := appendRecord(s.path, rec); err != nil {
		return err
	}
	s.active = append(s.active, msg)
	s.updated = now
	return nil
}

// AppendUsage records token usage, estimated cost and the request prefix hash.
func (s *Session) AppendUsage(u provider.Usage, costUSD float64, prefixHash string) error {
	return s.AppendUsageWithDuration(u, costUSD, prefixHash, 0)
}

// AppendUsageWithDuration records usage plus the wall-clock time the request
// took. A non-positive duration is omitted so older records stay readable.
func (s *Session) AppendUsageWithDuration(u provider.Usage, costUSD float64, prefixHash string, duration time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	usage := u
	durationMs := int64(0)
	if duration > 0 {
		durationMs = duration.Milliseconds()
		if durationMs == 0 {
			durationMs = 1
		}
	}
	rec := record{Type: RecordUsage, Timestamp: now, Usage: &usage, CostUSD: costUSD, PrefixHash: prefixHash, DurationMs: durationMs}
	if err := appendRecord(s.path, rec); err != nil {
		return err
	}
	s.usage = addUsage(s.usage, u)
	s.costUSD += costUSD
	s.updated = now
	return nil
}

// AppendCompaction records a reversible compaction event and applies it to the
// active in-memory view. Original message records stay on disk.
func (s *Session) AppendCompaction(ev CompactionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	rec := record{Type: RecordCompaction, Timestamp: ev.Timestamp, Compaction: &ev}
	if err := appendRecord(s.path, rec); err != nil {
		return err
	}
	s.active = applyCompaction(s.active, ev)
	s.compactions = append(s.compactions, ev)
	s.updated = ev.Timestamp
	return nil
}

// AppendSubagentLink records or updates the status of a child agent.
func (s *Session) AppendSubagentLink(link SubagentLink) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	l := link
	rec := record{Type: RecordSubagentLink, Timestamp: now, Subagent: &l}
	if err := appendRecord(s.path, rec); err != nil {
		return err
	}
	replaced := false
	for i := range s.subagents {
		if s.subagents[i].AgentID == link.AgentID {
			s.subagents[i] = link
			replaced = true
			break
		}
	}
	if !replaced {
		s.subagents = append(s.subagents, link)
	}
	s.updated = now
	return nil
}

// CreateChild creates a sub-agent transcript linked to parentToolCallID.
func (s *Session) CreateChild(parentToolCallID, agentID, agentType, description, systemPrompt, model string) (*Session, error) {
	agentID = strings.TrimSpace(agentID)
	if err := validateID(agentID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	parentID := s.meta.ID
	workDir := s.meta.WorkDir
	parentModel := s.meta.Model
	s.mu.Unlock()

	if strings.TrimSpace(model) == "" {
		model = parentModel
	}
	childID := parentID + subMarker + agentID
	meta := SessionMeta{
		ID:               childID,
		Name:             description,
		Model:            model,
		WorkDir:          workDir,
		CreatedAt:        time.Now().UTC(),
		ParentSessionID:  parentID,
		ParentToolCallID: parentToolCallID,
		AgentType:        agentType,
		SystemPrompt:     systemPrompt,
	}
	child, err := s.store.Create(meta)
	if err != nil {
		return nil, err
	}
	link := SubagentLink{
		ParentToolCallID: parentToolCallID,
		AgentID:          agentID,
		AgentType:        agentType,
		Description:      description,
		Status:           "running",
	}
	if err := s.AppendSubagentLink(link); err != nil {
		return nil, err
	}
	return child, nil
}

// ListChildren opens every child transcript of this session.
func (s *Session) ListChildren() ([]*Session, error) {
	s.mu.Lock()
	parentID := s.meta.ID
	store := s.store
	s.mu.Unlock()

	ids, err := store.childIDs(parentID)
	if err != nil {
		return nil, err
	}
	children := make([]*Session, 0, len(ids))
	for _, id := range ids {
		child, err := store.Open(id)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, nil
}

// ReplayMessages rebuilds the active message slice from the JSONL file on
// disk, applying compaction records while leaving originals untouched.
func (s *Session) ReplayMessages() []provider.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	parsed, err := parseFile(s.path)
	if err != nil {
		// Fall back to the in-memory view; the file was readable at open time.
		out := make([]provider.Message, len(s.active))
		copy(out, s.active)
		return out
	}
	out := make([]provider.Message, len(parsed.active))
	copy(out, parsed.active)
	return out
}

// ReplayMessagesWithFlags replays messages and their external-output flags.
func (s *Session) ReplayMessagesWithFlags() ([]provider.Message, []bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parsed, err := parseFile(s.path)
	if err != nil {
		msgs := make([]provider.Message, len(s.active))
		copy(msgs, s.active)
		return msgs, make([]bool, len(s.active))
	}
	msgs := make([]provider.Message, len(parsed.active))
	copy(msgs, parsed.active)
	flags := make([]bool, len(parsed.activeFlags))
	copy(flags, parsed.activeFlags)
	return msgs, flags
}

// TotalUsage rolls up this session's usage/cost plus every descendant child.
func (s *Session) TotalUsage() (provider.Usage, float64) {
	s.mu.Lock()
	u := s.usage
	cost := s.costUSD
	s.mu.Unlock()

	children, err := s.ListChildren()
	if err != nil {
		return u, cost
	}
	for _, child := range children {
		cu, cc := child.TotalUsage()
		u = addUsage(u, cu)
		cost += cc
	}
	return u, cost
}

// Usage returns this session's own usage and cost (excluding children).
func (s *Session) Usage() (provider.Usage, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage, s.costUSD
}

// Subagents returns a copy of the recorded sub-agent links.
func (s *Session) Subagents() []SubagentLink {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SubagentLink, len(s.subagents))
	copy(out, s.subagents)
	return out
}

// Compactions returns a copy of the applied compaction events.
func (s *Session) Compactions() []CompactionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CompactionEvent, len(s.compactions))
	copy(out, s.compactions)
	return out
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

type parsedFile struct {
	hasMeta     bool
	meta        SessionMeta
	active      []provider.Message
	activeFlags []bool
	allMessages []provider.Message
	allFlags    []bool
	usage       provider.Usage
	costUSD     float64
	subagents   []SubagentLink
	compactions []CompactionEvent
	name        string
	updated     time.Time
}

func parseFile(path string) (*parsedFile, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, filepath.Base(path))
		}
		return nil, fmt.Errorf("session: open %s: %w", path, err)
	}
	defer f.Close()

	info, statErr := f.Stat()
	p := &parsedFile{}
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<16))
	for {
		var rec record
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("session: decode %s: %w", path, err)
		}
		switch rec.Type {
		case RecordMeta:
			if rec.Meta != nil {
				p.meta = *rec.Meta
				p.hasMeta = true
				p.name = rec.Meta.Name
				if rec.Meta.CreatedAt.After(p.updated) {
					p.updated = rec.Meta.CreatedAt
				}
			}
		case RecordMessage:
			if rec.Message != nil {
				msg := *rec.Message
				p.allMessages = append(p.allMessages, msg)
				p.allFlags = append(p.allFlags, rec.External)
				p.active = append(p.active, msg)
				p.activeFlags = append(p.activeFlags, rec.External)
			}
		case RecordUsage:
			if rec.Usage != nil {
				p.usage = addUsage(p.usage, *rec.Usage)
				p.costUSD += rec.CostUSD
			}
		case RecordCompaction:
			if rec.Compaction != nil {
				ev := *rec.Compaction
				p.compactions = append(p.compactions, ev)
				p.active = applyCompaction(p.active, ev)
				p.activeFlags = applyCompactionFlags(p.activeFlags, ev)
			}
		case RecordSubagentLink:
			if rec.Subagent != nil {
				link := *rec.Subagent
				replaced := false
				for i := range p.subagents {
					if p.subagents[i].AgentID == link.AgentID {
						p.subagents[i] = link
						replaced = true
						break
					}
				}
				if !replaced {
					p.subagents = append(p.subagents, link)
				}
			}
		case RecordRename:
			p.name = rec.Rename
		}
		if rec.Timestamp.After(p.updated) {
			p.updated = rec.Timestamp
		}
	}
	if p.updated.IsZero() && statErr == nil {
		p.updated = info.ModTime()
	}
	return p, nil
}

// applyCompaction implements the reversible active-view transform.
func applyCompaction(active []provider.Message, ev CompactionEvent) []provider.Message {
	keep := ev.KeepFromIndex
	if keep < 0 {
		keep = 0
	}
	if keep > len(active) {
		keep = len(active)
	}
	out := make([]provider.Message, 0, 1+len(active)-keep)
	out = append(out, ev.SummaryMessage)
	out = append(out, active[keep:]...)
	return out
}

// applyCompactionFlags mirrors applyCompaction for external-output flags.
// Compaction only ever keeps a suffix of the active slice, so the retained
// flags are derived from the previous active flag slice.
func applyCompactionFlags(activeFlags []bool, ev CompactionEvent) []bool {
	keep := ev.KeepFromIndex
	if keep < 0 {
		keep = 0
	}
	if keep > len(activeFlags) {
		keep = len(activeFlags)
	}
	out := make([]bool, 0, 1+len(activeFlags)-keep)
	out = append(out, false) // summary is generated, not external
	out = append(out, activeFlags[keep:]...)
	return out
}

func addUsage(a, b provider.Usage) provider.Usage {
	return provider.Usage{
		PromptTokens:          a.PromptTokens + b.PromptTokens,
		CompletionTokens:      a.CompletionTokens + b.CompletionTokens,
		TotalTokens:           a.TotalTokens + b.TotalTokens,
		PromptCacheHitTokens:  a.PromptCacheHitTokens + b.PromptCacheHitTokens,
		PromptCacheMissTokens: a.PromptCacheMissTokens + b.PromptCacheMissTokens,
		ReasoningTokens:       a.ReasoningTokens + b.ReasoningTokens,
	}
}

// ---------------------------------------------------------------------------
// Disk helpers
// ---------------------------------------------------------------------------

// appendRecord appends one JSON line and fsyncs it before returning.
func appendRecord(path string, rec record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("session: marshal record: %w", err)
	}
	b = append(b, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("session: open append %s: %w", path, err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("session: append %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("session: sync %s: %w", path, err)
	}
	return f.Close()
}

type scannedSession struct {
	meta    SessionMeta
	updated time.Time
	active  []provider.Message
	usage   provider.Usage
	costUSD float64
	name    string
}

func (s scannedSession) displayName() string {
	if s.name != "" {
		return s.name
	}
	return s.meta.Name
}

func (s scannedSession) preview() string {
	for i := len(s.active) - 1; i >= 0; i-- {
		msg := s.active[i]
		text := msg.Content
		if text == "" {
			for _, part := range msg.Parts {
				if part.Text != "" {
					text += part.Text
				}
			}
		}
		text = strings.TrimSpace(text)
		if text != "" {
			text = strings.Join(strings.Fields(text), " ")
			if len(text) > 160 {
				text = text[:160] + "…"
			}
			return text
		}
	}
	return ""
}

func (s scannedSession) matches(query string) bool {
	if strings.Contains(strings.ToLower(s.meta.ID), query) ||
		strings.Contains(strings.ToLower(s.displayName()), query) ||
		strings.Contains(strings.ToLower(s.meta.WorkDir), query) {
		return true
	}
	for _, msg := range s.active {
		if strings.Contains(strings.ToLower(messageText(msg)), query) {
			return true
		}
	}
	return false
}

func messageText(msg provider.Message) string {
	var b strings.Builder
	b.WriteString(msg.Content)
	b.WriteString(msg.ReasoningContent)
	for _, part := range msg.Parts {
		if part.Text != "" {
			b.WriteString(part.Text)
		}
		if part.ImageURL != nil {
			b.WriteString(part.ImageURL.URL)
		}
	}
	for _, call := range msg.ToolCalls {
		b.WriteString(call.Function.Name)
		b.WriteString(call.Function.Arguments)
	}
	return b.String()
}

// scanParents reads all parent transcripts, newest-first.
func (s *Store) scanParents() ([]scannedSession, error) {
	entries, err := os.ReadDir(s.sessionsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: read dir: %w", err)
	}
	var out []scannedSession
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, sessionsExt) || strings.Contains(name, subMarker) {
			continue
		}
		id := strings.TrimSuffix(name, sessionsExt)
		if err := validateID(id); err != nil {
			continue
		}
		parsed, err := parseFile(s.Path(id))
		if err != nil || !parsed.hasMeta || parsed.meta.IsChild() {
			continue
		}
		out = append(out, scannedSession{
			meta:    parsed.meta,
			updated: parsed.updated,
			active:  parsed.active,
			usage:   parsed.usage,
			costUSD: parsed.costUSD,
			name:    parsed.name,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].updated.Equal(out[j].updated) {
			return out[i].meta.ID > out[j].meta.ID
		}
		return out[i].updated.After(out[j].updated)
	})
	return out, nil
}

// childIDs returns the child session ids belonging to parentID.
func (s *Store) childIDs(parentID string) ([]string, error) {
	entries, err := os.ReadDir(s.sessionsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: read dir: %w", err)
	}
	prefix := parentID + subMarker
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, sessionsExt) {
			continue
		}
		id := strings.TrimSuffix(name, sessionsExt)
		if strings.Contains(id, string(filepath.Separator)) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// copyTranscript copies src to dst, replacing the meta record using transform.
// Every non-meta record is preserved byte-for-byte in meaning (re-encoded).
func copyTranscript(src, dst string, transform func(SessionMeta) SessionMeta) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("session: open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("session: create %s: %w", dst, err)
	}

	w := bufio.NewWriter(out)
	dec := json.NewDecoder(bufio.NewReaderSize(in, 1<<16))
	wroteMeta := false
	for {
		var rec record
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("session: decode %s: %w", src, err)
		}
		if rec.Type == RecordMeta && rec.Meta != nil {
			meta := transform(*rec.Meta)
			rec.Meta = &meta
			rec.Timestamp = meta.CreatedAt
			wroteMeta = true
		}
		line, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	if !wroteMeta {
		return fmt.Errorf("%w: %s", ErrInvalidMeta, src)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}
