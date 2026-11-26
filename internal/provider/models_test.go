package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListModelsOpenRouterPricingAndFreeDetection(t *testing.T) {
	var gotAuth, gotReferer, gotTitle, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-Title")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"id": "deepseek/deepseek-r1:free",
					"name": "DeepSeek R1 (free)",
					"context_length": 163840,
					"pricing": {"prompt": "0", "completion": "0"},
					"architecture": {"modality": "text->text"}
				},
				{
					"id": "anthropic/claude-sonnet-4",
					"name": "Claude Sonnet 4",
					"context_length": 200000,
					"pricing": {"prompt": "0.000003", "completion": "0.000015"},
					"architecture": {"modality": "text+image->text"}
				},
				{
					"id": "google/gemma-3-27b-it",
					"context_length": 131072,
					"pricing": {"prompt": "0", "completion": "0"}
				}
			]
		}`))
	}))
	defer server.Close()

	client := NewOpenAICompatClient("openrouter", server.URL+"/v1", "sk-or-test", map[string]string{
		"HTTP-Referer": "https://tern.disgusting.work",
		"X-Title":      "tern",
	})
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-or-test" || gotReferer != "https://tern.disgusting.work" || gotTitle != "tern" {
		t.Fatalf("unexpected headers: auth=%q referer=%q title=%q", gotAuth, gotReferer, gotTitle)
	}
	if len(models) != 3 {
		t.Fatalf("len(models) = %d, want 3", len(models))
	}

	if !models[0].IsFree || models[0].PromptPerM != 0 || models[0].ContextLength != 163840 {
		t.Fatalf("models[0] = %+v, want free model", models[0])
	}
	if models[1].IsFree || !models[1].HasPricing || models[1].PromptPerM < 2.99 || models[1].PromptPerM > 3.01 || models[1].OutputPerM < 14.99 || !models[1].SupportsVision {
		t.Fatalf("models[1] = %+v, want paid vision model ($3/$15)", models[1])
	}
	if !models[2].IsFree {
		t.Fatalf("models[2] = %+v, want zero-price model marked IsFree", models[2])
	}
}
