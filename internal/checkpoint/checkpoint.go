// Package checkpoint implements a shadow-copy file snapshot store for Tern.
//
// The store is intentionally independent of git: every turn records the
// pre-edit state of the files it touches (content-addressed blobs plus file
// mode) and, on commit, the post-edit state. The recorded turns can then be
// undone or rewound.
//
// Storage layout for a session:
//
//	<baseDir>/<sessionID>/
//	    blobs/<sha256>     content-addressed file contents
//	    turns.json         committed turn metadata (atomic rewrites)
//
// Only regular files are snapshotted. Paths stored in records are absolute
// and cleaned so that restoring works regardless of the process working
// directory. Turn IDs are expected to increase monotonically; rewind
// operations act on turns in commit order.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	blobsDirName  = "blobs"
	turnsFileName = "turns.json"
	stateVersion  = 1
)

// Sentinel errors returned by the store.
var (
	// ErrNoTurns is returned by Undo when there is no committed turn.
	ErrNoTurns = errors.New("checkpoint: no committed turns")
	// ErrTurnCommitted is returned when a turn is mutated after CommitTurn.
	ErrTurnCommitted = errors.New("checkpoint: turn is already committed")
	// ErrInvalidPath is returned for empty or otherwise unusable paths.
	ErrInvalidPath = errors.New("checkpoint: invalid path")
	// ErrNotRegularFile is returned when a path to snapshot is not a regular file.
	ErrNotRegularFile = errors.New("checkpoint: not a regular file")
	// ErrInvalidSessionID is returned when a session ID would escape baseDir.
	ErrInvalidSessionID = errors.New("checkpoint: invalid session id")
	// ErrDetachedTurn is returned when operating on a turn that has no store.
	ErrDetachedTurn = errors.New("checkpoint: turn is not attached to a store")
)

// FileRecord is the before/after state of a single path in a turn.
type FileRecord struct {
	Path          string      `json:"path"`
	ExistedBefore bool        `json:"existed_before"`
	BeforeHash    string      `json:"before_hash,omitempty"`
	BeforeMode    os.FileMode `json:"before_mode"`
	ExistedAfter  bool        `json:"existed_after"`
	AfterHash     string      `json:"after_hash,omitempty"`
	AfterMode     os.FileMode `json:"after_mode"`
}

// TurnSnapshot is an in-progress or committed checkpoint turn.
//
// Create one with Store.BeginTurn, capture paths with RecordBefore, and finish
// with CommitTurn. A TurnSnapshot must not be reused after it is committed.
type TurnSnapshot struct {
	TurnID    int           `json:"turn_id"`
	Timestamp time.Time     `json:"timestamp"`
	Summary   string        `json:"summary"`
	Records   []*FileRecord `json:"records"`

	store     *Store
	committed bool
}

// TurnInfo is a lightweight view of a committed turn returned by Store.List.
type TurnInfo struct {
	TurnID    int       `json:"turn_id"`
	Timestamp time.Time `json:"timestamp"`
	Summary   string    `json:"summary"`
	FileCount int       `json:"file_count"`
	Paths     []string  `json:"paths"`
}

// RestoreResult reports what Undo and the rewind methods changed on disk.
//
// Restored contains paths whose pre-turn content was written back. Deleted
// contains paths that did not exist before the undone turns and were removed.
// TurnIDs lists the turns that were undone, in the order they were applied.
type RestoreResult struct {
	Restored []string `json:"restored"`
	Deleted  []string `json:"deleted"`
	TurnIDs  []int    `json:"turn_ids"`
}

// Store is a session's checkpoint store. It is safe for concurrent use.
type Store struct {
	mu        sync.Mutex
	dir       string
	sessionID string
	turns     []*TurnSnapshot
}

// stateFile is the on-disk representation of turns.json.
type stateFile struct {
	Version int             `json:"version"`
	Turns   []*TurnSnapshot `json:"turns"`
}

// NewStore opens (or creates) a checkpoint store for sessionID below baseDir.
// Reopening an existing directory reloads all committed turns from disk.
func NewStore(baseDir, sessionID string) (*Store, error) {
	baseDir = strings.TrimSpace(baseDir)
	sessionID = strings.TrimSpace(sessionID)
	if baseDir == "" {
		return nil, fmt.Errorf("%w: empty base directory", ErrInvalidPath)
	}
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}

	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: resolve base directory: %w", err)
	}
	dir := filepath.Join(absBase, sessionID)
	if err := os.MkdirAll(filepath.Join(dir, blobsDirName), 0o700); err != nil {
		return nil, fmt.Errorf("checkpoint: create store directory: %w", err)
	}

	s := &Store{dir: dir, sessionID: sessionID}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func validateSessionID(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("%w: empty id", ErrInvalidSessionID)
	}
	if sessionID == "." || sessionID == ".." || strings.ContainsAny(sessionID, `/\`) ||
		filepath.Base(sessionID) != sessionID {
		return fmt.Errorf("%w: %q", ErrInvalidSessionID, sessionID)
	}
	return nil
}

// Dir returns the absolute session checkpoint directory.
func (s *Store) Dir() string { return s.dir }

// SessionID returns the session identifier this store belongs to.
func (s *Store) SessionID() string { return s.sessionID }

// BeginTurn starts a new turn snapshot. The turn is not recorded until
// CommitTurn succeeds.
func (s *Store) BeginTurn(turnID int, summary string) *TurnSnapshot {
	return &TurnSnapshot{
		TurnID:    turnID,
		Timestamp: time.Now().UTC(),
		Summary:   summary,
		Records:   []*FileRecord{},
		store:     s,
	}
}

// RecordBefore captures the current (pre-edit) state of path into turn.
//
// It is idempotent per path within a turn: repeated calls keep the first
// captured state. A path that does not exist is recorded as ExistedBefore
// false, so undoing the turn deletes it if it was created.
func RecordBefore(turn *TurnSnapshot, path string) error {
	return turn.RecordBefore(path)
}

// RecordBefore is the method form of the package-level RecordBefore.
func (t *TurnSnapshot) RecordBefore(path string) error {
	if t == nil {
		return errors.New("checkpoint: nil turn")
	}
	if t.store == nil {
		return ErrDetachedTurn
	}
	s := t.store

	s.mu.Lock()
	defer s.mu.Unlock()

	if t.committed {
		return ErrTurnCommitted
	}
	abs, err := normalizePath(path)
	if err != nil {
		return err
	}
	for _, r := range t.Records {
		if r.Path == abs {
			// Already captured in this turn; keep the original pre-turn state.
			return nil
		}
	}

	st, err := s.capture(abs)
	if err != nil {
		return err
	}
	rec := &FileRecord{
		Path:          abs,
		ExistedBefore: st.exists,
		BeforeMode:    st.mode,
	}
	if st.exists {
		rec.BeforeHash = st.hash
	}
	t.Records = append(t.Records, rec)
	return nil
}

// CommitTurn captures the post-edit state of every recorded path and persists
// the turn. It may be called exactly once per turn.
func CommitTurn(turn *TurnSnapshot) error {
	if turn == nil {
		return errors.New("checkpoint: nil turn")
	}
	if turn.store == nil {
		return ErrDetachedTurn
	}
	return turn.store.CommitTurn(turn)
}

// CommitTurn is the Store method form of the package-level CommitTurn.
func (s *Store) CommitTurn(t *TurnSnapshot) error {
	if t == nil {
		return errors.New("checkpoint: nil turn")
	}
	if t.store != nil && t.store != s {
		return errors.New("checkpoint: turn belongs to a different store")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if t.committed {
		return ErrTurnCommitted
	}

	for _, r := range t.Records {
		st, err := s.capture(r.Path)
		if err != nil {
			return err
		}
		r.ExistedAfter = st.exists
		if st.exists {
			r.AfterHash = st.hash
			r.AfterMode = st.mode
		} else {
			r.AfterHash = ""
			r.AfterMode = 0
		}
	}

	next := make([]*TurnSnapshot, 0, len(s.turns)+1)
	next = append(next, s.turns...)
	next = append(next, t)
	if err := s.persistTurns(next); err != nil {
		return err
	}

	t.committed = true
	t.store = s
	s.turns = next
	return nil
}

// List returns committed turns in chronological (commit) order.
func (s *Store) List() []TurnInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]TurnInfo, 0, len(s.turns))
	for _, t := range s.turns {
		paths := make([]string, 0, len(t.Records))
		for _, r := range t.Records {
			paths = append(paths, r.Path)
		}
		out = append(out, TurnInfo{
			TurnID:    t.TurnID,
			Timestamp: t.Timestamp,
			Summary:   t.Summary,
			FileCount: len(t.Records),
			Paths:     paths,
		})
	}
	return out
}

// Undo restores all files touched by the latest committed turn to their
// pre-turn state and drops that turn from the history. Newly created files are
// deleted; modified or deleted files are restored with their original
// permissions. Undo returns ErrNoTurns when the history is empty.
func (s *Store) Undo() (*RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.turns) == 0 {
		return nil, ErrNoTurns
	}
	return s.restoreTurnsLocked(s.turns[len(s.turns)-1:])
}

// RewindBefore restores the workspace to the state before turnID, undoing
// turnID and every later turn (in reverse commit order). If no committed turn
// has an ID greater than or equal to turnID, it is a no-op.
func (s *Store) RewindBefore(turnID int) (*RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := len(s.turns)
	for i, t := range s.turns {
		if t.TurnID >= turnID {
			idx = i
			break
		}
	}
	if idx == len(s.turns) {
		return &RestoreResult{Restored: []string{}, Deleted: []string{}, TurnIDs: []int{}}, nil
	}
	return s.restoreTurnsLocked(s.turns[idx:])
}

// RewindAfter restores the workspace to the state immediately after turnID,
// undoing every turn committed later than turnID. It is a no-op when there is
// no later turn.
func (s *Store) RewindAfter(turnID int) (*RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := len(s.turns)
	for i, t := range s.turns {
		if t.TurnID > turnID {
			idx = i
			break
		}
	}
	if idx == len(s.turns) {
		return &RestoreResult{Restored: []string{}, Deleted: []string{}, TurnIDs: []int{}}, nil
	}
	return s.restoreTurnsLocked(s.turns[idx:])
}

// RewindTo is an alias for RewindBefore: it restores the workspace to the
// state before turnID. Use RewindAfter to keep turnID applied.
func (s *Store) RewindTo(turnID int) (*RestoreResult, error) {
	return s.RewindBefore(turnID)
}

// restoreTurnsLocked restores the given turns (and only those) in reverse
// commit order, then removes them from the history. s.mu must be held.
func (s *Store) restoreTurnsLocked(selected []*TurnSnapshot) (*RestoreResult, error) {
	agg := newRestoreAggregate()
	for i := len(selected) - 1; i >= 0; i-- {
		t := selected[i]
		if t == nil {
			continue
		}
		for j := len(t.Records) - 1; j >= 0; j-- {
			r := t.Records[j]
			if r == nil {
				continue
			}
			act, err := s.restoreRecord(r)
			if err != nil {
				return agg.result(undoneTurnIDs(selected)), fmt.Errorf("checkpoint: restore %s: %w", r.Path, err)
			}
			agg.set(r.Path, act)
		}
	}

	remaining := len(s.turns) - len(selected)
	if remaining < 0 {
		remaining = 0
	}
	next := make([]*TurnSnapshot, remaining, len(s.turns))
	copy(next, s.turns[:remaining])
	if err := s.persistTurns(next); err != nil {
		return agg.result(undoneTurnIDs(selected)), err
	}
	s.turns = next
	return agg.result(undoneTurnIDs(selected)), nil
}

func undoneTurnIDs(turns []*TurnSnapshot) []int {
	ids := make([]int, 0, len(turns))
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i] != nil {
			ids = append(ids, turns[i].TurnID)
		}
	}
	return ids
}

type restoreAction int

const (
	restoreNone restoreAction = iota
	restoreWrite
	restoreDelete
)

type restoreAggregate struct {
	order   []string
	actions map[string]restoreAction
}

func newRestoreAggregate() *restoreAggregate {
	return &restoreAggregate{actions: make(map[string]restoreAction)}
}

func (a *restoreAggregate) set(path string, act restoreAction) {
	if act == restoreNone {
		return
	}
	if _, ok := a.actions[path]; !ok {
		a.order = append(a.order, path)
	}
	a.actions[path] = act
}

func (a *restoreAggregate) result(turnIDs []int) *RestoreResult {
	res := &RestoreResult{
		Restored: []string{},
		Deleted:  []string{},
		TurnIDs:  turnIDs,
	}
	for _, p := range a.order {
		switch a.actions[p] {
		case restoreWrite:
			res.Restored = append(res.Restored, p)
		case restoreDelete:
			res.Deleted = append(res.Deleted, p)
		}
	}
	return res
}

// restoreRecord applies a single record's pre-turn state.
func (s *Store) restoreRecord(r *FileRecord) (restoreAction, error) {
	if !r.ExistedBefore {
		if _, err := os.Stat(r.Path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return restoreNone, nil
			}
			return restoreNone, err
		}
		if err := os.Remove(r.Path); err != nil {
			return restoreNone, err
		}
		return restoreDelete, nil
	}

	data, err := s.readBlob(r.BeforeHash)
	if err != nil {
		return restoreNone, err
	}
	if err := writeFileAtomic(r.Path, data, r.BeforeMode.Perm()); err != nil {
		return restoreNone, err
	}
	return restoreWrite, nil
}

type fileState struct {
	exists bool
	hash   string
	mode   os.FileMode
}

// capture reads path and stores its content as a blob.
func (s *Store) capture(path string) (fileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fileState{}, nil
		}
		return fileState{}, fmt.Errorf("checkpoint: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fileState{}, fmt.Errorf("%w: %s", ErrNotRegularFile, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileState{}, fmt.Errorf("checkpoint: read %s: %w", path, err)
	}
	hash, err := s.putBlob(data)
	if err != nil {
		return fileState{}, err
	}
	return fileState{exists: true, hash: hash, mode: info.Mode().Perm()}, nil
}

// putBlob writes data under its SHA-256 digest, deduplicating identical content.
func (s *Store) putBlob(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	path := filepath.Join(s.dir, blobsDirName, hash)
	if _, err := os.Stat(path); err == nil {
		return hash, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("checkpoint: stat blob %s: %w", hash, err)
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return "", fmt.Errorf("checkpoint: write blob %s: %w", hash, err)
	}
	return hash, nil
}

func (s *Store) readBlob(hash string) ([]byte, error) {
	if !validHash(hash) {
		return nil, fmt.Errorf("checkpoint: invalid blob hash %q", hash)
	}
	data, err := os.ReadFile(filepath.Join(s.dir, blobsDirName, hash))
	if err != nil {
		return nil, fmt.Errorf("checkpoint: read blob %s: %w", hash, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != hash {
		return nil, fmt.Errorf("checkpoint: blob %s is corrupt (hash %s)", hash, got)
	}
	return data, nil
}

func validHash(hash string) bool {
	if len(hash) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

func (s *Store) load() error {
	path := filepath.Join(s.dir, turnsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("checkpoint: read %s: %w", path, err)
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("checkpoint: parse %s: %w", path, err)
	}
	if st.Version > stateVersion {
		return fmt.Errorf("checkpoint: unsupported state version %d in %s", st.Version, path)
	}
	for _, t := range st.Turns {
		if t == nil {
			continue
		}
		t.committed = true
		if t.Records == nil {
			t.Records = []*FileRecord{}
		}
		s.turns = append(s.turns, t)
	}
	return nil
}

func (s *Store) persistTurns(turns []*TurnSnapshot) error {
	if turns == nil {
		turns = []*TurnSnapshot{}
	}
	data, err := json.MarshalIndent(stateFile{Version: stateVersion, Turns: turns}, "", "  ")
	if err != nil {
		return fmt.Errorf("checkpoint: encode state: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(filepath.Join(s.dir, turnsFileName), data, 0o600); err != nil {
		return fmt.Errorf("checkpoint: persist state: %w", err)
	}
	return nil
}

func normalizePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalidPath)
	}
	p := filepath.Clean(path)
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", fmt.Errorf("%w: %s: %v", ErrInvalidPath, path, err)
		}
		p = abs
	}
	return p, nil
}

// writeFileAtomic writes data to path via a temporary file in the same
// directory followed by a rename, creating parent directories as needed.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tern-checkpoint-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
