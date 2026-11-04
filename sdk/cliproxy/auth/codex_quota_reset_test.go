package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type codexResetFixture struct {
	mu               sync.Mutex
	exhausted        map[string]bool
	credits          int
	consumes         int
	calls            []string
	consumeStatus    int
	keepExhausted    bool
	failConfirmation bool
	redemptionIDs    []string
}

func (f *codexResetFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := r.Header.Get("Chatgpt-Account-Id")
	f.calls = append(f.calls, id+":"+r.Method+":"+r.URL.Path)
	switch r.URL.Path {
	case "/responses":
		if f.exhausted[id] {
			w.WriteHeader(429)
			return
		}
		_, _ = io.WriteString(w, `{"output":"recovered"}`)
	case "/backend-api/wham/usage":
		if f.failConfirmation && f.consumes > 0 {
			f.failConfirmation = false
			w.WriteHeader(500)
			return
		}
		_, _ = fmt.Fprintf(w, `{"rate_limit":{"allowed":%t,"limit_reached":%t}}`, !f.exhausted[id], f.exhausted[id])
	case "/backend-api/wham/rate-limit-reset-credits":
		_, _ = fmt.Fprintf(w, `{"available_count":%d,"applicable_available_count":%d}`, f.credits, f.credits)
	case "/backend-api/wham/rate-limit-reset-credits/consume":
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["redeem_request_id"] == "" {
			w.WriteHeader(400)
			return
		}
		f.consumes++
		for _, previous := range f.redemptionIDs {
			if previous == body["redeem_request_id"] {
				_, _ = io.WriteString(w, `{}`)
				return
			}
		}
		f.redemptionIDs = append(f.redemptionIDs, body["redeem_request_id"])
		if f.consumeStatus != 0 {
			w.WriteHeader(f.consumeStatus)
			return
		}
		if !f.keepExhausted {
			f.exhausted[id] = false
		}
		_, _ = io.WriteString(w, `{}`)
	default:
		w.WriteHeader(404)
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

func newCodexResetFixture(t *testing.T, f *codexResetFixture, enabled bool, ready bool) *Manager {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	base, _ := url.Parse(server.URL)
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.RegisterExecutor(&codexResetExecutor{base: base})
	m.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{AutoResetQuota: enabled}})
	m.SetRetryConfig(0, 0, 0)
	for _, id := range []string{"reset-a", "reset-b"} {
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "gpt-reset-test"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		a := &Auth{ID: id, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "fixture", "account_id": id}}
		if !ready {
			a.Unavailable = true
			a.Quota = QuotaState{Exceeded: true, NextRecoverAt: time.Now().Add(time.Hour)}
			a.NextRetryAfter = a.Quota.NextRecoverAt
		}
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func TestCodexAutoResetRetriesGeneration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			f := &codexResetFixture{exhausted: map[string]bool{"reset-a": true, "reset-b": true}, credits: 3}
			m := newCodexResetFixture(t, f, true, true)
			req := cliproxyexecutor.Request{Model: "gpt-reset-test"}
			var payload []byte
			if stream {
				result, err := m.ExecuteStream(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{Stream: true})
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					payload = append(payload, chunk.Payload...)
				}
			} else {
				result, err := m.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
				if err != nil {
					t.Fatal(err)
				}
				payload = result.Payload
			}
			if !strings.Contains(string(payload), "recovered") || f.consumes != 1 {
				t.Fatalf("payload=%s consumes=%d calls=%v", payload, f.consumes, f.calls)
			}
			if len(f.calls) < 2 || !strings.HasSuffix(f.calls[0], ":POST:/responses") || !strings.HasSuffix(f.calls[1], ":POST:/responses") {
				t.Fatalf("must try normal quotas first: %v", f.calls)
			}
		})
	}
}

func TestCodexAutoResetPrefersRemainingQuota(t *testing.T) {
	f := &codexResetFixture{exhausted: map[string]bool{"reset-a": true, "reset-b": false}, credits: 3}
	m := newCodexResetFixture(t, f, true, true)
	resp, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-reset-test"}, cliproxyexecutor.Options{})
	if err != nil || !strings.Contains(string(resp.Payload), "recovered") || f.consumes != 0 || len(f.calls) != 2 {
		t.Fatalf("response=%s err=%v consumes=%d calls=%v", resp.Payload, err, f.consumes, f.calls)
	}
}

func TestCodexAutoResetDoesNotSpendWithoutRecovery(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		enabled                       bool
		credits, status, wantConsumes int
		keep                          bool
	}{
		{name: "disabled", credits: 3},
		{name: "no credits", enabled: true},
		{name: "redemption failure", enabled: true, credits: 3, status: 500, wantConsumes: 1},
		{name: "quota still exhausted", enabled: true, credits: 3, wantConsumes: 1, keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &codexResetFixture{exhausted: map[string]bool{"reset-a": true, "reset-b": true}, credits: tc.credits, consumeStatus: tc.status, keepExhausted: tc.keep}
			m := newCodexResetFixture(t, f, tc.enabled, false)
			for range 2 {
				_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-reset-test"}, cliproxyexecutor.Options{})
				if err == nil {
					t.Fatal("expected original quota failure")
				}
			}
			if f.consumes != tc.wantConsumes {
				t.Fatalf("consumes=%d want=%d", f.consumes, tc.wantConsumes)
			}
		})
	}
}

func TestCodexAutoResetConcurrentRequestsSpendOneCredit(t *testing.T) {
	f := &codexResetFixture{exhausted: map[string]bool{"reset-a": true, "reset-b": true}, credits: 3}
	m := newCodexResetFixture(t, f, true, false)
	var wg sync.WaitGroup
	errors := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-reset-test"}, cliproxyexecutor.Options{})
			errors <- err
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("concurrent request did not recover: %v", err)
		}
	}
	if f.consumes != 1 {
		t.Fatalf("consumes=%d want=1", f.consumes)
	}
}

func TestCodexAutoResetPinnedAccountPreservesOtherQuota(t *testing.T) {
	f := &codexResetFixture{exhausted: map[string]bool{"reset-a": true, "reset-b": false}, credits: 3}
	m := newCodexResetFixture(t, f, true, true)
	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-reset-test"}, cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "reset-a"}})
	if err == nil || f.consumes != 0 || len(f.calls) != 1 {
		t.Fatalf("err=%v consumes=%d calls=%v", err, f.consumes, f.calls)
	}
}

func TestCodexAutoResetNaturalRenewalUsesNoCredit(t *testing.T) {
	f := &codexResetFixture{exhausted: map[string]bool{"reset-a": false, "reset-b": true}, credits: 3}
	m := newCodexResetFixture(t, f, true, false)
	resp, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-reset-test"}, cliproxyexecutor.Options{})
	if err != nil || !strings.Contains(string(resp.Payload), "recovered") || f.consumes != 0 {
		t.Fatalf("err=%v consumes=%d calls=%v", err, f.consumes, f.calls)
	}
}

func TestCodexAutoResetRetiresRedemptionAfterDelayedConfirmation(t *testing.T) {
	f := &codexResetFixture{exhausted: map[string]bool{"reset-a": true, "reset-b": true}, credits: 3, failConfirmation: true}
	m := newCodexResetFixture(t, f, true, false)
	run := func() error {
		_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-reset-test"}, cliproxyexecutor.Options{})
		return err
	}
	if err := run(); err == nil {
		t.Fatal("unconfirmed redemption must preserve cooldowns")
	}
	if f.consumes != 1 {
		t.Fatalf("consumes=%d want=1", f.consumes)
	}
	m.codexResetRetryAfter = time.Time{}
	if err := run(); err != nil {
		t.Fatalf("delayed confirmation did not recover: %v", err)
	}
	if f.consumes != 1 {
		t.Fatalf("recovery spent another credit: %d", f.consumes)
	}
	f.exhausted["reset-a"] = true
	m.codexResetBackoff["reset-a"] = time.Time{}
	if err := run(); err != nil {
		t.Fatalf("new exhaustion did not recover: %v", err)
	}
	if f.consumes != 2 || len(f.redemptionIDs) != 2 || f.redemptionIDs[0] == f.redemptionIDs[1] {
		t.Fatalf("redemption IDs must differ for distinct quota cycles: %v", f.redemptionIDs)
	}
}
