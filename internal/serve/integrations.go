package serve

import (
	"net/http"
	"strings"
	"sync"

	"github.com/tern-agent/tern/internal/integrations"
	"github.com/tern-agent/tern/internal/provider"
)

func (s *Server) handleGitStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx := r.Context()
	workDir := s.WorkDir
	includeRemote := r.URL.Query().Get("remote") != "0"

	gitSt := integrations.Status(ctx, workDir)
	var ghSt integrations.GitHubStatus
	var cfSt integrations.CloudflareStatus

	var wg sync.WaitGroup
	if includeRemote {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ghSt = integrations.GetGitHubStatus(ctx, workDir, gitSt.RepoSlug)
		}()
		go func() {
			defer wg.Done()
			cfSt = integrations.DetectCloudflare(ctx, workDir)
		}()
		wg.Wait()
	} else {
		ghSt = integrations.GitHubStatus{Repo: gitSt.RepoSlug}
		cfSt = integrations.DetectCloudflare(ctx, workDir)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"git":        gitSt,
		"github":     ghSt,
		"cloudflare": cfSt,
	})
}

func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	path := r.URL.Query().Get("path")
	staged := r.URL.Query().Get("staged") == "1" || r.URL.Query().Get("staged") == "true"
	diff, err := integrations.Diff(r.Context(), s.WorkDir, path, staged)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":   path,
		"staged": staged,
		"diff":   diff,
	})
}

func (s *Server) handleGitStage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Paths []string `json:"paths"`
		All   bool     `json:"all"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var err error
	if req.All || len(req.Paths) == 0 {
		err = integrations.Stage(r.Context(), s.WorkDir)
	} else {
		err = integrations.Stage(r.Context(), s.WorkDir, req.Paths...)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"git": integrations.Status(r.Context(), s.WorkDir),
	})
}

func (s *Server) handleGitUnstage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Paths []string `json:"paths"`
		All   bool     `json:"all"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var err error
	if req.All || len(req.Paths) == 0 {
		err = integrations.Unstage(r.Context(), s.WorkDir)
	} else {
		err = integrations.Unstage(r.Context(), s.WorkDir, req.Paths...)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"git": integrations.Status(r.Context(), s.WorkDir),
	})
}

func (s *Server) handleGitCommit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Message  string `json:"message"`
		StageAll bool   `json:"stage_all"`
		Push     bool   `json:"push"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// If nothing is staged yet, automatically stage all changes for convenience.
	stageAll := req.StageAll
	if !stageAll {
		st := integrations.Status(r.Context(), s.WorkDir)
		if st.StagedCount == 0 && len(st.Files) > 0 {
			stageAll = true
		}
	}

	var (
		res integrations.CommitResult
		err error
	)
	if req.Push {
		res, err = integrations.CommitAndPush(r.Context(), s.WorkDir, req.Message, stageAll)
	} else {
		res, err = integrations.Commit(r.Context(), s.WorkDir, req.Message, stageAll)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"result": res,
		"git":    integrations.Status(r.Context(), s.WorkDir),
	})
}

func (s *Server) handleGitPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	out, err := integrations.Push(r.Context(), s.WorkDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"output": out,
		"git":    integrations.Status(r.Context(), s.WorkDir),
	})
}

func (s *Server) handleGitCommitMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var prov provider.Provider
	var model string
	if s.Agent != nil {
		prov = s.Agent.Provider
	}
	s.mu.Lock()
	model = s.model
	s.mu.Unlock()
	if model == "" && s.Config != nil {
		model = s.Config.Model
	}
	msg, err := integrations.DraftCommitMessage(r.Context(), s.WorkDir, prov, model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message": msg,
	})
}

func (s *Server) handleGitHubPR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	url, err := integrations.CreatePR(r.Context(), s.WorkDir, req.Title, req.Body, req.Base, req.Draft)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url": url,
	})
}

func (s *Server) handleGitHubAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	action := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("action")))
	if action == "" && r.Method == http.MethodPost {
		var req struct {
			Action string `json:"action"`
		}
		_ = decodeJSON(r, &req)
		action = strings.ToLower(strings.TrimSpace(req.Action))
	}
	ctx := r.Context()
	switch action {
	case "prs", "pr":
		items, err := integrations.ListPRs(ctx, s.WorkDir, 12)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"prs": items})
	case "issues", "issue":
		items, err := integrations.ListIssues(ctx, s.WorkDir, 12)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"issues": items})
	case "releases", "release":
		items, err := integrations.ListReleases(ctx, s.WorkDir, 10)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"releases": items})
	default:
		gitSt := integrations.Status(ctx, s.WorkDir)
		ghSt := integrations.GetGitHubStatus(ctx, s.WorkDir, gitSt.RepoSlug)
		writeJSON(w, http.StatusOK, map[string]any{
			"github":  ghSt,
			"summary": integrations.FormatGitHubSummary(ghSt),
		})
	}
}

func (s *Server) handleCloudflareDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	out, err := integrations.DeployCloudflare(r.Context(), s.WorkDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"output":     out,
		"cloudflare": integrations.DetectCloudflare(r.Context(), s.WorkDir),
	})
}
