package cliproxy

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRoutingSoonestResetFirst(t *testing.T) {
	for _, affinity := range []bool{false, true} {
		cfg := &config.Config{Routing: config.RoutingConfig{Strategy: "soonest-reset-first", SessionAffinity: affinity, SessionAffinityTTL: "1h"}}
		state := normalizedRoutingRuntimeState(cfg)
		if state.strategy != "soonest-reset-first" {
			t.Fatalf("strategy = %q", state.strategy)
		}
		selector := newRoutingSelector(state)
		if s, ok := selector.(*coreauth.SessionAffinitySelector); ok {
			defer s.Stop()
		}
		auths := []*coreauth.Auth{
			{ID: "a", Provider: "codex", Attributes: map[string]string{"priority": "10"}},
			{ID: "z", Provider: "codex", Quota: coreauth.QuotaState{ObservedAt: time.Now(), Signals: map[string]string{"X-Codex-Primary-Reset-At": strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}}},
		}
		got, err := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{Headers: http.Header{"Session-Id": {"routing-reset-session"}}}, auths)
		if err != nil || got == nil || got.ID != "z" {
			t.Fatalf("affinity=%t got=%v error=%v", affinity, got, err)
		}
	}
}
