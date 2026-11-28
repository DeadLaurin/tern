package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tern-agent/tern/internal/integrations"
	"github.com/tern-agent/tern/internal/provider"
)

type gitStatusMsg struct {
	git           integrations.GitStatus
	gh            integrations.GitHubStatus
	cf            integrations.CloudflareStatus
	includeRemote bool
}

type gitDiffMsg struct {
	path string
	diff string
	err  error
}

type gitActionMsg struct {
	text       string
	err        error
	refreshGit bool
}

type gitAIMsgResult struct {
	message string
	err     error
}

type gitModalState struct {
	fileIndex   int
	diffPath    string
	diffLines   []string
	diffScroll  int
	diffLoading bool

	commitFocus bool
	commitInput []rune
	prFocus     bool
	prInput     []rune

	busy     bool
	feedback string
	isErr    bool
}

func (m *Model) fetchGitStatusCmd(includeRemote bool) tea.Cmd {
	workDir := strings.TrimSpace(m.opts.WorkDir)
	if workDir == "" {
		return nil
	}
	baseCtx := m.baseCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	return func() tea.Msg {
		gitSt := integrations.Status(baseCtx, workDir)
		var ghSt integrations.GitHubStatus
		var cfSt integrations.CloudflareStatus
		if includeRemote {
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				ghSt = integrations.GetGitHubStatus(baseCtx, workDir, gitSt.RepoSlug)
			}()
			go func() {
				defer wg.Done()
				cfSt = integrations.DetectCloudflare(baseCtx, workDir)
			}()
			wg.Wait()
		} else {
			ghSt = integrations.GitHubStatus{Repo: gitSt.RepoSlug}
			cfSt = integrations.DetectCloudflare(baseCtx, workDir)
		}
		return gitStatusMsg{
			git:           gitSt,
			gh:            ghSt,
			cf:            cfSt,
			includeRemote: includeRemote,
		}
	}
}

func (m *Model) fetchGitDiffCmd(path string, staged bool) tea.Cmd {
	workDir := strings.TrimSpace(m.opts.WorkDir)
	if workDir == "" {
		return nil
	}
	baseCtx := m.baseCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	return func() tea.Msg {
		diff, err := integrations.Diff(baseCtx, workDir, path, staged)
		return gitDiffMsg{path: path, diff: diff, err: err}
	}
}

func (m *Model) openGitModal(selectPath string, focusCommit bool) tea.Cmd {
	m.activeModal = modalGit
	m.gitModal.commitFocus = focusCommit
	m.gitModal.prFocus = false
	m.gitModal.diffScroll = 0
	m.gitModal.feedback = ""
	m.gitModal.isErr = false

	if selectPath != "" {
		for i, f := range m.gitStatus.Files {
			if f.Path == selectPath {
				m.gitModal.fileIndex = i
				break
			}
		}
	}
	m.clampGitFileIndex()
	m.dirty = true

	cmds := []tea.Cmd{m.fetchGitStatusCmd(true)}
	if cmd := m.loadSelectedGitDiff(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

func (m *Model) clampGitFileIndex() {
	n := len(m.gitStatus.Files)
	if n == 0 {
		m.gitModal.fileIndex = 0
		return
	}
	if m.gitModal.fileIndex < 0 {
		m.gitModal.fileIndex = 0
	}
	if m.gitModal.fileIndex >= n {
		m.gitModal.fileIndex = n - 1
	}
}

func (m *Model) loadSelectedGitDiff() tea.Cmd {
	if len(m.gitStatus.Files) == 0 {
		m.gitModal.diffPath = ""
		m.gitModal.diffLines = nil
		m.gitModal.diffLoading = false
		return nil
	}
	m.clampGitFileIndex()
	f := m.gitStatus.Files[m.gitModal.fileIndex]
	m.gitModal.diffPath = f.Path
	m.gitModal.diffLoading = true
	m.gitModal.diffScroll = 0
	return m.fetchGitDiffCmd(f.Path, f.Staged)
}

func (m *Model) handleGitStatusMsg(msg gitStatusMsg) tea.Cmd {
	m.gitStatus = msg.git
	if msg.git.Branch != "" {
		m.opts.GitBranch = msg.git.Branch
	}
	if msg.includeRemote || m.ghStatus.Repo == "" {
		m.ghStatus = msg.gh
	}
	m.cfStatus = msg.cf
	m.dirty = true

	if m.activeModal == modalGit {
		m.clampGitFileIndex()
		if len(m.gitStatus.Files) == 0 {
			m.gitModal.diffPath = ""
			m.gitModal.diffLines = nil
			m.gitModal.diffLoading = false
		} else if m.gitModal.diffPath == "" {
			return m.loadSelectedGitDiff()
		}
	}
	return nil
}

func (m *Model) handleGitDiffMsg(msg gitDiffMsg) {
	m.gitModal.diffLoading = false
	if msg.err != nil {
		m.gitModal.diffLines = []string{"Error loading diff: " + msg.err.Error()}
	} else if strings.TrimSpace(msg.diff) == "" {
		m.gitModal.diffLines = []string{"(no diff lines)"}
	} else {
		m.gitModal.diffLines = strings.Split(strings.TrimRight(msg.diff, "\n"), "\n")
	}
	m.gitModal.diffScroll = 0
	m.dirty = true
}

func (m *Model) handleGitActionMsg(msg gitActionMsg) tea.Cmd {
	m.gitModal.busy = false
	if msg.err != nil {
		m.gitModal.feedback = msg.err.Error()
		m.gitModal.isErr = true
		if m.activeModal != modalGit {
			m.addError(msg.text + ": " + msg.err.Error())
		}
	} else {
		m.gitModal.feedback = msg.text
		m.gitModal.isErr = false
		if strings.TrimSpace(msg.text) != "" {
			m.addInfo(msg.text)
		}
	}
	m.dirty = true
	if msg.refreshGit {
		return m.fetchGitStatusCmd(true)
	}
	return nil
}

func (m *Model) handleGitAIMsgResult(msg gitAIMsgResult) {
	m.gitModal.busy = false
	if msg.err != nil {
		m.gitModal.feedback = "AI draft failed: " + msg.err.Error()
		m.gitModal.isErr = true
	} else if strings.TrimSpace(msg.message) != "" {
		m.gitModal.commitInput = []rune(msg.message)
		m.gitModal.commitFocus = true
		m.gitModal.feedback = "AI drafted commit message · Enter commit · Ctrl+P commit & push"
		m.gitModal.isErr = false
	}
	m.dirty = true
}

func (m *Model) handleGitModalKey(msg tea.KeyMsg) tea.Cmd {
	// PR title input mode.
	if m.gitModal.prFocus {
		switch msg.Type {
		case tea.KeyEsc:
			m.gitModal.prFocus = false
			m.dirty = true
			return nil
		case tea.KeyEnter:
			title := strings.TrimSpace(string(m.gitModal.prInput))
			if title == "" {
				m.gitModal.feedback = "Enter a PR title first."
				m.gitModal.isErr = true
				m.dirty = true
				return nil
			}
			m.gitModal.prFocus = false
			m.gitModal.prInput = nil
			m.gitModal.busy = true
			m.gitModal.feedback = "Creating GitHub PR…"
			m.gitModal.isErr = false
			m.dirty = true
			workDir := m.opts.WorkDir
			return func() tea.Msg {
				url, err := integrations.CreatePR(context.Background(), workDir, title, "", "", false)
				return gitActionMsg{text: "Created PR: " + url, err: err, refreshGit: true}
			}
		case tea.KeyBackspace:
			if len(m.gitModal.prInput) > 0 {
				m.gitModal.prInput = m.gitModal.prInput[:len(m.gitModal.prInput)-1]
				m.dirty = true
			}
			return nil
		case tea.KeyRunes, tea.KeySpace:
			m.gitModal.prInput = append(m.gitModal.prInput, msg.Runes...)
			m.dirty = true
			return nil
		}
		return nil
	}

	// Commit message input mode.
	if m.gitModal.commitFocus {
		switch msg.Type {
		case tea.KeyEsc:
			m.gitModal.commitFocus = false
			m.dirty = true
			return nil
		case tea.KeyCtrlG:
			return m.draftGitCommitMsgCmd()
		case tea.KeyCtrlP:
			return m.runGitCommitCmd(true)
		case tea.KeyEnter:
			return m.runGitCommitCmd(false)
		case tea.KeyBackspace:
			if len(m.gitModal.commitInput) > 0 {
				m.gitModal.commitInput = m.gitModal.commitInput[:len(m.gitModal.commitInput)-1]
				m.dirty = true
			}
			return nil
		case tea.KeyRunes, tea.KeySpace:
			m.gitModal.commitInput = append(m.gitModal.commitInput, msg.Runes...)
			m.dirty = true
			return nil
		}
		return nil
	}

	// File list + diff navigation mode.
	switch msg.Type {
	case tea.KeyEsc:
		m.activeModal = modalNone
		m.dirty = true
		return nil
	case tea.KeyUp:
		if m.gitModal.fileIndex > 0 {
			m.gitModal.fileIndex--
			m.dirty = true
			return m.loadSelectedGitDiff()
		}
		return nil
	case tea.KeyDown:
		if m.gitModal.fileIndex < len(m.gitStatus.Files)-1 {
			m.gitModal.fileIndex++
			m.dirty = true
			return m.loadSelectedGitDiff()
		}
		return nil
	case tea.KeyPgUp:
		m.gitModal.diffScroll = maxInt(0, m.gitModal.diffScroll-10)
		m.dirty = true
		return nil
	case tea.KeyPgDown:
		m.gitModal.diffScroll = minInt(maxInt(0, len(m.gitModal.diffLines)-1), m.gitModal.diffScroll+10)
		m.dirty = true
		return nil
	case tea.KeyCtrlG:
		return m.draftGitCommitMsgCmd()
	case tea.KeyCtrlP:
		return m.runGitCommitCmd(true)
	case tea.KeySpace:
		return m.toggleSelectedFileStageCmd()
	}

	switch msg.String() {
	case "q":
		m.activeModal = modalNone
		m.dirty = true
		return nil
	case "j":
		m.gitModal.diffScroll = minInt(maxInt(0, len(m.gitModal.diffLines)-1), m.gitModal.diffScroll+3)
		m.dirty = true
		return nil
	case "k":
		m.gitModal.diffScroll = maxInt(0, m.gitModal.diffScroll-3)
		m.dirty = true
		return nil
	case "c":
		m.gitModal.commitFocus = true
		m.dirty = true
		return nil
	case "a":
		return m.stageAllCmd(true)
	case "u":
		return m.stageAllCmd(false)
	case "r":
		m.gitModal.feedback = "Refreshing…"
		m.gitModal.isErr = false
		m.dirty = true
		return m.fetchGitStatusCmd(true)
	case "p":
		return m.runGitPushCmd()
	case "P":
		if m.ghStatus.Repo != "" {
			m.gitModal.prFocus = true
			m.gitModal.prInput = nil
			m.dirty = true
		} else {
			m.gitModal.feedback = "No GitHub remote detected."
			m.gitModal.isErr = true
			m.dirty = true
		}
		return nil
	case "D":
		if m.cfStatus.Detected {
			return m.runCloudflareDeployCmd()
		}
		m.gitModal.feedback = "No Cloudflare project detected."
		m.gitModal.isErr = true
		m.dirty = true
		return nil
	}
	return nil
}

func (m *Model) toggleSelectedFileStageCmd() tea.Cmd {
	if len(m.gitStatus.Files) == 0 {
		return nil
	}
	m.clampGitFileIndex()
	f := m.gitStatus.Files[m.gitModal.fileIndex]
	workDir := m.opts.WorkDir
	return func() tea.Msg {
		var err error
		if f.Staged {
			err = integrations.Unstage(context.Background(), workDir, f.Path)
		} else {
			err = integrations.Stage(context.Background(), workDir, f.Path)
		}
		label := "Staged " + f.Path
		if f.Staged {
			label = "Unstaged " + f.Path
		}
		return gitActionMsg{text: label, err: err, refreshGit: true}
	}
}

func (m *Model) stageAllCmd(stage bool) tea.Cmd {
	workDir := m.opts.WorkDir
	return func() tea.Msg {
		var err error
		label := "Staged all changes"
		if stage {
			err = integrations.Stage(context.Background(), workDir)
		} else {
			err = integrations.Unstage(context.Background(), workDir)
			label = "Unstaged all changes"
		}
		return gitActionMsg{text: label, err: err, refreshGit: true}
	}
}

func (m *Model) draftGitCommitMsgCmd() tea.Cmd {
	if m.gitModal.busy {
		return nil
	}
	m.gitModal.busy = true
	m.gitModal.feedback = "Drafting commit message with AI…"
	m.gitModal.isErr = false
	m.dirty = true
	workDir := m.opts.WorkDir
	var prov provider.Provider
	var model string
	if m.opts.Agent != nil {
		prov = m.opts.Agent.Provider
	}
	if m.opts.Config != nil {
		model = m.opts.Config.Model
	}
	return func() tea.Msg {
		msg, err := integrations.DraftCommitMessage(context.Background(), workDir, prov, model)
		return gitAIMsgResult{message: msg, err: err}
	}
}

func (m *Model) runGitCommitCmd(push bool) tea.Cmd {
	msg := strings.TrimSpace(string(m.gitModal.commitInput))
	if msg == "" {
		m.gitModal.feedback = "Enter a commit message first (or press Ctrl+G for AI draft)."
		m.gitModal.isErr = true
		m.gitModal.commitFocus = true
		m.dirty = true
		return nil
	}
	stageAll := m.gitStatus.StagedCount == 0 && len(m.gitStatus.Files) > 0
	m.gitModal.busy = true
	if push {
		m.gitModal.feedback = "Committing & pushing…"
	} else {
		m.gitModal.feedback = "Committing…"
	}
	m.gitModal.isErr = false
	m.gitModal.commitInput = nil
	m.gitModal.commitFocus = false
	m.dirty = true

	workDir := m.opts.WorkDir
	return func() tea.Msg {
		var (
			res integrations.CommitResult
			err error
		)
		if push {
			res, err = integrations.CommitAndPush(context.Background(), workDir, msg, stageAll)
		} else {
			res, err = integrations.Commit(context.Background(), workDir, msg, stageAll)
		}
		if err != nil {
			return gitActionMsg{text: "Git commit", err: err, refreshGit: true}
		}
		summary := fmt.Sprintf("Committed %s: %s", res.SHA, msg)
		if res.Pushed {
			summary = fmt.Sprintf("Committed & pushed %s: %s", res.SHA, msg)
		}
		return gitActionMsg{text: summary, refreshGit: true}
	}
}

func (m *Model) runGitPushCmd() tea.Cmd {
	m.gitModal.busy = true
	m.gitModal.feedback = "Pushing…"
	m.gitModal.isErr = false
	m.dirty = true
	workDir := m.opts.WorkDir
	return func() tea.Msg {
		out, err := integrations.Push(context.Background(), workDir)
		return gitActionMsg{text: "Git push: " + out, err: err, refreshGit: true}
	}
}

func (m *Model) runCloudflareDeployCmd() tea.Cmd {
	m.gitModal.busy = true
	m.gitModal.feedback = "Deploying to Cloudflare…"
	m.gitModal.isErr = false
	m.dirty = true
	workDir := m.opts.WorkDir
	return func() tea.Msg {
		out, err := integrations.DeployCloudflare(context.Background(), workDir)
		return gitActionMsg{text: "Cloudflare deploy:\n" + out, err: err, refreshGit: true}
	}
}

func (m *Model) gitModalView() string {
	boxW := clampInt(m.width-8, 56, 108)
	innerW := maxInt(40, boxW-6)

	title := "Git · Changes & Commit"
	if m.gitStatus.Branch != "" {
		title = fmt.Sprintf("Git · %s", m.gitStatus.Branch)
		if m.gitStatus.Ahead > 0 {
			title += fmt.Sprintf(" ↑%d", m.gitStatus.Ahead)
		}
		if m.gitStatus.Behind > 0 {
			title += fmt.Sprintf(" ↓%d", m.gitStatus.Behind)
		}
	}

	var lines []string

	// Integration status bar (GitHub + Cloudflare).
	var badges []string
	if m.gitStatus.TotalAdded > 0 || m.gitStatus.TotalRemoved > 0 {
		badges = append(badges, m.st.paint(m.st.success, fmt.Sprintf("+%d", m.gitStatus.TotalAdded))+" "+
			m.st.paint(m.st.error, fmt.Sprintf("-%d", m.gitStatus.TotalRemoved)))
	}
	if m.ghStatus.Repo != "" {
		ghBadge := "GH: " + m.ghStatus.Repo
		if m.ghStatus.PR != nil {
			ghBadge += fmt.Sprintf(" (PR #%d %s)", m.ghStatus.PR.Number, m.ghStatus.PR.ChecksState)
		}
		badges = append(badges, m.st.paint(m.st.accent, ghBadge))
	}
	if m.cfStatus.Detected {
		cfBadge := fmt.Sprintf("CF %s: %s", m.cfStatus.Kind, emptyFallback(m.cfStatus.ProjectName, m.cfStatus.ConfigFile))
		badges = append(badges, m.st.paint(m.st.warn, cfBadge))
	}
	if len(badges) > 0 {
		lines = append(lines, truncateAnsi(strings.Join(badges, "  ·  "), innerW))
	}

	// File list (up to 7 rows).
	if !m.gitStatus.IsRepo {
		lines = append(lines, m.st.paint(m.st.dim, "Not inside a git repository."))
	} else if len(m.gitStatus.Files) == 0 {
		lines = append(lines, m.st.paint(m.st.success, "Working tree clean."))
	} else {
		limit := minInt(7, len(m.gitStatus.Files))
		start, end := windowRange(len(m.gitStatus.Files), m.gitModal.fileIndex, limit)
		for i := start; i < end; i++ {
			f := m.gitStatus.Files[i]
			mark := "[ ]"
			if f.Staged {
				mark = "[✓]"
			}
			stats := fmt.Sprintf("+%d -%d", f.Added, f.Removed)
			row := fmt.Sprintf("%s %s %s", mark, f.Status, f.Path)
			avail := maxInt(12, innerW-len(stats)-2)
			row = truncateCell(row, avail)
			if pad := innerW - displayWidth(row) - len(stats); pad > 0 {
				row += strings.Repeat(" ", pad)
			}
			row += stats
			if i == m.gitModal.fileIndex && !m.gitModal.commitFocus && !m.gitModal.prFocus {
				lines = append(lines, m.st.paint(m.st.selected, truncateCell(row, innerW)))
			} else {
				lines = append(lines, truncateCell(row, innerW))
			}
		}
	}

	// Diff preview.
	lines = append(lines, m.st.paint(m.st.border, strings.Repeat("─", innerW)))
	diffBudget := clampInt(m.height-22, 6, 16)
	if m.gitModal.diffLoading {
		lines = append(lines, m.st.paint(m.st.dim, "Loading diff…"))
	} else if len(m.gitModal.diffLines) > 0 {
		end := minInt(len(m.gitModal.diffLines), m.gitModal.diffScroll+diffBudget)
		for _, dl := range m.gitModal.diffLines[m.gitModal.diffScroll:end] {
			trimmed := truncateCell(dl, innerW)
			switch {
			case strings.HasPrefix(dl, "+++") || strings.HasPrefix(dl, "---") || strings.HasPrefix(dl, "diff --git"):
				lines = append(lines, m.st.paint(m.st.dim, trimmed))
			case strings.HasPrefix(dl, "@@"):
				lines = append(lines, m.st.paint(m.st.accent, trimmed))
			case strings.HasPrefix(dl, "+"):
				lines = append(lines, m.st.paint(m.st.success, trimmed))
			case strings.HasPrefix(dl, "-"):
				lines = append(lines, m.st.paint(m.st.error, trimmed))
			default:
				lines = append(lines, trimmed)
			}
		}
	}

	// Commit / PR input bar.
	lines = append(lines, m.st.paint(m.st.border, strings.Repeat("─", innerW)))
	if m.gitModal.prFocus {
		lines = append(lines, m.st.paint(m.st.accent, "PR title > ")+string(m.gitModal.prInput)+"_")
	} else {
		prefix := "Commit > "
		if m.gitModal.commitFocus {
			lines = append(lines, m.st.paint(m.st.success, prefix)+string(m.gitModal.commitInput)+"_")
		} else {
			val := string(m.gitModal.commitInput)
			if val == "" {
				val = "(press c to type message, or Ctrl+G for AI draft)"
			}
			lines = append(lines, m.st.paint(m.st.dim, prefix+truncateCell(val, innerW-len(prefix))))
		}
	}

	if m.gitModal.feedback != "" {
		st := m.st.success
		if m.gitModal.isErr {
			st = m.st.error
		} else if m.gitModal.busy {
			st = m.st.warn
		}
		lines = append(lines, m.st.paint(st, truncateCell(m.gitModal.feedback, innerW)))
	}

	footer := "↑/↓ file · Space stage · j/k diff · c commit · Ctrl+G AI msg · Ctrl+P commit&push · p push · Esc close"
	if m.ghStatus.Repo != "" {
		footer += " · P PR"
	}
	if m.cfStatus.Detected {
		footer += " · D CF deploy"
	}
	return m.modalFrame(title, strings.Join(lines, "\n"), footer)
}

// Slash commands: /git, /gh, /cf

func (m *Model) cmdGit(args string) tea.Cmd {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 {
		return m.openGitModal("", false)
	}
	sub := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), fields[0]))
	workDir := m.opts.WorkDir

	switch sub {
	case "status":
		st := integrations.Status(context.Background(), workDir)
		m.gitStatus = st
		if !st.IsRepo {
			m.addInfo("Not inside a git repository.")
			return nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Branch: %s", st.Branch)
		if st.Ahead > 0 {
			fmt.Fprintf(&b, " ↑%d", st.Ahead)
		}
		if st.Behind > 0 {
			fmt.Fprintf(&b, " ↓%d", st.Behind)
		}
		if len(st.Files) == 0 {
			b.WriteString("\nWorking tree clean.")
		} else {
			for _, f := range st.Files {
				fmt.Fprintf(&b, "\n  %s %s (+%d -%d)", f.Status, f.Path, f.Added, f.Removed)
			}
		}
		m.addInfo(b.String())
		return nil
	case "diff":
		return m.openGitModal(rest, false)
	case "push":
		return m.runGitPushCmd()
	case "commit", "cp":
		if rest == "" {
			return m.openGitModal("", true)
		}
		m.gitModal.commitInput = []rune(rest)
		return m.runGitCommitCmd(sub == "cp")
	default:
		return m.openGitModal("", false)
	}
}

func (m *Model) cmdGitHub(args string) tea.Cmd {
	fields := strings.Fields(strings.TrimSpace(args))
	sub := ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
	}
	workDir := m.opts.WorkDir
	return func() tea.Msg {
		ctx := context.Background()
		switch sub {
		case "prs", "pr":
			if sub == "pr" && len(fields) > 1 {
				title := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), fields[0]))
				url, err := integrations.CreatePR(ctx, workDir, title, "", "", false)
				return gitActionMsg{text: "Created PR: " + url, err: err, refreshGit: true}
			}
			prs, err := integrations.ListPRs(ctx, workDir, 10)
			if err != nil {
				return gitActionMsg{text: "GitHub PRs", err: err}
			}
			if len(prs) == 0 {
				return gitActionMsg{text: "No open pull requests."}
			}
			var lines []string
			for _, p := range prs {
				lines = append(lines, fmt.Sprintf("#%d %s (%s)", p.Number, p.Title, p.URL))
			}
			return gitActionMsg{text: "Open PRs:\n" + strings.Join(lines, "\n")}
		case "issues", "issue":
			issues, err := integrations.ListIssues(ctx, workDir, 10)
			if err != nil {
				return gitActionMsg{text: "GitHub issues", err: err}
			}
			if len(issues) == 0 {
				return gitActionMsg{text: "No open issues."}
			}
			var lines []string
			for _, is := range issues {
				lines = append(lines, fmt.Sprintf("#%d %s (%s)", is.Number, is.Title, is.URL))
			}
			return gitActionMsg{text: "Open issues:\n" + strings.Join(lines, "\n")}
		case "releases", "release":
			rels, err := integrations.ListReleases(ctx, workDir, 10)
			if err != nil {
				return gitActionMsg{text: "GitHub releases", err: err}
			}
			if len(rels) == 0 {
				return gitActionMsg{text: "No releases found."}
			}
			var lines []string
			for _, r := range rels {
				tag := r.TagName
				if r.IsLatest {
					tag += " [latest]"
				}
				lines = append(lines, tag)
			}
			return gitActionMsg{text: "Releases:\n" + strings.Join(lines, "\n")}
		default:
			gitSt := integrations.Status(ctx, workDir)
			ghSt := integrations.GetGitHubStatus(ctx, workDir, gitSt.RepoSlug)
			return gitActionMsg{text: integrations.FormatGitHubSummary(ghSt)}
		}
	}
}

func (m *Model) cmdCloudflare(args string) tea.Cmd {
	sub := strings.ToLower(strings.TrimSpace(args))
	if sub == "deploy" {
		return m.runCloudflareDeployCmd()
	}
	workDir := m.opts.WorkDir
	return func() tea.Msg {
		st := integrations.DetectCloudflare(context.Background(), workDir)
		return gitActionMsg{text: integrations.FormatCloudflareSummary(st)}
	}
}
