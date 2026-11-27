package serve

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	tern "github.com/tern-agent/tern"
	"github.com/tern-agent/tern/internal/config"
	terncontext "github.com/tern-agent/tern/internal/context"
	"github.com/tern-agent/tern/internal/ext"
	"github.com/tern-agent/tern/internal/provider"
)

type providerWire struct {
	Name         string `json:"name"`
	DisplayName  string `json:"display_name"`
	BaseURL      string `json:"base_url"`
	DefaultModel string `json:"default_model"`
	EnvVar       string `json:"env_var,omitempty"`
	Local        bool   `json:"local"`
	FreeTier     bool   `json:"free_tier,omitempty"`
	DataNotice   string `json:"data_notice,omitempty"`
	Recommended  bool   `json:"recommended"`
	HasKey       bool   `json:"has_key"`
	Active       bool   `json:"active"`
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	active := "deepseek"
	if s.Config != nil && strings.TrimSpace(s.Config.Provider) != "" {
		active = strings.ToLower(strings.TrimSpace(s.Config.Provider))
	}
	presets := config.BuiltinProviders()
	out := make([]providerWire, 0, len(presets))
	for _, p := range presets {
		local := !p.RequiresKey
		hasKey := !p.RequiresKey
		if p.RequiresKey && s.GlobalConfigDir != "" {
			if k, err := config.LoadAPIKeyForProvider(s.GlobalConfigDir, p.Name); err == nil && strings.TrimSpace(k) != "" {
				hasKey = true
			}
		}
		out = append(out, providerWire{
			Name:         p.Name,
			DisplayName:  p.DisplayName,
			BaseURL:      p.BaseURL,
			DefaultModel: p.DefaultModel,
			EnvVar:       p.EnvVar,
			Local:        local,
			FreeTier:     p.FreeTier,
			DataNotice:   p.DataNotice,
			Recommended:  p.Name == "deepseek",
			HasKey:       hasKey,
			Active:       strings.EqualFold(active, p.Name),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active":    active,
		"providers": out,
	})
}

type setProviderRequest struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Model    string `json:"model,omitempty"`
}

func (s *Server) handleSetProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		writeError(w, http.StatusConflict, "cannot switch provider while a turn is running")
		return
	}
	var req setProviderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	provName := strings.ToLower(strings.TrimSpace(req.Provider))
	if provName == "" {
		writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	baseURL := strings.TrimSpace(req.BaseURL)
	model := strings.TrimSpace(req.Model)
	apiKey := config.SanitizeAPIKey(req.APIKey)

	if preset, ok := config.LookupProvider(provName); ok {
		if baseURL == "" {
			baseURL = preset.BaseURL
		}
		if model == "" {
			model = preset.DefaultModel
		}
		if !preset.RequiresKey {
			apiKey = "local"
		} else if apiKey == "" && s.GlobalConfigDir != "" {
			if saved, err := config.LoadAPIKeyForProvider(s.GlobalConfigDir, provName); err == nil && saved != "" {
				apiKey = saved
			}
		}
		if preset.RequiresKey && apiKey == "" {
			writeError(w, http.StatusBadRequest, "API key is required for "+preset.DisplayName)
			return
		}
	}

	if s.OnConfigureProvider != nil {
		if err := s.OnConfigureProvider(provName, baseURL, apiKey, model); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		if s.Config != nil {
			s.Config.Provider = provName
			if baseURL != "" {
				s.Config.BaseURL = baseURL
			}
			if model != "" {
				s.Config.Model = model
			}
			if apiKey != "" {
				s.Config.APIKey = apiKey
			}
		}
		if s.Agent != nil {
			var extra map[string]string
			if preset, ok := config.LookupProvider(provName); ok && len(preset.ExtraHeaders) > 0 {
				extra = preset.ExtraHeaders
			}
			s.Agent.Provider = provider.NewOpenAICompatClient(provName, baseURL, apiKey, extra)
			if model != "" {
				s.Agent.SetModel(model)
			}
		}
	}

	s.mu.Lock()
	if model != "" {
		s.model = model
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.status())
}

type modelWire struct {
	ID             string  `json:"id"`
	ContextLength  int     `json:"context_length"`
	PromptPerM     float64 `json:"prompt_per_m"`
	OutputPerM     float64 `json:"output_per_m"`
	HasPricing     bool    `json:"has_pricing"`
	IsFree         bool    `json:"is_free"`
	SupportsVision bool    `json:"supports_vision"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	provName := "deepseek"
	current := ""
	if s.Config != nil {
		if strings.TrimSpace(s.Config.Provider) != "" {
			provName = strings.ToLower(strings.TrimSpace(s.Config.Provider))
		}
		current = strings.TrimSpace(s.Config.Model)
	}

	if provName == "deepseek" && s.Config != nil {
		names := make([]string, 0, len(s.Config.Pricing))
		for name := range s.Config.Pricing {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]modelWire, 0, len(names))
		for _, name := range names {
			p := s.Config.Pricing[name]
			out = append(out, modelWire{
				ID:             name,
				ContextLength:  p.ContextWindow,
				PromptPerM:     p.OffPeakCacheMissPerM,
				OutputPerM:     p.OffPeakOutputPerM,
				HasPricing:     true,
				SupportsVision: p.SupportsVision,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"provider": provName,
			"models":   out,
		})
		return
	}

	var remote []provider.RemoteModel
	if s.Agent != nil && s.Agent.Provider != nil {
		if lister, ok := s.Agent.Provider.(provider.ModelLister); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			fetched, err := lister.ListModels(ctx)
			cancel()
			if err == nil && len(fetched) > 0 {
				remote = fetched
			}
		}
	}
	if len(remote) == 0 {
		remote = provider.FallbackModelsForProvider(provName)
	}
	seen := make(map[string]struct{}, len(remote)+1)
	out := make([]modelWire, 0, len(remote)+1)
	for _, rm := range remote {
		if _, dup := seen[rm.ID]; dup {
			continue
		}
		seen[rm.ID] = struct{}{}
		out = append(out, modelWire{
			ID:             rm.ID,
			ContextLength:  rm.ContextLength,
			PromptPerM:     rm.PromptPerM,
			OutputPerM:     rm.OutputPerM,
			HasPricing:     rm.HasPricing,
			IsFree:         rm.IsFree,
			SupportsVision: rm.SupportsVision,
		})
	}
	if current != "" {
		if _, ok := seen[current]; !ok {
			caps := provider.DefaultModelCapabilities(current)
			out = append(out, modelWire{
				ID:             current,
				ContextLength:  caps.ContextWindow,
				IsFree:         strings.HasSuffix(strings.ToLower(current), ":free"),
				SupportsVision: caps.SupportsVision,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": provName,
		"models":   out,
	})
}

func (s *Server) handleChangelog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":   s.Version,
		"changelog": tern.Changelog,
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	cfg := s.Config
	if cfg == nil {
		cfg = config.Default()
	}
	keyStatus := "[not set]"
	if strings.TrimSpace(cfg.APIKey) != "" {
		keyStatus = "[set]"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":         cfg.Provider,
		"model":            cfg.Model,
		"base_url":         cfg.BaseURL,
		"api_key":          keyStatus,
		"reasoning_effort": cfg.ReasoningEffort,
		"permission_mode":  s.permissionMode(),
		"max_steps":        cfg.MaxSteps,
		"max_agents":       cfg.MaxAgents,
		"work_dir":         s.WorkDir,
	})
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	skills, _ := terncontext.ListSkills(s.WorkDir, s.GlobalConfigDir)
	writeJSON(w, http.StatusOK, map[string]any{"skills": skills})
}

func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	plugins, warnings := ext.ListAllExtensions(s.WorkDir, s.GlobalConfigDir)
	type pluginWire struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
		Enabled     bool   `json:"enabled"`
		Dir         string `json:"dir"`
	}
	out := make([]pluginWire, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, pluginWire{
			Name:        p.Manifest.Name,
			Version:     p.Manifest.Version,
			Description: p.Manifest.Description,
			Enabled:     p.Manifest.IsEnabled(),
			Dir:         p.Dir,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": out, "warnings": warnings})
}

func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.Checkpoints == nil {
		writeError(w, http.StatusServiceUnavailable, "checkpoints are not available")
		return
	}
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		writeError(w, http.StatusConflict, "cannot undo while a turn is running")
		return
	}
	res, err := s.Checkpoints.Undo()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": res.Restored, "deleted": res.Deleted})
}

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.Agent == nil || s.Agent.Provider == nil {
		writeError(w, http.StatusServiceUnavailable, "provider is not available")
		return
	}
	checker, ok := s.Agent.Provider.(provider.BalanceChecker)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"supported": false,
			"note":      "Balance checking is not supported for this provider.",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	bal, err := checker.GetBalance(ctx)
	if err != nil {
		if errors.Is(err, provider.ErrBalanceUnsupported) {
			writeJSON(w, http.StatusOK, map[string]any{
				"supported": false,
				"note":      "Balance checking is not supported for this provider.",
			})
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"supported": true,
		"compact":   bal.FormatCompact(),
		"balance":   bal,
	})
}

type askAnswerRequest struct {
	ID     string `json:"id"`
	Answer string `json:"answer"`
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req askAnswerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	s.mu.Lock()
	pending := s.asks[strings.TrimSpace(req.ID)]
	if pending != nil {
		delete(s.asks, pending.wire.ID)
		if s.pendingAsk != nil && s.pendingAsk.ID == pending.wire.ID {
			s.pendingAsk = nil
		}
	}
	s.mu.Unlock()
	if pending == nil {
		writeError(w, http.StatusNotFound, "no pending question with that id")
		return
	}
	ans := strings.TrimSpace(req.Answer)
	select {
	case pending.ch <- ans:
	default:
	}
	s.broadcast("ask_resolved", map[string]any{"id": pending.wire.ID, "answer": ans})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"sessions": []any{}})
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := s.Store.List("")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		curID := ""
		if s.Session != nil {
			curID = s.Session.ID()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"current":  curID,
			"sessions": list,
		})
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeError(w, http.StatusBadRequest, "id query parameter is required")
			return
		}
		if err := s.Store.Delete(id); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

type renameSessionRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.Session == nil {
		writeError(w, http.StatusServiceUnavailable, "no active session")
		return
	}
	var req renameSessionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.Session.Rename(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

type subagentMessageWire struct {
	Role      string `json:"role"`
	Content   string `json:"content,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
	ToolCalls []struct {
		Name string `json:"name"`
		Args string `json:"args,omitempty"`
	} `json:"tool_calls,omitempty"`
}

func (s *Server) handleSubagentTranscript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id query parameter is required")
		return
	}

	var status any
	if s.Subagents != nil {
		for _, st := range s.Subagents.List() {
			if st.ID == id {
				status = st
				break
			}
		}
	}

	var msgs []subagentMessageWire
	if s.Session != nil {
		if children, err := s.Session.ListChildren(); err == nil {
			parentID := s.Session.ID()
			for _, child := range children {
				if child == nil {
					continue
				}
				cid := child.ID()
				if cid == parentID+".sub."+id ||
					strings.HasSuffix(cid, ".sub."+id) ||
					strings.HasSuffix(cid, "."+id) ||
					strings.HasSuffix(cid, id) {
					for _, m := range child.ReplayMessages() {
						item := subagentMessageWire{
							Role:      string(m.Role),
							Content:   m.Content,
							Reasoning: m.ReasoningContent,
						}
						for _, tc := range m.ToolCalls {
							item.ToolCalls = append(item.ToolCalls, struct {
								Name string `json:"name"`
								Args string `json:"args,omitempty"`
							}{Name: tc.Function.Name, Args: tc.Function.Arguments})
						}
						msgs = append(msgs, item)
					}
					break
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":       id,
		"status":   status,
		"messages": msgs,
	})
}

func (s *Server) handleSubagentCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	cancelled := false
	if s.Subagents != nil {
		cancelled = s.Subagents.CancelOne(strings.TrimSpace(req.ID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": cancelled})
}
