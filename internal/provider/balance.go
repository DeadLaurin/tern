package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrBalanceUnsupported is returned when a provider does not expose an
// account/credit balance endpoint.
var ErrBalanceUnsupported = errors.New("provider: balance check not supported for this provider")

const defaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"

// CurrencyBalance holds one currency entry returned by a multi-currency
// provider such as DeepSeek (CNY and/or USD).
type CurrencyBalance struct {
	Currency string  `json:"currency"`
	Total    float64 `json:"total"`
	Granted  float64 `json:"granted,omitempty"`
	ToppedUp float64 `json:"topped_up,omitempty"`
}

// BalanceInfo describes the remaining account or API-key credit balance
// reported by a model provider.
type BalanceInfo struct {
	Provider   string            `json:"provider"`
	Currency   string            `json:"currency,omitempty"`
	Remaining  float64           `json:"remaining"`
	Total      float64           `json:"total,omitempty"`
	Used       float64           `json:"used,omitempty"`
	Granted    float64           `json:"granted,omitempty"`
	ToppedUp   float64           `json:"topped_up,omitempty"`
	Balances   []CurrencyBalance `json:"balances,omitempty"`
	HasLimit   bool              `json:"has_limit,omitempty"`
	IsFreeTier bool              `json:"is_free_tier,omitempty"`
	Available  bool              `json:"available"`
	Note       string            `json:"note,omitempty"`
}

func formatCurrencyAmount(amount float64, currency string) string {
	cur := strings.ToUpper(strings.TrimSpace(currency))
	switch cur {
	case "", "USD":
		return fmt.Sprintf("$%.2f USD", amount)
	case "CNY", "RMB":
		return fmt.Sprintf("¥%.2f CNY", amount)
	case "EUR":
		return fmt.Sprintf("€%.2f EUR", amount)
	case "GBP":
		return fmt.Sprintf("£%.2f GBP", amount)
	default:
		return fmt.Sprintf("%.2f %s", amount, cur)
	}
}

// FormatCompact returns a short human-readable summary of the balance suitable
// for status bars and sidebars (e.g. "¥11.00 CNY left", "$12.34 USD left",
// "free tier", "unlimited").
func (b *BalanceInfo) FormatCompact() string {
	if b == nil {
		return ""
	}
	if len(b.Balances) > 0 {
		var nonZero []string
		for _, cb := range b.Balances {
			if cb.Total > 0 {
				nonZero = append(nonZero, formatCurrencyAmount(cb.Total, cb.Currency))
			}
		}
		if len(nonZero) > 0 {
			return strings.Join(nonZero, " · ") + " left"
		}
		return formatCurrencyAmount(b.Balances[0].Total, b.Balances[0].Currency) + " left"
	}
	if b.HasLimit {
		return formatCurrencyAmount(b.Remaining, b.Currency) + " left"
	}
	if b.IsFreeTier {
		return "free tier"
	}
	if b.Available {
		return "unlimited"
	}
	return "unavailable"
}

// BalanceChecker is an optional interface implemented by providers that can
// report remaining account or API-key credits.
type BalanceChecker interface {
	GetBalance(ctx context.Context) (*BalanceInfo, error)
}

var _ BalanceChecker = (*DeepSeekClient)(nil)

// GetBalance fetches the remaining credit balance for the configured provider.
func (c *DeepSeekClient) GetBalance(ctx context.Context) (*BalanceInfo, error) {
	if c == nil {
		return nil, ErrBalanceUnsupported
	}
	prov := c.effectiveBalanceProvider()
	switch prov {
	case "deepseek":
		return c.getDeepSeekBalance(ctx)
	case "openrouter":
		return c.getOpenRouterBalance(ctx)
	case "free", "ollama", "lmstudio", "local":
		return &BalanceInfo{
			Provider:   prov,
			IsFreeTier: true,
			Available:  true,
			Note:       "free / local — no paid balance",
		}, nil
	default:
		return nil, ErrBalanceUnsupported
	}
}

func (c *DeepSeekClient) effectiveBalanceProvider() string {
	if prov := strings.ToLower(strings.TrimSpace(c.ProviderName)); prov != "" {
		return prov
	}
	raw := strings.ToLower(strings.TrimSpace(c.BaseURL))
	if raw == "" {
		return "deepseek"
	}
	if strings.Contains(raw, "openrouter.ai") {
		return "openrouter"
	}
	if strings.Contains(raw, "pollinations.ai") {
		return "free"
	}
	if isLoopbackURL(raw) {
		return "local"
	}
	return "deepseek"
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func (c *DeepSeekClient) balanceHTTPClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 12 * time.Second}
}

func (c *DeepSeekClient) doBalanceGET(ctx context.Context, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("provider: build balance request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token := sanitizeHeaderToken(c.APIKey); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range c.ExtraHeaders {
		if strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.balanceHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("provider: fetch balance: %w", err)
	}
	return resp, nil
}

func (c *DeepSeekClient) deepSeekBalanceEndpoint() string {
	base := strings.TrimSpace(c.BaseURL)
	if base == "" {
		base = defaultDeepSeekBaseURL
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(strings.ToLower(base), "/v1") {
		base = base[:len(base)-len("/v1")]
		base = strings.TrimRight(base, "/")
	}
	return base + "/user/balance"
}

func (c *DeepSeekClient) getDeepSeekBalance(ctx context.Context) (*BalanceInfo, error) {
	resp, err := c.doBalanceGET(ctx, c.deepSeekBalanceEndpoint())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr, _ := c.readAPIError(resp)
		return nil, apiErr
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("provider: read deepseek balance response: %w", err)
	}

	var payload struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency        string `json:"currency"`
			TotalBalance    string `json:"total_balance"`
			GrantedBalance  string `json:"granted_balance"`
			ToppedUpBalance string `json:"topped_up_balance"`
		} `json:"balance_infos"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("provider: decode deepseek balance response: %w", err)
	}

	info := &BalanceInfo{
		Provider:  "deepseek",
		Available: payload.IsAvailable,
		HasLimit:  true,
	}
	if len(payload.BalanceInfos) == 0 {
		return info, nil
	}

	balances := make([]CurrencyBalance, 0, len(payload.BalanceInfos))
	for _, entry := range payload.BalanceInfos {
		total, err := parseOptionalFloat(entry.TotalBalance)
		if err != nil {
			return nil, fmt.Errorf("provider: parse deepseek total_balance: %w", err)
		}
		granted, err := parseOptionalFloat(entry.GrantedBalance)
		if err != nil {
			return nil, fmt.Errorf("provider: parse deepseek granted_balance: %w", err)
		}
		toppedUp, err := parseOptionalFloat(entry.ToppedUpBalance)
		if err != nil {
			return nil, fmt.Errorf("provider: parse deepseek topped_up_balance: %w", err)
		}
		balances = append(balances, CurrencyBalance{
			Currency: strings.ToUpper(strings.TrimSpace(entry.Currency)),
			Total:    total,
			Granted:  granted,
			ToppedUp: toppedUp,
		})
	}
	info.Balances = balances

	// Pick the primary balance: prefer USD if it has a positive balance,
	// otherwise prefer the first currency with a positive balance (e.g. CNY),
	// falling back to the first entry.
	chosen := balances[0]
	for _, cb := range balances {
		if cb.Total > 0 {
			chosen = cb
			break
		}
	}
	for _, cb := range balances {
		if cb.Currency == "USD" && cb.Total > 0 {
			chosen = cb
			break
		}
	}

	info.Currency = chosen.Currency
	info.Remaining = chosen.Total
	info.Granted = chosen.Granted
	info.ToppedUp = chosen.ToppedUp
	return info, nil
}

func (c *DeepSeekClient) openRouterBaseURL() string {
	base := strings.TrimSpace(c.BaseURL)
	if base == "" {
		base = defaultOpenRouterBaseURL
	}
	return strings.TrimRight(base, "/")
}

func (c *DeepSeekClient) getOpenRouterBalance(ctx context.Context) (*BalanceInfo, error) {
	base := c.openRouterBaseURL()

	resp, err := c.doBalanceGET(ctx, base+"/credits")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			return nil, fmt.Errorf("provider: read openrouter credits response: %w", readErr)
		}
		var payload struct {
			Data struct {
				TotalCredits float64 `json:"total_credits"`
				TotalUsage   float64 `json:"total_usage"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("provider: decode openrouter credits response: %w", err)
		}
		remaining := payload.Data.TotalCredits - payload.Data.TotalUsage
		if remaining < 0 {
			remaining = 0
		}
		return &BalanceInfo{
			Provider:  "openrouter",
			Currency:  "USD",
			Remaining: remaining,
			Total:     payload.Data.TotalCredits,
			Used:      payload.Data.TotalUsage,
			Available: remaining > 0,
			HasLimit:  true,
		}, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()

	keyResp, err := c.doBalanceGET(ctx, base+"/auth/key")
	if err != nil {
		return nil, err
	}
	if keyResp.StatusCode < 200 || keyResp.StatusCode >= 300 {
		apiErr, _ := c.readAPIError(keyResp)
		return nil, apiErr
	}
	defer keyResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(keyResp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("provider: read openrouter auth/key response: %w", err)
	}
	var payload struct {
		Data struct {
			Limit          *float64 `json:"limit"`
			LimitRemaining *float64 `json:"limit_remaining"`
			Usage          float64  `json:"usage"`
			IsFreeTier     bool     `json:"is_free_tier"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("provider: decode openrouter auth/key response: %w", err)
	}

	info := &BalanceInfo{
		Provider:   "openrouter",
		Currency:   "USD",
		Used:       payload.Data.Usage,
		IsFreeTier: payload.Data.IsFreeTier,
		Available:  true,
	}
	if payload.Data.LimitRemaining != nil {
		rem := *payload.Data.LimitRemaining
		if rem < 0 {
			rem = 0
		}
		info.Remaining = rem
		info.HasLimit = true
		info.Available = *payload.Data.LimitRemaining > 0
		if payload.Data.Limit != nil {
			info.Total = *payload.Data.Limit
		}
	}
	return info, nil
}

func parseOptionalFloat(raw string) (float64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}
