package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

func testStore(t *testing.T, sessionID string) (*Store, string) {
	t.Helper()
	base := filepath.Join(t.TempDir(), "checkpoints")
	work := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(base, sessionID)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, work
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode.Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode.Perm()); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func requireContains(t *testing.T, paths []string, want string) {
	t.Helper()
	for _, p := range paths {
		if p == want {
			return
		}
	}
	t.Fatalf("expected %q in %v", want, paths)
}

func requireNotContains(t *testing.T, paths []string, unwanted string) {
	t.Helper()
	for _, p := range paths {
		if p == unwanted {
			t.Fatalf("did not expect %q in %v", unwanted, paths)
		}
	}
}

func requireNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be absent, stat err = %v", path, err)
	}
}

func TestUndoRestoresModifiedFileAcrossTurns(t *testing.T) {
	s, work := testStore(t, "session-1")
	file := filepath.Join(work, "a.txt")
	writeTestFile(t, file, "v0", 0o640)

	turn1 := s.BeginTurn(1, "first edit")
	if turn1.TurnID != 1 || turn1.Summary != "first edit" || turn1.Timestamp.IsZero() {
		t.Fatalf("unexpected turn metadata: %+v", turn1)
	}
	if err := RecordBefore(turn1, file); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, file, "v1", 0o640)
	if err := CommitTurn(turn1); err != nil {
		t.Fatal(err)
	}

	rec := turn1.Records[0]
	if rec.Path != file || !rec.ExistedBefore || rec.BeforeHash != hashOf("v0") ||
		!rec.ExistedAfter || rec.AfterHash != hashOf("v1") {
		t.Fatalf("unexpected turn1 record: %+v", rec)
	}
	if rec.BeforeMode.Perm() != 0o640 || rec.AfterMode.Perm() != 0o640 {
		t.Fatalf("unexpected modes: before=%v after=%v", rec.BeforeMode, rec.AfterMode)
	}

	turn2 := s.BeginTurn(2, "second edit")
	if err := RecordBefore(turn2, file); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, file, "v2", 0o600)
	if err := CommitTurn(turn2); err != nil {
		t.Fatal(err)
	}

	list := s.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(list))
	}
	if list[0].TurnID != 1 || list[0].Summary != "first edit" || list[0].FileCount != 1 || len(list[0].Paths) != 1 {
		t.Fatalf("unexpected first list entry: %+v", list[0])
	}
	if list[1].TurnID != 2 || list[1].Summary != "second edit" || list[1].FileCount != 1 {
		t.Fatalf("unexpected second list entry: %+v", list[1])
	}

	res, err := s.Undo()
	if err != nil {
		t.Fatal(err)
	}
	requireContains(t, res.Restored, file)
	if got := readTestFile(t, file); got != "v1" {
		t.Fatalf("after first undo got %q, want v1", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("after first undo mode = %v, want 0640", info.Mode().Perm())
		}
	}
	if list := s.List(); len(list) != 1 || list[0].TurnID != 1 {
		t.Fatalf("after first undo list = %+v", list)
	}

	res, err = s.Undo()
	if err != nil {
		t.Fatal(err)
	}
	requireContains(t, res.Restored, file)
	if got := readTestFile(t, file); got != "v0" {
		t.Fatalf("after second undo got %q, want v0", got)
	}
	if list := s.List(); len(list) != 0 {
		t.Fatalf("after second undo expected empty list, got %+v", list)
	}

	if _, err := s.Undo(); !errors.Is(err, ErrNoTurns) {
		t.Fatalf("expected ErrNoTurns, got %v", err)
	}
}

func TestUndoDeletesCreatedFile(t *testing.T) {
	s, work := testStore(t, "create")
	file := filepath.Join(work, "nested", "new.txt")

	turn := s.BeginTurn(1, "create file")
	if err := RecordBefore(turn, file); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, file, "brand new", 0o644)
	if err := CommitTurn(turn); err != nil {
		t.Fatal(err)
	}

	rec := turn.Records[0]
	if rec.ExistedBefore || rec.BeforeHash != "" || rec.BeforeMode != 0 {
		t.Fatalf("new file should not exist before turn: %+v", rec)
	}
	if !rec.ExistedAfter || rec.AfterHash != hashOf("brand new") {
		t.Fatalf("new file should exist after turn: %+v", rec)
	}

	res, err := s.Undo()
	if err != nil {
		t.Fatal(err)
	}
	requireContains(t, res.Deleted, file)
	requireNotContains(t, res.Restored, file)
	requireNoFile(t, file)
	if list := s.List(); len(list) != 0 {
		t.Fatalf("expected empty list, got %+v", list)
	}
}

func TestUndoRestoresDeletedFileAndMode(t *testing.T) {
	s, work := testStore(t, "delete")
	file := filepath.Join(work, "gone.txt")
	writeTestFile(t, file, "important contents", 0o640)

	turn := s.BeginTurn(1, "delete file")
	if err := RecordBefore(turn, file); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := CommitTurn(turn); err != nil {
		t.Fatal(err)
	}

	rec := turn.Records[0]
	if !rec.ExistedBefore || rec.BeforeHash != hashOf("important contents") {
		t.Fatalf("unexpected before record: %+v", rec)
	}
	if rec.ExistedAfter || rec.AfterHash != "" || rec.AfterMode != 0 {
		t.Fatalf("unexpected after record: %+v", rec)
	}

	res, err := s.Undo()
	if err != nil {
		t.Fatal(err)
	}
	requireContains(t, res.Restored, file)
	requireNotContains(t, res.Deleted, file)
	if got := readTestFile(t, file); got != "important contents" {
		t.Fatalf("restored content = %q", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("restored mode = %v, want 0640", info.Mode().Perm())
		}
	}
}

func TestMultipleEditsSameFileInOneTurn(t *testing.T) {
	s, work := testStore(t, "idempotent")
	file := filepath.Join(work, "same.txt")
	writeTestFile(t, file, "original", 0o644)

	turn := s.BeginTurn(1, "many edits")
	if err := RecordBefore(turn, file); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, file, "intermediate", 0o644)
	if err := RecordBefore(turn, file); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, file, "final", 0o644)
	if err := RecordBefore(turn, file); err != nil {
		t.Fatal(err)
	}
	if err := CommitTurn(turn); err != nil {
		t.Fatal(err)
	}

	if len(turn.Records) != 1 {
		t.Fatalf("expected one deduplicated record, got %d", len(turn.Records))
	}
	rec := turn.Records[0]
	if rec.BeforeHash != hashOf("original") {
		t.Fatalf("pre-turn state was overwritten: %+v", rec)
	}
	if rec.AfterHash != hashOf("final") {
		t.Fatalf("post-turn state = %s, want %s", rec.AfterHash, hashOf("final"))
	}

	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, file); got != "original" {
		t.Fatalf("after undo got %q, want original", got)
	}
}

func TestRewindBeforeAcrossTurns(t *testing.T) {
	s, work := testStore(t, "rewind-before")
	file := filepath.Join(work, "tracked.txt")
	writeTestFile(t, file, "v0", 0o644)
	created := filepath.Join(work, "created-late.txt")

	for i := 1; i <= 3; i++ {
		turn := s.BeginTurn(i, fmt.Sprintf("edit %d", i))
		if err := RecordBefore(turn, file); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, file, fmt.Sprintf("v%d", i), 0o644)
		if i == 3 {
			if err := RecordBefore(turn, created); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, created, "late", 0o644)
		}
		if err := CommitTurn(turn); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.RewindBefore(2)
	if err != nil {
		t.Fatal(err)
	}
	requireContains(t, res.Restored, file)
	requireContains(t, res.Deleted, created)
	if got := readTestFile(t, file); got != "v1" {
		t.Fatalf("after RewindBefore(2) got %q, want v1", got)
	}
	requireNoFile(t, created)
	list := s.List()
	if len(list) != 1 || list[0].TurnID != 1 {
		t.Fatalf("after RewindBefore(2) list = %+v", list)
	}
	if len(res.TurnIDs) != 2 || res.TurnIDs[0] != 3 || res.TurnIDs[1] != 2 {
		t.Fatalf("unexpected undone turn ids: %v", res.TurnIDs)
	}

	res, err = s.RewindBefore(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restored) != 1 || res.Restored[0] != file {
		t.Fatalf("unexpected rewind result: %+v", res)
	}
	if got := readTestFile(t, file); got != "v0" {
		t.Fatalf("after RewindBefore(1) got %q, want v0", got)
	}
	if list := s.List(); len(list) != 0 {
		t.Fatalf("expected empty list, got %+v", list)
	}

	res, err = s.RewindBefore(1)
	if err != nil {
		t.Fatalf("no-op rewind should not error: %v", err)
	}
	if len(res.Restored) != 0 || len(res.Deleted) != 0 || len(res.TurnIDs) != 0 {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

func TestRewindAfterAndRewindTo(t *testing.T) {
	s, work := testStore(t, "rewind-after")
	file := filepath.Join(work, "f.txt")
	writeTestFile(t, file, "v0", 0o644)

	for i := 1; i <= 3; i++ {
		turn := s.BeginTurn(i, fmt.Sprintf("edit %d", i))
		if err := RecordBefore(turn, file); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, file, fmt.Sprintf("v%d", i), 0o644)
		if err := CommitTurn(turn); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.RewindAfter(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restored) != 1 || len(res.TurnIDs) != 2 {
		t.Fatalf("unexpected RewindAfter result: %+v", res)
	}
	if got := readTestFile(t, file); got != "v1" {
		t.Fatalf("after RewindAfter(1) got %q, want v1", got)
	}
	if list := s.List(); len(list) != 1 || list[0].TurnID != 1 {
		t.Fatalf("after RewindAfter(1) list = %+v", list)
	}

	if res, err := s.RewindAfter(1); err != nil || len(res.TurnIDs) != 0 {
		t.Fatalf("repeat RewindAfter should be a no-op, got %+v, %v", res, err)
	}

	res, err = s.RewindTo(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TurnIDs) != 1 || res.TurnIDs[0] != 1 {
		t.Fatalf("unexpected RewindTo result: %+v", res)
	}
	if got := readTestFile(t, file); got != "v0" {
		t.Fatalf("after RewindTo(1) got %q, want v0", got)
	}
}

func TestReloadFromDisk(t *testing.T) {
	base := filepath.Join(t.TempDir(), "checkpoints")
	work := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(base, "reload")
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(work, "a.txt")
	b := filepath.Join(work, "b.txt")
	c := filepath.Join(work, "c.txt")
	writeTestFile(t, a, "a0", 0o644)
	writeTestFile(t, b, "b0", 0o644)

	turn1 := s.BeginTurn(1, "edit a")
	if err := RecordBefore(turn1, a); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, a, "a1", 0o600)
	if err := CommitTurn(turn1); err != nil {
		t.Fatal(err)
	}

	turn2 := s.BeginTurn(2, "edit b and create c")
	for _, p := range []string{b, c} {
		if err := RecordBefore(turn2, p); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, b, "b1", 0o644)
	writeTestFile(t, c, "b1", 0o644) // same content as b, should dedupe
	if err := CommitTurn(turn2); err != nil {
		t.Fatal(err)
	}
	if turn2.Records[1].AfterHash != turn2.Records[0].AfterHash {
		t.Fatalf("identical contents should share a hash: %+v", turn2.Records)
	}

	blobEntries, err := os.ReadDir(filepath.Join(base, "reload", blobsDirName))
	if err != nil {
		t.Fatal(err)
	}
	// unique contents: a0, a1, b0, b1
	if len(blobEntries) != 4 {
		names := make([]string, 0, len(blobEntries))
		for _, e := range blobEntries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		t.Fatalf("expected 4 deduplicated blobs, got %d: %v", len(blobEntries), names)
	}

	reloaded, err := NewStore(base, "reload")
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	list := reloaded.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 reloaded turns, got %d", len(list))
	}
	if list[0].TurnID != 1 || list[0].Summary != "edit a" || list[0].FileCount != 1 {
		t.Fatalf("unexpected reloaded turn 1: %+v", list[0])
	}
	if list[1].TurnID != 2 || list[1].Summary != "edit b and create c" || list[1].FileCount != 2 {
		t.Fatalf("unexpected reloaded turn 2: %+v", list[1])
	}
	if !list[0].Timestamp.Equal(turn1.Timestamp) {
		t.Fatalf("timestamp not persisted: %v vs %v", list[0].Timestamp, turn1.Timestamp)
	}

	// Commit on the reloaded store must not duplicate history.
	turn3 := reloaded.BeginTurn(3, "noop")
	if err := CommitTurn(turn3); err != nil {
		t.Fatal(err)
	}
	if got := len(reloaded.List()); got != 3 {
		t.Fatalf("expected 3 turns after new commit, got %d", got)
	}
	if _, err := reloaded.RewindBefore(3); err != nil {
		t.Fatal(err)
	}

	// Undo using the reloaded records and blobs.
	res, err := reloaded.Undo()
	if err != nil {
		t.Fatalf("undo on reloaded store: %v", err)
	}
	requireContains(t, res.Restored, b)
	requireContains(t, res.Deleted, c)
	if got := readTestFile(t, b); got != "b0" {
		t.Fatalf("b after undo = %q, want b0", got)
	}
	if got := readTestFile(t, a); got != "a1" {
		t.Fatalf("a should be untouched by undo, got %q", got)
	}
	requireNoFile(t, c)

	if list := reloaded.List(); len(list) != 1 || list[0].TurnID != 1 {
		t.Fatalf("unexpected list after undo: %+v", list)
	}

	// The truncated history is persisted too.
	again, err := NewStore(base, "reload")
	if err != nil {
		t.Fatal(err)
	}
	if list := again.List(); len(list) != 1 || list[0].TurnID != 1 {
		t.Fatalf("truncated history not persisted: %+v", list)
	}
}

func TestTurnMutationAfterCommitRejected(t *testing.T) {
	s, work := testStore(t, "errors")
	file := filepath.Join(work, "x.txt")
	writeTestFile(t, file, "x", 0o644)

	turn := s.BeginTurn(1, "commit")
	if err := RecordBefore(turn, file); err != nil {
		t.Fatal(err)
	}
	if err := CommitTurn(turn); err != nil {
		t.Fatal(err)
	}
	if err := RecordBefore(turn, file); !errors.Is(err, ErrTurnCommitted) {
		t.Fatalf("RecordBefore after commit: got %v, want ErrTurnCommitted", err)
	}
	if err := CommitTurn(turn); !errors.Is(err, ErrTurnCommitted) {
		t.Fatalf("second commit: got %v, want ErrTurnCommitted", err)
	}
}

func TestValidationAndErrors(t *testing.T) {
	base := filepath.Join(t.TempDir(), "checkpoints")
	for _, id := range []string{"", ".", "..", "../escape", "a/b", `a\b`} {
		if _, err := NewStore(base, id); !errors.Is(err, ErrInvalidSessionID) {
			t.Fatalf("NewStore(session=%q): got %v, want ErrInvalidSessionID", id, err)
		}
	}
	if _, err := NewStore("", "ok"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("NewStore(empty base): got %v, want ErrInvalidPath", err)
	}

	s, work := testStore(t, "validation")
	if err := RecordBefore(nil, filepath.Join(work, "x")); err == nil {
		t.Fatal("RecordBefore(nil) should fail")
	}
	if err := CommitTurn(nil); err == nil {
		t.Fatal("CommitTurn(nil) should fail")
	}
	if err := CommitTurn(&TurnSnapshot{TurnID: 1}); !errors.Is(err, ErrDetachedTurn) {
		t.Fatalf("detached commit: got %v, want ErrDetachedTurn", err)
	}
	turn := s.BeginTurn(1, "bad")
	if err := RecordBefore(turn, "  "); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("empty path: got %v, want ErrInvalidPath", err)
	}
	dir := filepath.Join(work, "a-directory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RecordBefore(turn, dir); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("directory path: got %v, want ErrNotRegularFile", err)
	}
	if len(turn.Records) != 0 {
		t.Fatalf("failed captures should not record: %+v", turn.Records)
	}
}

func TestRecordBeforeRelativePath(t *testing.T) {
	s, work := testStore(t, "relative")
	file := filepath.Join(work, "rel.txt")
	writeTestFile(t, file, "rel", 0o644)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	turn := s.BeginTurn(1, "relative")
	if err := RecordBefore(turn, "./rel.txt"); err != nil {
		t.Fatal(err)
	}
	expected, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Records[0].Path != expected {
		t.Fatalf("relative path not normalized: got %q, want %q", turn.Records[0].Path, expected)
	}
	writeTestFile(t, file, "changed", 0o644)
	if err := CommitTurn(turn); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, file); got != "rel" {
		t.Fatalf("after undo got %q, want rel", got)
	}
}
