package context

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// defaultMaxEntries is the number of repository entries included in the repo
// map injected into the first user message. It is deliberately small so the
// block stays cheap in tokens.
const defaultMaxEntries = 80

// alwaysSkippedDirs are directories that never contribute to a repository
// index. VCS metadata is never useful as context, node_modules is almost
// always enormous, and .tern stores internal agent state.
var alwaysSkippedDirs = map[string]bool{
	".git":         true,
	".hg":          true,
	".svn":         true,
	".bzr":         true,
	".jj":          true,
	"_darcs":       true,
	"CVS":          true,
	"node_modules": true,
	".tern":        true,
}

// RepoIndex is a cheap snapshot of a working tree. It respects the root
// .gitignore, skips VCS metadata and dependency directories, and records a
// deterministic, size-capped list of relative paths.
type RepoIndex struct {
	Root         string
	FileCount    int
	DirCount     int
	IgnoredCount int
	Duration     time.Duration
	Entries      []string
	Truncated    bool
}

// IndexRepo walks workDir and builds a RepoIndex. maxEntries caps the number
// of entries in RepoIndex.Entries; values <= 0 use defaultMaxEntries.
func IndexRepo(workDir string, maxEntries int) (*RepoIndex, error) {
	start := time.Now()

	root, err := resolveDir(workDir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("context: index repository: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("context: index repository: %s is not a directory", root)
	}

	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}

	ignore := parseGitignore(root)
	index := &RepoIndex{Root: root}
	var files []string

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == root {
			return nil
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			name := d.Name()
			if alwaysSkippedDirs[name] || ignore.ignored(rel, true) {
				index.IgnoredCount++
				return filepath.SkipDir
			}
			index.DirCount++
			return nil
		}

		if ignore.ignored(rel, false) {
			index.IgnoredCount++
			return nil
		}
		index.FileCount++
		files = append(files, rel)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("context: index repository: %w", walkErr)
	}

	// WalkDir is lexical already, but sorting here keeps Entries stable even
	// if the walk implementation changes in the future.
	sort.Strings(files)
	index.Entries = capEntries(files, maxEntries)
	index.Truncated = len(files) > maxEntries
	index.Duration = time.Since(start)
	return index, nil
}

// capEntries returns at most maxEntries paths. When entries are omitted, the
// final slot is used for the directory that was collapsed so the preview
// signals that its contents are not exhaustive.
func capEntries(files []string, maxEntries int) []string {
	if len(files) <= maxEntries {
		return append([]string(nil), files...)
	}
	if maxEntries <= 0 {
		return nil
	}
	if maxEntries == 1 {
		return []string{files[0]}
	}

	kept := append([]string(nil), files[:maxEntries-1]...)
	if dir := path.Dir(files[maxEntries]); dir != "." && dir != "/" {
		kept = append(kept, dir+"/")
	} else {
		kept = append(kept, files[maxEntries-1])
	}
	sort.Strings(kept)
	return kept
}

// Summary returns a concise one-line description of the index.
func (r *RepoIndex) Summary() string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("indexed %d files · %s · .gitignore respected", r.FileCount, formatIndexDuration(r.Duration))
}

// Format renders a compact block suitable for one-time injection into the
// first user message. It returns an empty string when there is nothing to
// show, so callers never emit an empty section.
func (r *RepoIndex) Format() string {
	if r == nil || r.FileCount == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<repo-map>\n")
	b.WriteString(r.Summary())
	b.WriteString("\n")
	for _, entry := range r.Entries {
		b.WriteString(entry)
		b.WriteString("\n")
	}
	if r.Truncated {
		fmt.Fprintf(&b, "... (%d files total)\n", r.FileCount)
	}
	b.WriteString("</repo-map>")
	return b.String()
}

func formatIndexDuration(d time.Duration) string {
	d = d.Round(time.Millisecond)
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d.String()
}

// ---------------------------------------------------------------------------
// .gitignore matching
// ---------------------------------------------------------------------------

type ignoreRule struct {
	pattern  string
	negated  bool
	dirOnly  bool
	anchored bool
}

type ignoreMatcher struct {
	rules []ignoreRule
}

// parseGitignore reads the root .gitignore. A missing or unreadable file is
// treated as an empty rule set: indexing a repository must never fail because
// of an optional ignore file.
func parseGitignore(root string) *ignoreMatcher {
	m := &ignoreMatcher{}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return m
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		rule := ignoreRule{}
		if strings.HasPrefix(trimmed, "!") {
			rule.negated = true
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "!"))
			if trimmed == "" {
				continue
			}
		}
		if strings.HasPrefix(trimmed, "/") {
			rule.anchored = true
			trimmed = strings.TrimPrefix(trimmed, "/")
		}
		if strings.HasSuffix(trimmed, "/") {
			rule.dirOnly = true
			trimmed = strings.TrimSuffix(trimmed, "/")
		}
		if trimmed == "" {
			continue
		}
		// Patterns containing a slash are relative to the .gitignore
		// directory, matching git's semantics.
		if strings.Contains(trimmed, "/") {
			rule.anchored = true
		}
		rule.pattern = trimmed
		m.rules = append(m.rules, rule)
	}
	return m
}

// ignored reports whether rel is ignored. Later rules override earlier ones,
// so a negation can re-include a previously ignored path.
func (m *ignoreMatcher) ignored(rel string, isDir bool) bool {
	if m == nil || rel == "" || rel == "." {
		return false
	}
	ignored := false
	for _, rule := range m.rules {
		if rule.matches(rel) {
			ignored = !rule.negated
		}
	}
	return ignored
}

func (r ignoreRule) matches(rel string) bool {
	rel = strings.Trim(strings.TrimSpace(filepath.ToSlash(rel)), "/")
	if rel == "" {
		return false
	}
	segments := strings.Split(rel, "/")

	// A plain pattern applies to a single entry. Directory-only patterns
	// apply to the directory and, implicitly, to all of its descendants.
	for i := 1; i <= len(segments); i++ {
		if !r.dirOnly && i != len(segments) {
			continue
		}
		target := strings.Join(segments[:i], "/")
		if r.matchTarget(target) {
			return true
		}
	}
	return false
}

func (r ignoreRule) matchTarget(target string) bool {
	if r.anchored {
		return globMatchPath(r.pattern, target)
	}
	base := target
	if i := strings.LastIndex(target, "/"); i >= 0 {
		base = target[i+1:]
	}
	ok, err := path.Match(r.pattern, base)
	if err != nil {
		return r.pattern == base
	}
	return ok
}

// globMatchPath matches slash-separated paths, supporting ** as a wildcard
// that spans any number of path segments.
func globMatchPath(pattern, name string) bool {
	return matchGlobSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchGlobSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchGlobSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pattern[0], name[0])
		if err != nil {
			ok = pattern[0] == name[0]
		}
		if !ok {
			return false
		}
		pattern = pattern[1:]
		name = name[1:]
	}
	return len(name) == 0
}
