package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetBalanceDeepSeekUSDAndCNY(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"is_available": true,
			"balance_infos": [
				{"currency": "CNY", "total_balance": "45.00", "granted_balance": "5.00", "topped_up_balance": "40.00"},
				{"currency": "USD", "total_balance": "12.34", "granted_balance": "2.00", "topped_up_balance": "10.34"}
			]
		}`))
	}))
	defer srv.Close()

	client := NewOpenAICompatClient("deepseek", srv.URL+"/v1/", "sk-ds-123\r\n", nil)
	bal, err := client.GetBalance(context.Background())
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if gotPath != "/user/balance" {
		t.Fatalf("path = %q, want /user/balance", gotPath)
	}
	if gotAuth != "Bearer sk-ds-123" {
		t.Fatalf("auth = %q, want Bearer sk-ds-123", gotAuth)
	}
	if bal.Currency != "USD" || bal.Remaining != 12.34 || bal.Granted != 2.0 || bal.ToppedUp != 10.34 || !bal.Available {
		t.Fatalf("unexpected balance: %+v", bal)
	}
	if got := bal.FormatCompact(); got != "¥45.00 CNY · $12.34 USD left" {
		t.Fatalf("FormatCompact = %q, want %q", got, "¥45.00 CNY · $12.34 USD left")
	}
}

func TestGetBalanceDeepSeekCNYOnlyWhenUSDZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"is_available": true,
			"balance_infos": [
				{"currency": "CNY", "total_balance": "11.00", "granted_balance": "0.00", "topped_up_balance": "11.00"},
				{"currency": "USD", "total_balance": "0.00", "granted_balance": "0.00", "topped_up_balance": "0.00"}
			]
		}`))
	}))
	defer srv.Close()

	client := NewOpenAICompatClient("deepseek", srv.URL, "sk-ds-cny", nil)
	bal, err := client.GetBalance(context.Background())
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if bal.Currency != "CNY" || bal.Remaining != 11.0 {
		t.Fatalf("expected CNY 11.00 primary balance, got %+v", bal)
	}
	if got := bal.FormatCompact(); got != "¥11.00 CNY left" {
		t.Fatalf("FormatCompact = %q, want %q", got, "¥11.00 CNY left")
	}
}

func TestGetBalanceOpenRouterCreditsAndFallback(t *testing.T) {
	t.Run("credits endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/credits" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"total_credits":10.0,"total_usage":2.45}}`))
		}))
		defer srv.Close()

		c := NewOpenAICompatClient("openrouter", srv.URL+"/v1", "sk-or-test", nil)
		bal, err := c.GetBalance(context.Background())
		if err != nil {
			t.Fatalf("GetBalance: %v", err)
		}
		if bal.Remaining < 7.54 || bal.Remaining > 7.56 || bal.Total != 10.0 || bal.Used != 2.45 {
			t.Fatalf("unexpected balance: %+v", bal)
		}
		if got := bal.FormatCompact(); got != "$7.55 USD left" {
			t.Fatalf("FormatCompact = %q, want $7.55 USD left", got)
		}
	})

	t.Run("auth/key fallback", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/credits":
				w.WriteHeader(http.StatusForbidden)
			case "/v1/auth/key":
				_, _ = w.Write([]byte(`{"data":{"limit":5.0,"limit_remaining":3.25,"usage":1.75,"is_free_tier":false}}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		c := NewOpenAICompatClient("openrouter", srv.URL+"/v1", "sk-or-test", nil)
		bal, err := c.GetBalance(context.Background())
		if err != nil {
			t.Fatalf("GetBalance: %v", err)
		}
		if !bal.HasLimit || bal.Remaining != 3.25 || bal.Total != 5.0 || bal.Used != 1.75 {
			t.Fatalf("unexpected fallback balance: %+v", bal)
		}
	})
}

func TestGetBalanceFreeLocalAndUnsupported(t *testing.T) {
	freeClient := NewOpenAICompatClient("free", "https://text.pollinations.ai/openai", "", nil)
	bal, err := freeClient.GetBalance(context.Background())
	if err != nil || !bal.IsFreeTier || bal.FormatCompact() != "free tier" {
		t.Fatalf("free balance = %+v, err=%v", bal, err)
	}

	openaiClient := NewOpenAICompatClient("openai", "https://api.openai.com/v1", "sk-test", nil)
	if _, err := openaiClient.GetBalance(context.Background()); !errors.Is(err, ErrBalanceUnsupported) {
		t.Fatalf("openai err = %v, want ErrBalanceUnsupported", err)
	}
}
