package subagent

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/tern-agent/tern/internal/permission"
)

// PathLocker is a cooperative per-path write lock table.
//
// It exists so that two write-capable sub-agents never edit the same file at
// the same time. A lock is advisory: every write path in the runner acquires
// it before invoking a mutating tool.
type PathLocker struct {
	mu    sync.Mutex
	locks map[string]*pathLock
}

type pathLock struct {
	held    bool
	holder  string
	waiters []chan struct{}
}

// NewPathLocker returns an empty lock table.
func NewPathLocker() *PathLocker {
	return &PathLocker{locks: make(map[string]*pathLock)}
}

// Acquire waits until path is free or ctx is done. The returned release
// function is idempotent.
func (l *PathLocker) Acquire(ctx context.Context, path string) (func(), error) {
	return l.acquire(ctx, path, "")
}

// AcquireAs is Acquire with a holder label used in conflict messages.
func (l *PathLocker) AcquireAs(ctx context.Context, path, holder string) (func(), error) {
	return l.acquire(ctx, path, holder)
}

func (l *PathLocker) acquire(ctx context.Context, path, holder string) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := lockKey(path)
	for {
		l.mu.Lock()
		entry := l.locks[key]
		if entry == nil {
			entry = &pathLock{}
			l.locks[key] = entry
		}
		if !entry.held {
			entry.held = true
			entry.holder = holder
			l.mu.Unlock()
			return l.releaseFunc(key), nil
		}
		ch := make(chan struct{})
		entry.waiters = append(entry.waiters, ch)
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			l.removeWaiter(key, ch)
			return nil, fmt.Errorf("subagent: waiting for write lock on %s: %w", key, ctx.Err())
		case <-ch:
		}
	}
}

// TryAcquire acquires path immediately or returns a descriptive error.
func (l *PathLocker) TryAcquire(path string) (func(), error) {
	return l.TryAcquireAs(path, "")
}

// TryAcquireAs is TryAcquire with a holder label.
func (l *PathLocker) TryAcquireAs(path, holder string) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	key := lockKey(path)
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.locks[key]
	if entry == nil {
		entry = &pathLock{}
		l.locks[key] = entry
	}
	if entry.held {
		who := entry.holder
		if who == "" {
			who = "another sub-agent"
		}
		return nil, fmt.Errorf("subagent: %s is already being edited by %s; wait for it to finish or use a different file", key, who)
	}
	entry.held = true
	entry.holder = holder
	return l.releaseFunc(key), nil
}

// AcquireImmediate is an alias for TryAcquire, matching the name used in the
// design notes.
func (l *PathLocker) AcquireImmediate(path string) (func(), error) {
	return l.TryAcquire(path)
}

// AcquireMany acquires several paths in sorted order to avoid deadlocks.
func (l *PathLocker) AcquireMany(ctx context.Context, paths []string) (func(), error) {
	keys := make([]string, 0, len(paths))
	for _, p := range paths {
		keys = append(keys, lockKey(p))
	}
	sort.Strings(keys)
	releases := make([]func(), 0, len(keys))
	for _, key := range keys {
		release, err := l.acquire(ctx, key, "")
		if err != nil {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
			return nil, err
		}
		releases = append(releases, release)
	}
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}, nil
}

// Locked reports whether path is currently locked. It is intended for tests
// and diagnostics.
func (l *PathLocker) Locked(path string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.locks[lockKey(path)]
	return entry != nil && entry.held
}

func (l *PathLocker) removeWaiter(key string, ch chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.locks[key]
	if entry == nil {
		return
	}
	for i, waiter := range entry.waiters {
		if waiter == ch {
			entry.waiters = append(entry.waiters[:i], entry.waiters[i+1:]...)
			break
		}
	}
	if !entry.held && len(entry.waiters) == 0 {
		delete(l.locks, key)
	}
}

func (l *PathLocker) releaseFunc(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			entry := l.locks[key]
			if entry == nil {
				return
			}
			entry.held = false
			entry.holder = ""
			if len(entry.waiters) > 0 {
				ch := entry.waiters[0]
				entry.waiters = entry.waiters[1:]
				close(ch)
			}
			if !entry.held && len(entry.waiters) == 0 {
				delete(l.locks, key)
			}
		})
	}
}

func lockKey(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "<unknown>"
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

// ---------------------------------------------------------------------------
// Isolated workcopy
// ---------------------------------------------------------------------------

// Workcopy is a disposable copy of a workspace. Write agents running in
// isolated mode edit the copy; the parent receives a unified patch.
type Workcopy struct {
	Source string
	Dir    string
}

// PrepareWorkcopy copies workDir into a fresh temporary directory, skipping
// VCS metadata, dependency trees and common build artifacts.
func PrepareWorkcopy(workDir string) (*Workcopy, error) {
	src, err := filepath.Abs(strings.TrimSpace(workDir))
	if err != nil {
		return nil, fmt.Errorf("subagent: resolve workdir: %w", err)
	}
	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("subagent: stat workdir %s: %w", src, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("subagent: workdir %s is not a directory", src)
	}
	dst, err := os.MkdirTemp("", "tern-subagent-*")
	if err != nil {
		return nil, fmt.Errorf("subagent: create workcopy: %w", err)
	}
	if err := copyWorkspace(src, dst); err != nil {
		os.RemoveAll(dst)
		return nil, err
	}
	return &Workcopy{Source: src, Dir: dst}, nil
}

// NewWorkcopy is an alias for PrepareWorkcopy.
func NewWorkcopy(workDir string) (*Workcopy, error) { return PrepareWorkcopy(workDir) }

// Cleanup removes the temporary workcopy.
func (w *Workcopy) Cleanup() error {
	if w == nil || w.Dir == "" {
		return nil
	}
	if filepath.Base(w.Dir) == filepath.Base(w.Source) {
		return fmt.Errorf("subagent: refusing to remove workcopy %s", w.Dir)
	}
	return os.RemoveAll(w.Dir)
}

// Patch computes a unified diff of every changed, added and deleted file
// relative to the source workspace. Paths use forward slashes and are sorted.
func (w *Workcopy) Patch() (string, []string, error) {
	if w == nil {
		return "", nil, nil
	}
	files, err := workspaceFiles(w.Source)
	if err != nil {
		return "", nil, err
	}
	copied, err := workspaceFiles(w.Dir)
	if err != nil {
		return "", nil, err
	}
	all := make(map[string]struct{}, len(files)+len(copied))
	for rel := range files {
		all[rel] = struct{}{}
	}
	for rel := range copied {
		all[rel] = struct{}{}
	}
	relPaths := make([]string, 0, len(all))
	for rel := range all {
		relPaths = append(relPaths, rel)
	}
	sort.Strings(relPaths)

	var patch strings.Builder
	changed := make([]string, 0)
	for _, rel := range relPaths {
		oldPath, oldOK := files[rel]
		newPath, newOK := copied[rel]
		var oldContent, newContent string
		if oldOK {
			data, err := os.ReadFile(oldPath)
			if err != nil {
				return "", nil, fmt.Errorf("subagent: read %s: %w", oldPath, err)
			}
			oldContent = string(data)
		}
		if newOK {
			data, err := os.ReadFile(newPath)
			if err != nil {
				return "", nil, fmt.Errorf("subagent: read %s: %w", newPath, err)
			}
			newContent = string(data)
		}
		diff := unifiedDiffFor(rel, oldContent, newContent)
		if diff == "" {
			continue
		}
		patch.WriteString(diff)
		if !strings.HasSuffix(diff, "\n") {
			patch.WriteByte('\n')
		}
		changed = append(changed, rel)
	}
	return patch.String(), changed, nil
}

// ChangedFiles returns just the sorted relative paths that differ.
func (w *Workcopy) ChangedFiles() ([]string, error) {
	_, changed, err := w.Patch()
	return changed, err
}

func workspaceFiles(root string) (map[string]string, error) {
	out := make(map[string]string)
	root = filepath.Clean(root)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if skipWorkspaceEntry(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if skipWorkspaceEntry(name) {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			// Symlinks are not copied, so they cannot be safely patched.
			return nil
		}
		if !d.Type().IsRegular() && d.Type() != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = path
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("subagent: walk %s: %w", root, err)
	}
	return out, nil
}

func copyWorkspace(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if path != src && skipWorkspaceEntry(name) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		return os.WriteFile(target, data, mode)
	})
}

func skipWorkspaceEntry(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules",
		"dist", "build", "target", "bin", "obj", "out", ".next", ".nuxt",
		".cache", "coverage", ".pytest_cache", ".mypy_cache", "__pycache__",
		".venv", "venv", ".tox", ".gradle", ".terraform", ".DS_Store":
		return true
	}
	lower := strings.ToLower(name)
	for _, ext := range []string{".o", ".a", ".so", ".dylib", ".dll", ".exe", ".class", ".pyc", ".pyo", ".wasm", ".test"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// unifiedDiffFor indents deleted/added contents that lack a trailing newline
// in the same way permission.UnifiedDiff expects, and avoids diffing binary
// blobs (NUL bytes) to keep patches readable.
func unifiedDiffFor(rel, oldContent, newContent string) string {
	if oldContent == newContent {
		return ""
	}
	if strings.ContainsRune(oldContent, 0) || strings.ContainsRune(newContent, 0) {
		return ""
	}
	return permission.UnifiedDiff(rel, oldContent, newContent)
}

func permissionDiff(rel, oldContent, newContent string) string {
	return permission.UnifiedDiff(rel, oldContent, newContent)
}
