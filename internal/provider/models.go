package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RemoteModel describes one model advertised by a provider's /models endpoint.
type RemoteModel struct {
	ID             string
	Name           string
	ContextLength  int
	PromptPerM     float64 // USD per 1M prompt tokens
	OutputPerM     float64 // USD per 1M completion tokens
	HasPricing     bool
	IsFree         bool
	SupportsVision bool
}

// ModelLister is an optional interface implemented by providers that can list
// available models dynamically (OpenRouter, OpenAI, Groq, Ollama, etc.).
type ModelLister interface {
	ListModels(ctx context.Context) ([]RemoteModel, error)
}

func (c *DeepSeekClient) modelsEndpoint() string {
	base := c.BaseURL
	if base == "" {
		base = defaultDeepSeekBaseURL
	}
	return strings.TrimRight(base, "/") + "/models"
}

// ListModels fetches the available models from the provider's OpenAI-compatible
// GET /models endpoint (including OpenRouter pricing, context length, and free
// tier metadata when present).
func (c *DeepSeekClient) ListModels(ctx context.Context) ([]RemoteModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.modelsEndpoint(), nil)
	if err != nil {
		return nil, fmt.Errorf("provider: build models request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if key := strings.TrimSpace(c.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range c.ExtraHeaders {
		if strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			req.Header.Set(k, v)
		}
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 12 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provider: fetch models: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("provider: read models response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &APIError{
			Provider:   c.ProviderName,
			StatusCode: resp.StatusCode,
			Message:    strings.TrimSpace(string(body)),
		}
	}

	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
			Pricing       *struct {
				Prompt     json.RawMessage `json:"prompt"`
				Completion json.RawMessage `json:"completion"`
			} `json:"pricing"`
			Architecture *struct {
				Modality        string   `json:"modality"`
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("provider: decode models response: %w", err)
	}

	models := make([]RemoteModel, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}

		rm := RemoteModel{
			ID:            id,
			Name:          strings.TrimSpace(item.Name),
			ContextLength: item.ContextLength,
		}
		if rm.ContextLength <= 0 {
			rm.ContextLength = DefaultModelCapabilities(id).ContextWindow
		}

		// Vision detection from architecture metadata or model-family heuristics.
		if item.Architecture != nil {
			if strings.Contains(strings.ToLower(item.Architecture.Modality), "image") {
				rm.SupportsVision = true
			}
			for _, mod := range item.Architecture.InputModalities {
				if strings.EqualFold(strings.TrimSpace(mod), "image") {
					rm.SupportsVision = true
					break
				}
			}
		}
		if !rm.SupportsVision {
			rm.SupportsVision = DefaultModelCapabilities(id).SupportsVision
		}

		// OpenRouter and compatible APIs report per-token USD prices as strings
		// (e.g. "0.000003" or "0"). Convert to per-1M USD.
		if item.Pricing != nil {
			promptTok, okP := parseFlexibleFloat(item.Pricing.Prompt)
			compTok, okC := parseFlexibleFloat(item.Pricing.Completion)
			if okP || okC {
				rm.HasPricing = true
				rm.PromptPerM = promptTok * 1_000_000
				rm.OutputPerM = compTok * 1_000_000
				if promptTok == 0 && compTok == 0 {
					rm.IsFree = true
				}
			}
		}
		if strings.HasSuffix(strings.ToLower(id), ":free") {
			rm.IsFree = true
			rm.HasPricing = true
			rm.PromptPerM = 0
			rm.OutputPerM = 0
		}
		models = append(models, rm)
	}
	return models, nil
}

func parseFlexibleFloat(raw json.RawMessage) (float64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// FallbackModelsForProvider returns a curated starter list of models for a
// provider preset before or if GET /models is unavailable.
func FallbackModelsForProvider(providerName string) []RemoteModel {
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case "openrouter":
		return []RemoteModel{
			{ID: "deepseek/deepseek-r1:free", ContextLength: 163_840, HasPricing: true, IsFree: true},
			{ID: "deepseek/deepseek-chat-v3-0324:free", ContextLength: 163_840, HasPricing: true, IsFree: true},
			{ID: "qwen/qwen3-coder:free", ContextLength: 262_144, HasPricing: true, IsFree: true},
			{ID: "google/gemini-2.0-flash-exp:free", ContextLength: 1_048_576, HasPricing: true, IsFree: true, SupportsVision: true},
			{ID: "meta-llama/llama-3.3-70b-instruct:free", ContextLength: 131_072, HasPricing: true, IsFree: true},
			{ID: "anthropic/claude-sonnet-4", ContextLength: 200_000, PromptPerM: 3.0, OutputPerM: 15.0, HasPricing: true, SupportsVision: true},
			{ID: "openai/gpt-4.1", ContextLength: 1_047_576, PromptPerM: 2.0, OutputPerM: 8.0, HasPricing: true, SupportsVision: true},
			{ID: "google/gemini-2.5-pro", ContextLength: 1_048_576, PromptPerM: 1.25, OutputPerM: 10.0, HasPricing: true, SupportsVision: true},
		}
	case "openai":
		return []RemoteModel{
			{ID: "gpt-4.1", ContextLength: 1_047_576, SupportsVision: true},
			{ID: "gpt-4.1-mini", ContextLength: 1_047_576, SupportsVision: true},
			{ID: "o3", ContextLength: 200_000, SupportsVision: true},
			{ID: "o4-mini", ContextLength: 200_000, SupportsVision: true},
		}
	case "groq":
		return []RemoteModel{
			{ID: "llama-3.3-70b-versatile", ContextLength: 131_072},
			{ID: "deepseek-r1-distill-llama-70b", ContextLength: 131_072},
			{ID: "qwen-qwq-32b", ContextLength: 131_072},
		}
	case "xai":
		return []RemoteModel{
			{ID: "grok-3", ContextLength: 131_072},
			{ID: "grok-3-mini", ContextLength: 131_072},
		}
	case "mistral":
		return []RemoteModel{
			{ID: "mistral-large-latest", ContextLength: 131_072},
			{ID: "codestral-latest", ContextLength: 262_144},
		}
	case "together":
		return []RemoteModel{
			{ID: "meta-llama/Llama-3.3-70B-Instruct-Turbo", ContextLength: 131_072},
			{ID: "deepseek-ai/DeepSeek-V3", ContextLength: 131_072},
		}
	case "ollama":
		return []RemoteModel{
			{ID: "qwen2.5-coder:14b", ContextLength: 32_768, HasPricing: true, IsFree: true},
			{ID: "deepseek-r1:14b", ContextLength: 65_536, HasPricing: true, IsFree: true},
		}
	case "lmstudio":
		return []RemoteModel{
			{ID: "local-model", ContextLength: 32_768, HasPricing: true, IsFree: true},
		}
	default:
		return nil
	}
}
