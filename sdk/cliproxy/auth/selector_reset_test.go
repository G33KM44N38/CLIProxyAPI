package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func resetTestAuth(id string, priority int, reset time.Time) *Auth {
	a := &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": strconv.Itoa(priority)}}
	if !reset.IsZero() {
		a.Quota.ObserveResponseHeadersForProvider("codex", http.Header{"X-Codex-Primary-Reset-At": {strconv.FormatInt(reset.Unix(), 10)}}, time.Now())
	}
	return a
}

func TestSoonestResetFirstOrder(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		modify func(a, b *Auth)
		want   string
	}{
		{"reset beats ID and priority", func(a, b *Auth) {}, "z"},
		{"unknown after known", func(a, b *Auth) { a.Quota.Signals = nil }, "z"},
		{"all unknown uses priority", func(a, b *Auth) { a.Quota.Signals = nil; b.Quota.Signals = nil }, "a"},
		{"expired reset is unknown", func(a, b *Auth) {
			a.Quota.Signals["X-Codex-Primary-Reset-At"] = strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
		}, "z"},
		{"secondary earlier than primary", func(a, b *Auth) {
			a.Quota.Signals["X-Codex-Secondary-Reset-At"] = strconv.FormatInt(now.Add(time.Minute).Unix(), 10)
		}, "a"},
		{"relative anchored to observation", func(a, b *Auth) {
			a.Quota.ObservedAt = now.Add(-time.Minute)
			a.Quota.Signals = map[string]string{"X-Codex-Primary-Reset-After-Seconds": "120"}
		}, "a"},
		{"consumed window is not a renewal deadline", func(a, b *Auth) {
			a.Quota.Signals["X-Codex-Primary-Reset-At"] = strconv.FormatInt(now.Add(time.Minute).Unix(), 10)
			a.Quota.Signals["X-Codex-Primary-Used-Percent"] = "100"
		}, "z"},
		{"disabled skipped", func(a, b *Auth) { b.Disabled = true }, "a"},
		{"credential quota skipped", func(a, b *Auth) {
			b.Quota.Exceeded = true
			b.Quota.Reason = "credential_quota"
			b.Quota.NextRecoverAt = now.Add(time.Hour)
		}, "a"},
		{"model cooldown skipped", func(a, b *Auth) {
			b.ModelStates = map[string]*ModelState{"gpt-test": {Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true}}}
		}, "a"},
		{"unrelated quota ignored", func(a, b *Auth) {
			a.Quota.Signals["X-Codex-Code-Review-Primary-Reset-At"] = strconv.FormatInt(now.Add(time.Second).Unix(), 10)
		}, "z"},
		{"tie uses priority", func(a, b *Auth) { b.Quota.Signals = a.Quota.Clone().Signals }, "a"},
		{"tie uses ID", func(a, b *Auth) { b.Quota.Signals = a.Quota.Clone().Signals; b.Attributes["priority"] = "10" }, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := resetTestAuth("a", 10, now.Add(24*time.Hour)), resetTestAuth("z", 0, now.Add(2*time.Hour))
			tc.modify(a, b)
			got, err := (&SoonestResetFirstSelector{}).Pick(context.Background(), "codex", "gpt-test", cliproxyexecutor.Options{}, []*Auth{a, b})
			if err != nil || got == nil || got.ID != tc.want {
				t.Fatalf("picked %v, error %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestSoonestResetFirstManagerAffinity(t *testing.T) {
	for _, affinity := range []bool{false, true} {
		t.Run(strconv.FormatBool(affinity), func(t *testing.T) {
			var selector Selector = &SoonestResetFirstSelector{}
			if affinity {
				s := NewSessionAffinitySelector(selector)
				t.Cleanup(s.Stop)
				selector = s
			}
			m := NewManager(nil, selector, nil)
			m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex"})
			now := time.Now()
			a, b := resetTestAuth("a-reset-test-"+t.Name(), 10, now.Add(24*time.Hour)), resetTestAuth("z-reset-test-"+t.Name(), 0, now.Add(2*time.Hour))
			for _, a := range []*Auth{a, b} {
				registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: "gpt-reset-test"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
				if _, err := m.Register(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			pick := func(session, want string) {
				t.Helper()
				got, err := m.SelectAuth(context.Background(), "codex", "gpt-reset-test", cliproxyexecutor.Options{Headers: http.Header{"Session-Id": {session}}})
				if err != nil || got == nil || got.ID != want {
					t.Fatalf("session %s: got %v error %v want %s", session, got, err, want)
				}
			}
			pick("first", b.ID)
			// A newly observed earlier reset must affect new sessions, not an existing binding.
			a.Quota = b.Quota.Clone()
			a.Quota.Signals["X-Codex-Primary-Reset-At"] = strconv.FormatInt(now.Add(time.Minute).Unix(), 10)
			if _, err := m.Update(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			if affinity {
				pick("first", b.ID)
			}
			pick("new", a.ID)
			// A real 429 makes the bound account unavailable and permits rebinding.
			retry := time.Hour
			m.MarkResult(context.Background(), Result{AuthID: b.ID, Provider: "codex", Model: "gpt-reset-test", Error: &Error{HTTPStatus: 429, Message: "quota"}, RetryAfter: &retry})
			pick("first", a.ID)
		})
	}
}

func TestSoonestResetFirstPreservesAutoReset(t *testing.T) {
	f := &codexResetFixture{exhausted: map[string]bool{"reset-a": true, "reset-b": true}, credits: 3}
	m := newCodexResetFixture(t, f, true, true)
	m.SetSelector(&SoonestResetFirstSelector{})
	// Percentages observed before redemption must never become a second cooldown
	// that prevents the existing auto-reset flow from retrying the recovered auth.
	for _, id := range []string{"reset-a", "reset-b"} {
		a, _ := m.GetByID(id)
		a.Quota.ObserveResponseHeadersForProvider("codex", http.Header{
			"X-Codex-Primary-Used-Percent": {"100"},
			"X-Codex-Primary-Reset-At":     {strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)},
		}, time.Now())
		if _, err := m.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	response, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-reset-test"}, cliproxyexecutor.Options{})
	if err != nil || string(response.Payload) != `{"output":"recovered"}` {
		t.Fatalf("response=%s error=%v", response.Payload, err)
	}
	if f.consumes != 1 {
		t.Fatalf("redemptions=%d want 1", f.consumes)
	}
}
