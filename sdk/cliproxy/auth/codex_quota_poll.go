package auth

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	codexQuotaPollInterval       = 60 * time.Second
	codexQuotaPollDefaultTimeout = 10 * time.Second
	codexQuotaObservationMaxAge  = 600 * time.Second
)

// This loop shares the auth-refresh lifecycle, but only performs read-only usage
// GETs with existing access tokens. It never logs in, refreshes OAuth, redeems
// credits, persists credentials, or changes cooldown/affinity state.
func (m *Manager) runCodexQuotaPolling(ctx context.Context) {
	ticker := time.NewTicker(codexQuotaPollInterval)
	defer ticker.Stop()
	m.runCodexQuotaPollingTicks(ctx, ticker.C)
}

func (m *Manager) runCodexQuotaPollingTicks(ctx context.Context, ticks <-chan time.Time) {
	for {
		m.pollCodexQuotas(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

func codexQuotaPollEligible(a *Auth, now time.Time) bool {
	if a == nil || !strings.EqualFold(a.Provider, "codex") || a.Disabled || a.Status == StatusDisabled || hasUnauthorizedAuthFailure(a) {
		return false
	}
	token, _ := a.Metadata["access_token"].(string)
	account, _ := a.Metadata["account_id"].(string)
	if token == "" || account == "" {
		return false
	}
	if expiry, ok := a.AccessTokenExpirationTime(); ok && !expiry.IsZero() && !expiry.After(now) {
		return false
	}
	return true
}

func (m *Manager) pollCodexQuotas(ctx context.Context) []<-chan struct{} {
	if ctx.Err() != nil || m.HomeEnabled() {
		return nil
	}
	m.mu.RLock()
	if !selectorUsesResetOrder(m.selector) {
		m.mu.RUnlock()
		return nil
	}
	executor, ok := m.executors["codex"]
	if !ok {
		m.mu.RUnlock()
		return nil
	}
	timeout := m.codexQuotaPollTimeout
	if timeout <= 0 {
		timeout = codexQuotaPollDefaultTimeout
	}
	accounts := make([]*Auth, 0, len(m.auths))
	now := time.Now()
	for _, a := range m.auths {
		if codexQuotaPollEligible(a, now) {
			accounts = append(accounts, a.Clone())
		}
	}
	m.mu.RUnlock()
	completions := make([]<-chan struct{}, 0, len(accounts))
	for _, a := range accounts {
		m.codexQuotaPollMu.Lock()
		if m.codexQuotaPollInFlight == nil {
			m.codexQuotaPollInFlight = make(map[string]chan struct{})
			m.codexQuotaPollSlots = make(chan struct{}, refreshMaxConcurrency)
		}
		done, running := m.codexQuotaPollInFlight[a.ID]
		if running {
			completions = append(completions, done)
			m.codexQuotaPollMu.Unlock()
			continue
		}
		done = make(chan struct{})
		m.codexQuotaPollInFlight[a.ID] = done
		slots := m.codexQuotaPollSlots
		completions = append(completions, done)
		m.codexQuotaPollMu.Unlock()
		go func(a *Auth) {
			defer func() {
				m.codexQuotaPollMu.Lock()
				delete(m.codexQuotaPollInFlight, a.ID)
				close(done)
				m.codexQuotaPollMu.Unlock()
			}()
			// One in-flight GET per account and a shared concurrency bound across ticks.
			// A slow account never prevents a healthy account's next 60s observation.
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			if ctx.Err() != nil {
				return
			}
			// Bound this read-only probe through response-body decoding as well.
			probeCtx, cancelProbe := context.WithTimeout(ctx, timeout)
			defer cancelProbe()
			observedAt := time.Now()
			var usage codexPollingUsage
			if err := codexQuotaRequest(probeCtx, executor, a, http.MethodGet, "usage", nil, &usage); err != nil {
				return
			}
			headers := usage.headers()
			if len(headers) == 0 {
				return
			}
			m.mu.Lock()
			current := m.auths[a.ID]
			publishedAt := time.Now()
			// A removed/replaced/refreshed credential or a newer response wins. Only
			// observation fields are changed, never availability or generation state.
			if probeCtx.Err() == nil && selectorUsesResetOrder(m.selector) && codexQuotaPollEligible(current, publishedAt) && current.RegistrationEpoch == a.RegistrationEpoch && current.Generation == a.Generation && (!current.Quota.ObservedAt.After(observedAt) || current.Quota.ObservedAt.After(publishedAt)) {
				current.Quota.ObserveResponseHeadersForProvider("codex", headers, observedAt)
			}
			m.mu.Unlock()
		}(a)
	}
	return completions
}

type codexPollingWindow struct {
	UsedPercent *float64 `json:"used_percent"`
	ResetAt     *int64   `json:"reset_at"`
	ResetAfter  *int64   `json:"reset_after_seconds"`
}

type codexPollingUsage struct {
	RateLimit *struct {
		Allowed      *bool               `json:"allowed"`
		LimitReached *bool               `json:"limit_reached"`
		Primary      *codexPollingWindow `json:"primary_window"`
		Secondary    *codexPollingWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

func (u codexPollingUsage) headers() http.Header {
	if u.RateLimit == nil {
		return nil
	}
	headers := make(http.Header)
	if v := u.RateLimit.Allowed; v != nil {
		headers.Set("X-Codex-Allowed", strconv.FormatBool(*v))
	}
	if v := u.RateLimit.LimitReached; v != nil {
		headers.Set("X-Codex-Limit-Reached", strconv.FormatBool(*v))
	}
	for name, w := range map[string]*codexPollingWindow{"Primary": u.RateLimit.Primary, "Secondary": u.RateLimit.Secondary} {
		if w == nil || w.UsedPercent == nil || math.IsNaN(*w.UsedPercent) || math.IsInf(*w.UsedPercent, 0) || *w.UsedPercent < 0 || *w.UsedPercent > 100 {
			continue
		}
		if (w.ResetAt == nil || *w.ResetAt <= 0) && (w.ResetAfter == nil || *w.ResetAfter < 0) {
			continue
		}
		prefix := "X-Codex-" + name + "-"
		headers.Set(prefix+"Used-Percent", strconv.FormatFloat(*w.UsedPercent, 'f', -1, 64))
		if w.ResetAt != nil && *w.ResetAt > 0 {
			headers.Set(prefix+"Reset-At", strconv.FormatInt(*w.ResetAt, 10))
		}
		if w.ResetAfter != nil && *w.ResetAfter >= 0 {
			headers.Set(prefix+"Reset-After-Seconds", strconv.FormatInt(*w.ResetAfter, 10))
		}
	}
	return headers
}
