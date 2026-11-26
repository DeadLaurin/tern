package serve

import (
	"context"
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
		local := p.EnvVar == ""
		hasKey := local
		if !local && s.GlobalConfigDir != "" {
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
		if preset.EnvVar == "" {
			apiKey = "local"
		} else if apiKey == "" && s.GlobalConfigDir != "" {
			if saved, err := config.LoadAPIKeyForProvider(s.GlobalConfigDir, provName); err == nil && saved != "" {
				apiKey = saved
			}
		}
		if preset.EnvVar != "" && apiKey == "" {
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
