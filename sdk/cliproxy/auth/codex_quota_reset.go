package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const codexQuotaURL = "https://chatgpt.com/backend-api/wham/"

// tryCodexQuotaReset runs only before a response has reached the client. Normal
// account failover always takes precedence over redeeming an existing credit.
func (m *Manager) tryCodexQuotaReset(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, cause error) bool {
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.Codex.AutoResetQuota || cfg.Home.Enabled || len(providers) != 1 || providers[0] != "codex" || statusCodeFromError(cause) != http.StatusTooManyRequests {
		return false
	}
	// Serialize the whole decision, not just the redemption, so concurrent failed
	// requests cannot spend a second credit after the first account recovers.
	generation := m.codexResetGeneration.Load()
	m.codexResetMu.Lock()
	defer m.codexResetMu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	if generation != m.codexResetGeneration.Load() {
		return true
	}
	accounts, candidates, ready := m.codexQuotaResetAccounts(ctx, model, opts)
	if ready {
		return true
	}
	if len(accounts) == 0 {
		return false
	}
	if len(candidates) == 0 {
		// A concurrent reset may already have recovered the requested account.
		return false
	}
	if time.Now().Before(m.codexResetRetryAfter) {
		return false
	}
	executor, ok := m.Executor("codex")
	if !ok {
		return false
	}
	for _, account := range accounts {
		exhausted, errUsage := codexQuotaExhausted(ctx, executor, account)
		if errUsage != nil {
			return false
		}
		if !exhausted {
			// A natural quota renewal also takes precedence over reset credits.
			delete(m.codexResetRequests, account.ID)
			_, _, errReset := m.ResetQuota(ctx, account.ID)
			return errReset == nil
		}
	}
	if m.codexResetBackoff == nil {
		m.codexResetBackoff = make(map[string]time.Time)
	}
	for _, account := range candidates {
		if time.Now().Before(m.codexResetBackoff[account.ID]) {
			continue
		}
		m.codexResetBackoff[account.ID] = time.Now().Add(5 * time.Minute)
		var credits struct {
			AvailableCount  *int `json:"available_count"`
			ApplicableCount *int `json:"applicable_available_count"`
		}
		if errCredits := codexQuotaRequest(ctx, executor, account, http.MethodGet, "rate-limit-reset-credits", nil, &credits); errCredits != nil {
			continue
		}
		available := credits.AvailableCount
		if credits.ApplicableCount != nil {
			available = credits.ApplicableCount
		}
		if available == nil || *available <= 0 {
			continue
		}
		if m.codexResetRequests == nil {
			m.codexResetRequests = make(map[string]string)
		}
		requestID := m.codexResetRequests[account.ID]
		if requestID == "" {
			requestID = uuid.NewString()
			m.codexResetRequests[account.ID] = requestID
		}
		body, _ := json.Marshal(map[string]string{"redeem_request_id": requestID})
		// A failed POST may have spent the credit. Do not try another account.
		m.codexResetRetryAfter = time.Now().Add(5 * time.Minute)
		if errConsume := codexQuotaRequest(ctx, executor, account, http.MethodPost, "rate-limit-reset-credits/consume", body, nil); errConsume != nil {
			log.Warn("codex auto reset: redemption failed; preserving cooldowns and backing off")
			return false
		}
		exhausted, errUsage := codexQuotaExhausted(ctx, executor, account)
		if errUsage != nil || exhausted {
			log.Warn("codex auto reset: redemption did not confirm available quota; preserving cooldowns")
			return false
		}
		if _, _, errReset := m.ResetQuota(ctx, account.ID); errReset != nil {
			log.Warn("codex auto reset: could not persist recovered quota state")
			return false
		}
		log.Info("codex auto reset: all account quotas exhausted; redeemed one reset credit and recovered quota")
		delete(m.codexResetRequests, account.ID)
		m.codexResetGeneration.Add(1)
		return true
	}
	return false
}

// Check the full Codex pool even for a pinned/prefixed request. Only redeem a
// credit on an account the current request is allowed to use.
func (m *Manager) codexQuotaResetAccounts(ctx context.Context, model string, opts cliproxyexecutor.Options) (accounts, candidates []*Auth, ready bool) {
	now := time.Now()
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	for _, account := range m.List() {
		if account.Provider != "codex" || account.Disabled || account.Status == StatusDisabled || account.AuthKind() != AuthKindOAuth || hasUnauthorizedAuthFailure(account) {
			continue
		}
		if expiry, ok := account.AccessTokenExpirationTime(); ok && !expiry.IsZero() && !expiry.After(now) {
			continue
		}
		accounts = append(accounts, account)
		checkModel := m.selectionModelKeyForAuth(account, model)
		blocked, reason, _ := isAuthBlockedForModel(account, checkModel, now)
		eligible := eligibility.allows(account) && (pinned == "" || pinned == account.ID) && m.authSupportsRouteModel(registry.GetGlobalRegistry(), account, model)
		if !blocked {
			// Quota is still usable somewhere. Do not spend a reset credit.
			return nil, nil, eligible
		}
		if reason != blockReasonCooldown {
			continue
		}
		if eligible {
			candidates = append(candidates, account)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if a, b := authPriority(candidates[i]), authPriority(candidates[j]); a != b {
			return a > b
		}
		return candidates[i].ID < candidates[j].ID
	})
	return accounts, candidates, false
}

func codexQuotaExhausted(ctx context.Context, executor ProviderExecutor, account *Auth) (bool, error) {
	var usage struct {
		RateLimit *struct {
			Allowed      *bool `json:"allowed"`
			LimitReached bool  `json:"limit_reached"`
			Primary      *struct {
				UsedPercent float64 `json:"used_percent"`
			} `json:"primary_window"`
			Secondary *struct {
				UsedPercent float64 `json:"used_percent"`
			} `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := codexQuotaRequest(ctx, executor, account, http.MethodGet, "usage", nil, &usage); err != nil {
		return false, err
	}
	limit := usage.RateLimit
	if limit == nil || limit.Allowed == nil {
		return false, fmt.Errorf("missing Codex quota status")
	}
	return !*limit.Allowed || limit.LimitReached || (limit.Primary != nil && limit.Primary.UsedPercent >= 100) || (limit.Secondary != nil && limit.Secondary.UsedPercent >= 100), nil
}

func codexQuotaRequest(ctx context.Context, executor ProviderExecutor, account *Auth, method, path string, body []byte, target any) error {
	req, err := http.NewRequestWithContext(ctx, method, codexQuotaURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "codex-1")
	req.Header.Set("Originator", "Codex Desktop")
	req.Header.Set("User-Agent", "codex-tui/0.149.1")
	if accountID, ok := account.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	resp, err := executor.HttpRequest(ctx, account, req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Codex quota endpoint returned HTTP %d", resp.StatusCode)
	}
	if target == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target)
}
