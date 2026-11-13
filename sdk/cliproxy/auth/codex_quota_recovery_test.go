package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestCodexQuotaPollRecoversManualReset(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		recover    bool
	}{
		{"reset", `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"reset_after_seconds":600}}}`, true},
		{"exhausted", `{"rate_limit":{"allowed":false,"limit_reached":true}}`, false},
		{"contradictory", `{"rate_limit":{"allowed":true,"limit_reached":false,"secondary_window":{"used_percent":100,"reset_after_seconds":600}}}`, false},
		{"missing", `{"rate_limit":{"allowed":true}}`, false},
		{"missing-reset", `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			m := NewManager(nil, &FillFirstSelector{}, nil)
			m.RegisterExecutor(&codexResetExecutor{base: base})
			a := &Auth{ID: "manual-reset", Provider: "codex", Status: StatusActive}
			a.Metadata = map[string]any{"access_token": "fixture", "account_id": "manual-reset"}
			if _, err := m.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: "codex", Model: "gpt-test", CredentialScope: false, Error: &Error{HTTPStatus: 429, Message: `{"error":{"type":"usage_limit_reached","limit_window_minutes":10080}}`}})
			before, _ := m.GetByID(a.ID)
			if !before.ModelStates["gpt-test"].Quota.Exceeded {
				t.Fatal("quota fixture was not blocked")
			}
			for _, done := range m.pollCodexQuotas(context.Background()) {
				<-done
			}
			after, _ := m.GetByID(a.ID)
			if got := after.ModelStates["gpt-test"].Quota.Exceeded; got == tc.recover {
				t.Fatalf("quota exceeded=%v, want %v", got, !tc.recover)
			}
			if tc.recover && (after.Unavailable || after.ModelStates["gpt-test"].Unavailable) {
				t.Fatal("recovered account remains unavailable")
			}
		})
	}
}

func TestCodexQuotaPollPreservesNewerAndUnrelatedFailures(t *testing.T) {
	for _, scenario := range []string{"newer-quota", "mixed-transient", "mixed-unauthorized", "model-only", "challenge"} {
		t.Run(scenario, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				fmt.Fprint(w, `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"reset_after_seconds":600}}}`)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			m := NewManager(nil, &FillFirstSelector{}, nil)
			m.RegisterExecutor(&codexResetExecutor{base: base})
			a := &Auth{ID: "preserved-" + scenario, Provider: "codex", Status: StatusActive}
			a.Metadata = map[string]any{"access_token": "fixture", "account_id": a.ID}
			if _, err := m.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			quota := Result{AuthID: a.ID, Provider: "codex", Model: "gpt-test", CredentialScope: scenario != "model-only", Error: &Error{HTTPStatus: 429, Message: `{"error":{"type":"usage_limit_reached","limit_window_minutes":10080}}`}}
			if scenario == "model-only" {
				quota.Error.Message = `{"error":{"type":"usage_limit_reached","limit_name":"spark","limit_window_minutes":300}}`
			}
			m.MarkResult(context.Background(), quota)
			if scenario == "mixed-transient" {
				code := 503
				if scenario == "mixed-unauthorized" {
					code = 401
				}
				m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: "codex", Model: "other", Error: &Error{HTTPStatus: code, Message: "unrelated failure"}})
			}
			if scenario == "challenge" {
				m.mu.Lock()
				m.auths[a.ID].ModelStates["gpt-test"].Quota.Reason = "cloudflare challenge"
				m.mu.Unlock()
			}
			done := m.pollCodexQuotas(context.Background())
			if len(done) > 0 {
				<-entered
			}
			if scenario == "mixed-unauthorized" {
				m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: "codex", Model: "other", Error: &Error{HTTPStatus: 401, Message: "unauthorized"}})
			}
			if scenario == "newer-quota" {
				m.MarkResult(context.Background(), quota)
			}
			before, _ := m.GetByID(a.ID)
			close(release)
			for _, c := range done {
				<-c
			}
			after, _ := m.GetByID(a.ID)
			if !after.ModelStates["gpt-test"].Quota.Exceeded || !after.ModelStates["gpt-test"].NextRetryAfter.Equal(before.ModelStates["gpt-test"].NextRetryAfter) {
				t.Fatal("probe cleared an unproven or newer failure")
			}
			if after.Generation != before.Generation {
				t.Fatalf("probe changed failure generation %d to %d", before.Generation, after.Generation)
			}
			if scenario == "mixed-transient" || scenario == "mixed-unauthorized" {
				if after.ModelStates["other"].LastError.HTTPStatus != before.ModelStates["other"].LastError.HTTPStatus {
					t.Fatal("unrelated error was changed")
				}
			}
		})
	}
}

type codexResetExecutor struct{ base *url.URL }

func (*codexResetExecutor) Identifier() string                                { return "codex" }
func (*codexResetExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) { return a, nil }
func (e *codexResetExecutor) HttpRequest(ctx context.Context, a *Auth, req *http.Request) (*http.Response, error) {
	req = req.Clone(ctx)
	req.URL.Scheme, req.URL.Host = e.base.Scheme, e.base.Host
	return http.DefaultClient.Do(req)
}
func (e *codexResetExecutor) Execute(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", e.base.String()+"/responses", nil)
	req.Header.Set("Chatgpt-Account-Id", a.ID)
	resp, err := e.HttpRequest(ctx, a, req)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == 429 {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: 429, Code: "usage_limit_reached", Message: "quota exhausted"}
	}
	body, err := io.ReadAll(resp.Body)
	return cliproxyexecutor.Response{Payload: body}, err
}
func (e *codexResetExecutor) ExecuteStream(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	resp, err := e.Execute(ctx, a, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: resp.Payload}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}
func (e *codexResetExecutor) CountTokens(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, a, req, opts)
}

func TestCodexQuotaRecoveryProbeLifecycle(t *testing.T) {
	for _, scenario := range []string{"recover", "cancel", "home"} {
		t.Run(scenario, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/backend-api/wham/usage" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				calls.Add(1)
				close(entered)
				<-release
				fmt.Fprint(w, `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"reset_after_seconds":600}}}`)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			m := NewManager(nil, &FillFirstSelector{}, nil)
			m.RegisterExecutor(&codexResetExecutor{base: base})
			for _, id := range []string{"healthy", "blocked"} {
				a := &Auth{ID: id, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "fixture", "account_id": id}}
				if _, err := m.Register(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			m.MarkResult(context.Background(), Result{AuthID: "blocked", Provider: "codex", Model: "gpt-test", Error: &Error{HTTPStatus: 429, Message: `{"error":{"type":"usage_limit_reached","limit_window_minutes":10080}}`}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m.StartAutoRefresh(ctx, time.Hour)
			defer m.StopAutoRefresh()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("refresh lifecycle did not start recovery probe")
			}
			done := m.pollCodexQuotas(ctx)
			if len(done) != 1 {
				close(release)
				t.Fatalf("in-flight probes=%d want1", len(done))
			}
			if scenario == "cancel" {
				cancel()
			}
			if scenario == "home" {
				cfg := &internalconfig.Config{}
				cfg.Home.Enabled = true
				m.runtimeConfig.Store(cfg)
			}
			close(release)
			for _, c := range done {
				select {
				case <-c:
				case <-time.After(3 * time.Second):
					t.Fatal("probe failed to complete")
				}
			}
			after, _ := m.GetByID("blocked")
			if got := after.ModelStates["gpt-test"].Quota.Exceeded; got != (scenario != "recover") {
				t.Fatalf("quota exceeded=%v scenario=%s", got, scenario)
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream GETs=%d want1", calls.Load())
			}
			healthy, _ := m.GetByID("healthy")
			if healthy.Status != StatusActive || healthy.Unavailable {
				t.Fatal("healthy credential changed")
			}
		})
	}
}
