package auth

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// SoonestResetFirstSelector consumes the earliest known rolling quota window.
// Known future resets outrank unknown or expired observations. Priority breaks
// equal/unknown reset ties, then ID. No quota probe runs on the request path.
// The scheduler deliberately treats this as a custom selector: its fast path
// narrows candidates to the highest priority before calling a selector.
type SoonestResetFirstSelector struct{}

func selectorUsesResetOrder(selector Selector) bool {
	if affinity, ok := selector.(*SessionAffinitySelector); ok {
		selector = affinity.fallback
	}
	_, ok := selector.(*SoonestResetFirstSelector)
	return ok
}

func fallbackPriorityAuths(selector Selector, available []*Auth) []*Auth {
	if selectorUsesResetOrder(selector) {
		return available
	}
	return highestPriorityAuths(available)
}

func (s *SoonestResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getSelectorAvailableAuthsAcrossPriorities(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	var best *Auth
	var bestReset time.Time
	for _, candidate := range available {
		reset := codexSoonestReset(candidate, now)
		if best == nil || resetOrderBefore(reset, bestReset) || reset.Equal(bestReset) &&
			(authPriority(candidate) > authPriority(best) || authPriority(candidate) == authPriority(best) && candidate.ID < best.ID) {
			best, bestReset = candidate, reset
		}
	}
	return best, nil
}

func resetOrderBefore(a, b time.Time) bool {
	return !a.IsZero() && (b.IsZero() || a.Before(b))
}

func codexSoonestReset(candidate *Auth, now time.Time) time.Time {
	if candidate == nil || !strings.EqualFold(candidate.Provider, "codex") {
		return time.Time{}
	}
	q := candidate.Quota
	if q.ObservedAt.IsZero() || q.ObservedAt.After(now) || now.Sub(q.ObservedAt) > codexQuotaObservationMaxAge {
		return time.Time{}
	}
	var earliest time.Time
	// Only credential-wide primary/secondary budgets apply here. Named additional
	// limits and code-review quotas belong to other routes and must not rank them.
	for _, window := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + window + "-"
		// A consumed window cannot rank usable quota. In particular, its old
		// reset date must not survive a confirmed reset credit as a new deadline.
		if used, err := strconv.ParseFloat(q.Signals[prefix+"Used-Percent"], 64); err == nil && used >= 100 {
			continue
		}
		reset := codexWindowReset(q, prefix)
		// A passed reset does not establish the start of the next rolling window.
		if reset.After(now) && (earliest.IsZero() || reset.Before(earliest)) {
			earliest = reset
		}
	}
	return earliest
}

func codexWindowReset(q QuotaState, prefix string) time.Time {
	if seconds, err := strconv.ParseInt(q.Signals[prefix+"Reset-At"], 10, 64); err == nil && seconds > 0 {
		return time.Unix(seconds, 0)
	}
	if seconds, err := strconv.ParseFloat(q.Signals[prefix+"Reset-After-Seconds"], 64); err == nil && !q.ObservedAt.IsZero() && seconds >= 0 && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds < float64(math.MaxInt64)/float64(time.Second) {
		return q.ObservedAt.Add(time.Duration(seconds * float64(time.Second)))
	}
	return time.Time{}
}
