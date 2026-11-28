// Package integrations provides lightweight Git, GitHub, and Cloudflare
// integrations for tern's TUI, Web UI, and slash commands.
package integrations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tern-agent/tern/internal/provider"
)

const (
	defaultGitTimeout = 15 * time.Second
	pushTimeout       = 60 * time.Second
	maxDiffBytes      = 64 * 1024
)

// FileChange represents one modified, staged, or untracked path in a git repo.
type FileChange struct {
	Path      string `json:"path"`
	Status    string `json:"status"` // e.g. "M", "A", "D", "R", "?"
	Staged    bool   `json:"staged"`
	Unstaged  bool   `json:"unstaged"`
	Untracked bool   `json:"untracked"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
}

// GitStatus is a point-in-time snapshot of a git working tree.
type GitStatus struct {
	IsRepo       bool         `json:"is_repo"`
	Branch       string       `json:"branch"`
	Upstream     string       `json:"upstream,omitempty"`
	Ahead        int          `json:"ahead"`
	Behind       int          `json:"behind"`
	Files        []FileChange `json:"files"`
	StagedCount  int          `json:"staged_count"`
	TotalAdded   int          `json:"total_added"`
	TotalRemoved int          `json:"total_removed"`
	RemoteURL    string       `json:"remote_url,omitempty"`
	RepoSlug     string       `json:"repo_slug,omitempty"`
	Error        string       `json:"error,omitempty"`
}

// CommitResult summarizes a git commit (and optional push).
type CommitResult struct {
	SHA        string `json:"sha"`
	Summary    string `json:"summary"`
	Pushed     bool   `json:"pushed"`
	PushOutput string `json:"push_output,omitempty"`
}

// nonInteractiveEnv returns process environment with prompts/pagers disabled.
func nonInteractiveEnv() []string {
	env := os.Environ()
	defaults := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"PAGER":               "cat",
		"GIT_PAGER":           "cat",
		"CI":                  "true",
	}
	present := make(map[string]bool, len(env))
	for _, kv := range env {
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			present[strings.ToUpper(kv[:idx])] = true
		}
	}
	for k, v := range defaults {
		if !present[k] {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func runCmd(ctx context.Context, timeout time.Duration, workDir, name string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = workDir
	cmd.Env = nonInteractiveEnv()
	cmd.Stdin = nil
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimRight(buf.String(), "\r\n")
	if err != nil {
		if out != "" {
			return out, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), out)
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// Status inspects the git working tree in workDir.
func Status(ctx context.Context, workDir string) GitStatus {
	if strings.TrimSpace(workDir) == "" {
		return GitStatus{}
	}
	top, err := runCmd(ctx, defaultGitTimeout, workDir, "git", "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(top) != "true" {
		return GitStatus{IsRepo: false}
	}

	st := GitStatus{IsRepo: true}
	raw, err := runCmd(ctx, defaultGitTimeout, workDir, "git", "status", "--porcelain=v2", "--branch")
	if err != nil {
		st.Error = err.Error()
		return st
	}

	fileMap := make(map[string]*FileChange)
	ensure := func(p string) *FileChange {
		fc, ok := fileMap[p]
		if !ok {
			fc = &FileChange{Path: p}
			fileMap[p] = fc
		}
		return fc
	}

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			st.Branch = strings.TrimSpace(strings.TrimPrefix(line, "# branch.head "))
		case strings.HasPrefix(line, "# branch.upstream "):
			st.Upstream = strings.TrimSpace(strings.TrimPrefix(line, "# branch.upstream "))
		case strings.HasPrefix(line, "# branch.ab "):
			parts := strings.Fields(strings.TrimPrefix(line, "# branch.ab "))
			for _, p := range parts {
				if strings.HasPrefix(p, "+") {
					st.Ahead, _ = strconv.Atoi(p[1:])
				} else if strings.HasPrefix(p, "-") {
					st.Behind, _ = strconv.Atoi(p[1:])
				}
			}
		case strings.HasPrefix(line, "1 ") || strings.HasPrefix(line, "2 "):
			// Ordinary (1) or rename/copy (2) changed entry.
			fields := strings.Fields(line)
			if len(fields) < 9 {
				continue
			}
			xy := fields[1]
			var path string
			if strings.HasPrefix(line, "2 ") {
				// Format: 2 XY sub mH mI mW hH hI Xscore path\torigPath
				if len(fields) >= 10 {
					rest := strings.Join(fields[9:], " ")
					if tab := strings.IndexByte(rest, '\t'); tab >= 0 {
						path = rest[:tab]
					} else {
						path = rest
					}
				}
			} else {
				// Find path after 8th field preserving spaces.
				path = nthFieldTail(line, 8)
			}
			if path == "" {
				continue
			}
			fc := ensure(path)
			if len(xy) >= 1 && xy[0] != '.' {
				fc.Staged = true
				fc.Status = string(xy[0])
			}
			if len(xy) >= 2 && xy[1] != '.' {
				fc.Unstaged = true
				if fc.Status == "" {
					fc.Status = string(xy[1])
				}
			}
		case strings.HasPrefix(line, "u "):
			path := nthFieldTail(line, 10)
			if path != "" {
				fc := ensure(path)
				fc.Unstaged = true
				fc.Status = "U"
			}
		case strings.HasPrefix(line, "? "):
			path := strings.TrimPrefix(line, "? ")
			if path != "" {
				fc := ensure(path)
				fc.Untracked = true
				fc.Status = "?"
			}
		}
	}

	// Collect numstat for unstaged and staged changes.
	applyNumstat := func(args ...string) {
		out, err := runCmd(ctx, defaultGitTimeout, workDir, "git", args...)
		if err != nil || out == "" {
			return
		}
		for _, line := range strings.Split(out, "\n") {
			parts := strings.Split(line, "\t")
			if len(parts) < 3 {
				continue
			}
			add, _ := strconv.Atoi(parts[0])
			del, _ := strconv.Atoi(parts[1])
			p := parts[2]
			if fc, ok := fileMap[p]; ok {
				fc.Added += add
				fc.Removed += del
			}
		}
	}
	applyNumstat("diff", "--numstat")
	applyNumstat("diff", "--cached", "--numstat")

	files := make([]FileChange, 0, len(fileMap))
	for _, fc := range fileMap {
		if fc.Untracked && fc.Added == 0 {
			fc.Added = countFileLines(filepath.Join(workDir, fc.Path))
		}
		if fc.Staged {
			st.StagedCount++
		}
		st.TotalAdded += fc.Added
		st.TotalRemoved += fc.Removed
		files = append(files, *fc)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Staged != files[j].Staged {
			return files[i].Staged
		}
		return files[i].Path < files[j].Path
	})
	st.Files = files

	if remote, err := runCmd(ctx, defaultGitTimeout, workDir, "git", "remote", "get-url", "origin"); err == nil {
		st.RemoteURL = strings.TrimSpace(remote)
		st.RepoSlug = ParseGitHubSlug(st.RemoteURL)
	}
	return st
}

func nthFieldTail(line string, skip int) string {
	i := 0
	n := len(line)
	for f := 0; f < skip; f++ {
		for i < n && line[i] == ' ' {
			i++
		}
		for i < n && line[i] != ' ' {
			i++
		}
	}
	for i < n && line[i] == ' ' {
		i++
	}
	if i >= n {
		return ""
	}
	return line[i:]
}

func countFileLines(absPath string) int {
	data, err := os.ReadFile(absPath)
	if err != nil || len(data) == 0 || bytes.IndexByte(data, 0) >= 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

var githubSlugRe = regexp.MustCompile(`github\.com[:/]([^/\s]+/[^/\s]+?)(?:\.git)?$`)

// ParseGitHubSlug extracts "owner/repo" from an SSH or HTTPS GitHub remote URL.
func ParseGitHubSlug(remoteURL string) string {
	remoteURL = strings.TrimSpace(remoteURL)
	m := githubSlugRe.FindStringSubmatch(remoteURL)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// Diff returns the unified diff for one path (or the entire repo if path == "").
func Diff(ctx context.Context, workDir, path string, staged bool) (string, error) {
	if strings.TrimSpace(workDir) == "" {
		return "", errors.New("workDir is empty")
	}
	path = strings.TrimSpace(path)
	if path != "" {
		// Check if untracked.
		stOut, _ := runCmd(ctx, defaultGitTimeout, workDir, "git", "status", "--porcelain", "--", path)
		if strings.HasPrefix(stOut, "?? ") && !staged {
			abs := filepath.Join(workDir, path)
			data, err := os.ReadFile(abs)
			if err != nil {
				return "", err
			}
			if bytes.IndexByte(data, 0) >= 0 {
				return fmt.Sprintf("diff --git a/%s b/%s\nnew file (binary, %d bytes)", path, path, len(data)), nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "diff --git a/%s b/%s\nnew file\n--- /dev/null\n+++ b/%s\n", path, path, path)
			lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", len(lines))
			for _, l := range lines {
				b.WriteByte('+')
				b.WriteString(l)
				b.WriteByte('\n')
				if b.Len() > maxDiffBytes {
					b.WriteString("\n[... diff truncated ...]\n")
					break
				}
			}
			return b.String(), nil
		}
	}

	args := []string{"diff"}
	if staged {
		args = append(args, "--cached")
	}
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := runCmd(ctx, defaultGitTimeout, workDir, "git", args...)
	if err != nil {
		return "", err
	}
	// If unstaged diff for a specific file is empty, fall back to staged diff.
	if out == "" && path != "" && !staged {
		out, _ = runCmd(ctx, defaultGitTimeout, workDir, "git", "diff", "--cached", "--", path)
	}
	if len(out) > maxDiffBytes {
		out = out[:maxDiffBytes] + "\n[... diff truncated ...]"
	}
	return out, nil
}

// Stage adds the specified paths (or all changes when paths is empty).
func Stage(ctx context.Context, workDir string, paths ...string) error {
	args := []string{"add"}
	if len(paths) == 0 {
		args = append(args, "-A")
	} else {
		args = append(args, "--")
		args = append(args, paths...)
	}
	_, err := runCmd(ctx, defaultGitTimeout, workDir, "git", args...)
	return err
}

// Unstage removes the specified paths (or all staged files when paths is empty)
// from the git index.
func Unstage(ctx context.Context, workDir string, paths ...string) error {
	args := []string{"reset", "HEAD"}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	_, err := runCmd(ctx, defaultGitTimeout, workDir, "git", args...)
	return err
}

// Commit creates a git commit with message. When stageAll is true (or nothing
// is staged yet and stageAll is requested), all changes are staged first.
func Commit(ctx context.Context, workDir, message string, stageAll bool) (CommitResult, error) {
	msg := strings.TrimSpace(message)
	if msg == "" {
		return CommitResult{}, errors.New("commit message is required")
	}
	if stageAll {
		if err := Stage(ctx, workDir); err != nil {
			return CommitResult{}, err
		}
	}
	out, err := runCmd(ctx, defaultGitTimeout, workDir, "git", "commit", "-m", msg)
	if err != nil {
		return CommitResult{}, err
	}
	sha, _ := runCmd(ctx, defaultGitTimeout, workDir, "git", "rev-parse", "--short", "HEAD")
	firstLine := out
	if nl := strings.IndexByte(out, '\n'); nl >= 0 {
		firstLine = out[:nl]
	}
	return CommitResult{
		SHA:     strings.TrimSpace(sha),
		Summary: strings.TrimSpace(firstLine),
	}, nil
}

// Push pushes the current branch to its upstream (or sets upstream on origin).
func Push(ctx context.Context, workDir string) (string, error) {
	out, err := runCmd(ctx, pushTimeout, workDir, "git", "push")
	if err == nil {
		if out == "" {
			return "Pushed successfully", nil
		}
		return out, nil
	}
	// If no upstream is configured yet, retry with -u origin <branch>.
	if strings.Contains(err.Error(), "no upstream branch") || strings.Contains(err.Error(), "has no upstream") {
		branch, berr := runCmd(ctx, defaultGitTimeout, workDir, "git", "rev-parse", "--abbrev-ref", "HEAD")
		if berr == nil && strings.TrimSpace(branch) != "" && strings.TrimSpace(branch) != "HEAD" {
			out2, err2 := runCmd(ctx, pushTimeout, workDir, "git", "push", "-u", "origin", strings.TrimSpace(branch))
			if err2 == nil {
				if out2 == "" {
					return "Pushed and set upstream origin/" + strings.TrimSpace(branch), nil
				}
				return out2, nil
			}
			return out2, err2
		}
	}
	return out, err
}

// CommitAndPush commits changes and pushes them in one call.
func CommitAndPush(ctx context.Context, workDir, message string, stageAll bool) (CommitResult, error) {
	res, err := Commit(ctx, workDir, message, stageAll)
	if err != nil {
		return res, err
	}
	pushOut, err := Push(ctx, workDir)
	if err != nil {
		return res, fmt.Errorf("committed %s, but push failed: %w", res.SHA, err)
	}
	res.Pushed = true
	res.PushOutput = pushOut
	return res, nil
}

// DraftCommitMessage generates a concise, single-line commit message from the
// current repository diff using the active AI provider.
func DraftCommitMessage(ctx context.Context, workDir string, prov provider.Provider, model string) (string, error) {
	st := Status(ctx, workDir)
	if !st.IsRepo {
		return "", errors.New("not a git repository")
	}
	if len(st.Files) == 0 {
		return "", errors.New("no changes to commit")
	}

	diffText, _ := Diff(ctx, workDir, "", st.StagedCount > 0)
	if diffText == "" {
		diffText, _ = Diff(ctx, workDir, "", false)
	}
	const maxPromptDiff = 8000
	if len(diffText) > maxPromptDiff {
		diffText = diffText[:maxPromptDiff] + "\n[... truncated ...]"
	}

	var fileSummary strings.Builder
	for i, f := range st.Files {
		if i >= 25 {
			fmt.Fprintf(&fileSummary, "... and %d more files\n", len(st.Files)-i)
			break
		}
		fmt.Fprintf(&fileSummary, "%s %s (+%d -%d)\n", f.Status, f.Path, f.Added, f.Removed)
	}

	if prov == nil || strings.TrimSpace(model) == "" {
		return fallbackCommitMessage(st), nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	ch, err := prov.Stream(reqCtx, provider.ChatRequest{
		Model: model,
		Messages: []provider.Message{
			{
				Role: provider.RoleSystem,
				Content: "Write a single concise conventional commit message (max 72 chars, e.g. 'feat(tui): add git diff sidebar'). " +
					"Output ONLY the commit message line with no quotes, no markdown, and no explanation.",
			},
			{
				Role:    provider.RoleUser,
				Content: "Changed files:\n" + fileSummary.String() + "\nDiff:\n" + diffText,
			},
		},
		MaxTokens:       128,
		ReasoningEffort: "none",
		Stream:          true,
	})
	if err != nil {
		return fallbackCommitMessage(st), nil
	}

	var out strings.Builder
	for ev := range ch {
		if ev.Type == provider.EventTextDelta {
			out.WriteString(ev.Text)
		}
	}
	msg := cleanCommitMessage(out.String())
	if msg == "" {
		return fallbackCommitMessage(st), nil
	}
	return msg, nil
}

func cleanCommitMessage(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "`\"'")
	if nl := strings.IndexByte(raw, '\n'); nl >= 0 {
		raw = strings.TrimSpace(raw[:nl])
	}
	raw = strings.Trim(raw, "`\"'")
	return strings.TrimSpace(raw)
}

func fallbackCommitMessage(st GitStatus) string {
	if len(st.Files) == 1 {
		return "chore: update " + filepath.Base(st.Files[0].Path)
	}
	return fmt.Sprintf("chore: update %d files", len(st.Files))
}
