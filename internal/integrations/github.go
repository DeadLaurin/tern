package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const ghTimeout = 10 * time.Second

// PullRequestInfo summarizes a GitHub pull request.
type PullRequestInfo struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	State          string `json:"state"`
	URL            string `json:"url"`
	Draft          bool   `json:"draft"`
	HeadRefName    string `json:"head_ref,omitempty"`
	ChecksState    string `json:"checks_state,omitempty"` // passing, failing, pending, none
	ReviewDecision string `json:"review_decision,omitempty"`
}

// WorkflowRunInfo summarizes a GitHub Actions workflow run.
type WorkflowRunInfo struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url"`
	Branch     string `json:"branch,omitempty"`
}

// IssueInfo summarizes a GitHub issue.
type IssueInfo struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	State  string   `json:"state"`
	URL    string   `json:"url"`
	Labels []string `json:"labels,omitempty"`
}

// ReleaseInfo summarizes a GitHub release.
type ReleaseInfo struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	PublishedAt string `json:"published_at,omitempty"`
	IsLatest    bool   `json:"is_latest"`
	URL         string `json:"url,omitempty"`
}

// GitHubStatus reports GitHub integration state for the current repository.
type GitHubStatus struct {
	Available   bool             `json:"available"`
	GHInstalled bool             `json:"gh_installed"`
	Repo        string           `json:"repo,omitempty"`
	PR          *PullRequestInfo `json:"pr,omitempty"`
	LatestRun   *WorkflowRunInfo `json:"latest_run,omitempty"`
	Error       string           `json:"error,omitempty"`
}

// GetGitHubStatus inspects GitHub PR and Actions status for workDir using `gh`.
func GetGitHubStatus(ctx context.Context, workDir, repoSlug string) GitHubStatus {
	st := GitHubStatus{Repo: repoSlug}
	if _, err := exec.LookPath("gh"); err != nil {
		return st
	}
	st.GHInstalled = true
	if repoSlug == "" {
		return st
	}
	st.Available = true

	// Current branch PR (if any).
	if raw, err := runCmd(ctx, ghTimeout, workDir, "gh", "pr", "view", "--json", "number,title,state,url,isDraft,headRefName,reviewDecision,statusCheckRollup"); err == nil && strings.TrimSpace(raw) != "" {
		var wire struct {
			Number            int    `json:"number"`
			Title             string `json:"title"`
			State             string `json:"state"`
			URL               string `json:"url"`
			IsDraft           bool   `json:"isDraft"`
			HeadRefName       string `json:"headRefName"`
			ReviewDecision    string `json:"reviewDecision"`
			StatusCheckRollup []struct {
				State      string `json:"state"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"statusCheckRollup"`
		}
		if json.Unmarshal([]byte(raw), &wire) == nil && wire.Number > 0 {
			checks := "none"
			if len(wire.StatusCheckRollup) > 0 {
				checks = "passing"
				for _, c := range wire.StatusCheckRollup {
					conc := strings.ToUpper(c.Conclusion)
					state := strings.ToUpper(c.State)
					status := strings.ToUpper(c.Status)
					if conc == "FAILURE" || conc == "TIMED_OUT" || conc == "CANCELLED" || state == "FAILURE" || state == "ERROR" {
						checks = "failing"
						break
					}
					if status == "IN_PROGRESS" || status == "QUEUED" || state == "PENDING" || (conc == "" && state == "") {
						checks = "pending"
					}
				}
			}
			st.PR = &PullRequestInfo{
				Number:         wire.Number,
				Title:          wire.Title,
				State:          wire.State,
				URL:            wire.URL,
				Draft:          wire.IsDraft,
				HeadRefName:    wire.HeadRefName,
				ChecksState:    checks,
				ReviewDecision: wire.ReviewDecision,
			}
		}
	}

	// Latest workflow run.
	if raw, err := runCmd(ctx, ghTimeout, workDir, "gh", "run", "list", "--limit", "1", "--json", "name,status,conclusion,url,headBranch"); err == nil && strings.TrimSpace(raw) != "" {
		var runs []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			URL        string `json:"url"`
			HeadBranch string `json:"headBranch"`
		}
		if json.Unmarshal([]byte(raw), &runs) == nil && len(runs) > 0 {
			r := runs[0]
			st.LatestRun = &WorkflowRunInfo{
				Name:       r.Name,
				Status:     r.Status,
				Conclusion: r.Conclusion,
				URL:        r.URL,
				Branch:     r.HeadBranch,
			}
		}
	}

	return st
}

// CreatePR creates a GitHub pull request for the current branch using `gh`.
func CreatePR(ctx context.Context, workDir, title, body, base string, draft bool) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", errors.New("gh CLI not found (install from https://cli.github.com)")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return "", errors.New("PR title is required")
	}
	args := []string{"pr", "create", "--title", title, "--body", strings.TrimSpace(body)}
	if strings.TrimSpace(base) != "" {
		args = append(args, "--base", strings.TrimSpace(base))
	}
	if draft {
		args = append(args, "--draft")
	}
	out, err := runCmd(ctx, 30*time.Second, workDir, "gh", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ListPRs lists open GitHub pull requests in the repository.
func ListPRs(ctx context.Context, workDir string, limit int) ([]PullRequestInfo, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, errors.New("gh CLI not found")
	}
	if limit <= 0 {
		limit = 10
	}
	raw, err := runCmd(ctx, ghTimeout, workDir, "gh", "pr", "list", "--limit", strconv.Itoa(limit), "--json", "number,title,state,url,isDraft,headRefName")
	if err != nil {
		return nil, err
	}
	var wire []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		State       string `json:"state"`
		URL         string `json:"url"`
		IsDraft     bool   `json:"isDraft"`
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return nil, err
	}
	out := make([]PullRequestInfo, 0, len(wire))
	for _, w := range wire {
		out = append(out, PullRequestInfo{
			Number:      w.Number,
			Title:       w.Title,
			State:       w.State,
			URL:         w.URL,
			Draft:       w.IsDraft,
			HeadRefName: w.HeadRefName,
		})
	}
	return out, nil
}

// ListIssues lists open GitHub issues in the repository.
func ListIssues(ctx context.Context, workDir string, limit int) ([]IssueInfo, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, errors.New("gh CLI not found")
	}
	if limit <= 0 {
		limit = 10
	}
	raw, err := runCmd(ctx, ghTimeout, workDir, "gh", "issue", "list", "--limit", strconv.Itoa(limit), "--json", "number,title,state,url,labels")
	if err != nil {
		return nil, err
	}
	var wire []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		URL    string `json:"url"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return nil, err
	}
	out := make([]IssueInfo, 0, len(wire))
	for _, w := range wire {
		labels := make([]string, 0, len(w.Labels))
		for _, l := range w.Labels {
			labels = append(labels, l.Name)
		}
		out = append(out, IssueInfo{
			Number: w.Number,
			Title:  w.Title,
			State:  w.State,
			URL:    w.URL,
			Labels: labels,
		})
	}
	return out, nil
}

// ListReleases lists recent GitHub releases in the repository.
func ListReleases(ctx context.Context, workDir string, limit int) ([]ReleaseInfo, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, errors.New("gh CLI not found")
	}
	if limit <= 0 {
		limit = 10
	}
	raw, err := runCmd(ctx, ghTimeout, workDir, "gh", "release", "list", "--limit", strconv.Itoa(limit), "--json", "tagName,name,publishedAt,isLatest")
	if err != nil {
		return nil, err
	}
	var wire []struct {
		TagName     string `json:"tagName"`
		Name        string `json:"name"`
		PublishedAt string `json:"publishedAt"`
		IsLatest    bool   `json:"isLatest"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return nil, err
	}
	out := make([]ReleaseInfo, 0, len(wire))
	for _, w := range wire {
		out = append(out, ReleaseInfo{
			TagName:     w.TagName,
			Name:        w.Name,
			PublishedAt: w.PublishedAt,
			IsLatest:    w.IsLatest,
		})
	}
	return out, nil
}

// FormatGitHubSummary builds a compact human-readable report for /gh.
func FormatGitHubSummary(st GitHubStatus) string {
	if st.Repo == "" {
		return "No GitHub remote detected for this workspace."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub: %s", st.Repo)
	if !st.GHInstalled {
		b.WriteString("\n(gh CLI not installed — install from https://cli.github.com for PR/CI actions)")
		return b.String()
	}
	if st.PR != nil {
		state := st.PR.State
		if st.PR.Draft {
			state = "DRAFT"
		}
		fmt.Fprintf(&b, "\nPR #%d [%s] %s", st.PR.Number, state, st.PR.Title)
		if st.PR.ChecksState != "" && st.PR.ChecksState != "none" {
			fmt.Fprintf(&b, " (checks: %s)", st.PR.ChecksState)
		}
		if st.PR.URL != "" {
			fmt.Fprintf(&b, "\n  %s", st.PR.URL)
		}
	} else {
		b.WriteString("\nPR: none open for current branch")
	}
	if st.LatestRun != nil {
		res := st.LatestRun.Conclusion
		if res == "" {
			res = st.LatestRun.Status
		}
		fmt.Fprintf(&b, "\nCI: %s — %s", st.LatestRun.Name, res)
	}
	return b.String()
}
