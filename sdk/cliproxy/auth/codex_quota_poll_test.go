package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestCodexQuotaBackgroundColdStart(t *testing.T) {
	var probes atomic.Int32
	observed := make(chan string, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("unexpected quota operation %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		id := r.Header.Get("Chatgpt-Account-Id")
		if id != "a" && id != "z" {
			t.Errorf("ineligible account probed: %s", id)
		}
		probes.Add(1)
		after := 9000
		if id == "z" {
			after = 2000
		}
		_, _ = fmt.Fprintf(w, `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":10,"reset_after_seconds":%d,"limit_window_seconds":18000}}}`, after)
		observed <- id
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	m := NewManager(nil, &SoonestResetFirstSelector{}, nil)
	m.RegisterExecutor(&codexResetExecutor{base: base})
	for _, id := range []string{"a", "z", "disabled", "expired", "api-key"} {
		a := resetTestAuth(id, 0, time.Time{})
		a.Metadata = map[string]any{"access_token": "fixture-access-only", "account_id": id, "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}
		if id == "a" {
			a.Attributes["priority"] = "10"
		}
		if id == "disabled" {
			a.Disabled = true
		}
		if id == "expired" {
			a.Metadata["expired"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
		}
		if id == "api-key" {
			a.Metadata = nil
		}
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartAutoRefresh(ctx, time.Hour)
	defer m.StopAutoRefresh()
	seen := map[string]bool{}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for len(seen) < 2 {
		select {
		case id := <-observed:
			seen[id] = true
		case <-deadline.C:
			t.Fatal("cold start did not probe all enabled access-only Codex accounts")
		}
	}
	waitCodexQuotaPolls(t, m)
	available := m.List()
	for i := 0; i < 3; i++ {
		got, err := (&SoonestResetFirstSelector{}).Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, available)
		if err != nil || got.ID != "z" {
			t.Fatalf("cold selection=%v error=%v", got, err)
		}
	}
	if probes.Load() != 2 {
		t.Fatalf("quota calls=%d; requests must not probe", probes.Load())
	}
	a, _ := m.GetByID("z")
	if a.Metadata["access_token"] != "fixture-access-only" || a.Metadata["refresh_token"] != nil || a.Quota.Exceeded {
		t.Fatal("quota read changed authentication or cooldown")
	}
}

func TestCodexResetObservationFreshness(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		name     string
		observed time.Time
		known    bool
	}{
		{"current", now, true}, {"boundary", now.Add(-600 * time.Second), true}, {"stale", now.Add(-600*time.Second - time.Nanosecond), false}, {"future", now.Add(time.Nanosecond), false}, {"missing", time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := resetTestAuth("test", 0, now.Add(7*24*time.Hour))
			a.Quota.ObservedAt = tc.observed
			if got := !codexSoonestReset(a, now).IsZero(); got != tc.known {
				t.Fatalf("known=%t want %t", got, tc.known)
			}
		})
	}
}

func TestCodexQuotaPollingTicksAndCancellation(t *testing.T) {
	if codexQuotaPollInterval != 60*time.Second {
		t.Fatal("quota polling must run every 60s")
	}
	var calls atomic.Int32
	responses := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":10,"reset_after_seconds":2000}}}`)
		responses <- struct{}{}
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	m := NewManager(nil, &SoonestResetFirstSelector{}, nil)
	m.RegisterExecutor(&codexResetExecutor{base: base})
	a := resetTestAuth("poll-periodic", 0, time.Time{})
	a.Metadata = map[string]any{"account_id": "poll-periodic", "access_token": "fixture"}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.runCodexQuotaPollingTicks(ctx, ticks); close(done) }()
	wait := func(c <-chan struct{}) {
		t.Helper()
		select {
		case <-c:
		case <-time.After(2 * time.Second):
			t.Fatal("background round did not complete")
		}
	}
	wait(responses)
	waitCodexQuotaPolls(t, m)
	// Simulate the next 60s tick without sleeping.
	ticks <- time.Now()
	wait(responses)
	cancel()
	wait(done)
	if calls.Load() != 2 {
		t.Fatalf("rounds=%d want 2", calls.Load())
	}
}

func TestCodexQuotaPollDiscardsStaleResults(t *testing.T) {
	for _, mode := range []string{"disable", "replace", "newer-response", "cancel", "strategy-change", "failure"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			releaseProbe := sync.OnceFunc(func() { close(release) })
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				if mode == "failure" {
					w.WriteHeader(500)
					return
				}
				_, _ = fmt.Fprint(w, `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":10,"reset_after_seconds":2000}}}`)
			}))
			defer server.Close()
			defer releaseProbe()
			base, _ := url.Parse(server.URL)
			m := NewManager(nil, &SoonestResetFirstSelector{}, nil)
			m.RegisterExecutor(&codexResetExecutor{base: base})
			a := resetTestAuth("poll-stale", 0, time.Now().Add(time.Hour))
			a.Metadata = map[string]any{"account_id": "poll-stale", "access_token": "fixture"}
			if _, err := m.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			before, _ := m.GetByID(a.ID)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				for _, ch := range m.pollCodexQuotas(ctx) {
					<-ch
				}
				close(done)
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("quota probe not started")
			}
			switch mode {
			case "disable":
				a.Disabled = true
				_, _ = m.Update(context.Background(), a)
			case "replace":
				a.Metadata["access_token"] = "replacement"
				_, _ = m.Update(context.Background(), a)
			case "newer-response":
				m.mu.Lock()
				m.auths[a.ID].Quota.ObserveResponseHeadersForProvider("codex", http.Header{"X-Codex-Primary-Reset-After-Seconds": {"42"}}, time.Now())
				m.mu.Unlock()
			case "cancel":
				cancel()
			case "strategy-change":
				m.SetSelector(&FillFirstSelector{})
			}
			releaseProbe()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("quota probe not stopped")
			}
			got, _ := m.GetByID(a.ID)
			if mode == "newer-response" {
				if got.Quota.Signals["X-Codex-Primary-Reset-After-Seconds"] != "42" {
					t.Fatal("old probe overwrote newer response")
				}
			} else if !got.Quota.ObservedAt.Equal(before.Quota.ObservedAt) {
				t.Fatal("stale or failed quota probe replaced observation")
			}
		})
	}
}

func waitCodexQuotaPolls(t *testing.T, m *Manager) {
	t.Helper()
	m.codexQuotaPollMu.Lock()
	channels := make([]<-chan struct{}, 0, len(m.codexQuotaPollInFlight))
	for _, ch := range m.codexQuotaPollInFlight {
		channels = append(channels, ch)
	}
	m.codexQuotaPollMu.Unlock()
	for _, ch := range channels {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("quota observation did not finish")
		}
	}
}

func TestCodexQuotaSlowAccountDoesNotBlockNextRound(t *testing.T) {
	var fast, slow atomic.Int32
	seen := make(chan string, 8)
	release := make(chan struct{})
	releaseSlow := sync.OnceFunc(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("Chatgpt-Account-Id")
		if id == "slow" {
			slow.Add(1)
			seen <- id
			<-release
		} else {
			fast.Add(1)
			seen <- id
		}
		_, _ = fmt.Fprint(w, `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":10,"reset_after_seconds":2000}}}`)
	}))
	defer server.Close()
	defer releaseSlow()
	base, _ := url.Parse(server.URL)
	m := NewManager(nil, &SoonestResetFirstSelector{}, nil)
	m.RegisterExecutor(&codexResetExecutor{base: base})
	for _, id := range []string{"fast", "slow"} {
		a := resetTestAuth(id, 0, time.Time{})
		a.Metadata = map[string]any{"account_id": id, "access_token": "fixture"}
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	wait := func() {
		t.Helper()
		select {
		case <-seen:
		case <-time.After(2 * time.Second):
			t.Fatal("probe not started")
		}
	}
	m.pollCodexQuotas(context.Background())
	wait()
	wait()
	m.codexQuotaPollMu.Lock()
	fastDone := m.codexQuotaPollInFlight["fast"]
	m.codexQuotaPollMu.Unlock()
	if fastDone != nil {
		select {
		case <-fastDone:
		case <-time.After(2 * time.Second):
			t.Fatal("healthy account not published")
		}
	}
	m.pollCodexQuotas(context.Background())
	wait()
	if fast.Load() != 2 || slow.Load() != 1 {
		t.Fatalf("fast=%d slow=%d; slow account was duplicated or blocked next round", fast.Load(), slow.Load())
	}
	releaseSlow()
	waitCodexQuotaPolls(t, m)
}

func TestCodexQuotaPollRepairsFutureObservation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":10,"reset_after_seconds":2000}}}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	m := NewManager(nil, &SoonestResetFirstSelector{}, nil)
	m.RegisterExecutor(&codexResetExecutor{base: base})
	a := resetTestAuth("future-observation", 0, time.Now().Add(7*24*time.Hour))
	a.Quota.ObservedAt = time.Now().Add(time.Hour)
	a.Metadata = map[string]any{"account_id": a.ID, "access_token": "fixture"}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for _, ch := range m.pollCodexQuotas(context.Background()) {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("quota poll did not finish")
		}
	}
	got, _ := m.GetByID(a.ID)
	if got.Quota.ObservedAt.After(time.Now()) || codexSoonestReset(got, time.Now()).IsZero() {
		t.Fatal("fresh background observation could not replace invalid future snapshot")
	}
}
