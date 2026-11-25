package context

import (
	stdcontext "context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	gitTimeout  = 3 * time.Second
	dateLayout  = "2006-01-02"
	contextTags = "environment"
)

// BuildInitialContext builds the small, volatile environment block injected
// into the first user message of a session. It must not be placed in the
// system prompt: the date, git branch, and git status change between sessions
// and would otherwise invalidate DeepSeek's prefix cache.
func BuildInitialContext(workDir string, now time.Time) string {
	dir, err := resolveDir(workDir)
	if err != nil {
		dir = filepath.Clean(workDir)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<%s>\n", contextTags)
	fmt.Fprintf(&b, "Working directory: %s\n", dir)
	fmt.Fprintf(&b, "Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "Date: %s\n", now.Format(dateLayout))

	if insideGitRepo(dir) {
		if branch := gitBranch(dir); branch != "" {
			fmt.Fprintf(&b, "Git branch: %s\n", branch)
		}
		if status := runGit(dir, "status", "--short"); status != "" {
			b.WriteString("Git status:\n")
			b.WriteString(status)
			b.WriteString("\n")
		} else {
			b.WriteString("Git status: clean\n")
		}
	}

	fmt.Fprintf(&b, "</%s>", contextTags)
	return b.String()
}

func insideGitRepo(dir string) bool {
	return runGit(dir, "rev-parse", "--is-inside-work-tree") == "true"
}

func gitBranch(dir string) string {
	if branch := runGit(dir, "rev-parse", "--abbrev-ref", "HEAD"); branch != "" && branch != "HEAD" {
		return branch
	}
	// Handles repositories whose HEAD is not born yet (git init with no
	// commits), where rev-parse --abbrev-ref HEAD can fail.
	return runGit(dir, "symbolic-ref", "--short", "HEAD")
}

func runGit(dir string, args ...string) string {
	ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), gitTimeout)
	defer cancel()

	cmdArgs := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
