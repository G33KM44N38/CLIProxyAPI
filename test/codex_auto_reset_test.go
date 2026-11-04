package test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type codexResetWireTransport struct{ base *url.URL }

func (tr codexResetWireTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = tr.base.Scheme, tr.base.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestCodexAutoResetWithRealExecutor(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var mu sync.Mutex
			recovered := ""
			consumes, generations := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				id := r.Header.Get("Chatgpt-Account-Id")
				if id == "" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Errorf("missing account identity or OAuth credentials")
					w.WriteHeader(401)
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/responses"):
					generations++
					if recovered != id {
						w.WriteHeader(429)
						_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"quota exhausted","resets_in_seconds":3600}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"reset-proof\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
				case strings.HasSuffix(r.URL.Path, "/usage"):
					_, _ = fmt.Fprintf(w, `{"rate_limit":{"allowed":%t,"limit_reached":%t}}`, recovered == id, recovered != id)
				case strings.HasSuffix(r.URL.Path, "/rate-limit-reset-credits"):
					_, _ = io.WriteString(w, `{"available_count":2}`)
				case strings.HasSuffix(r.URL.Path, "/rate-limit-reset-credits/consume"):
					consumes++
					if generations != 2 {
						t.Errorf("spent a credit before trying both quotas: %d", generations)
					}
					recovered = id
					_, _ = io.WriteString(w, `{}`)
				default:
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", codexResetWireTransport{base: base})
			cfg := &config.Config{Codex: config.CodexConfig{AutoResetQuota: true, StreamBootstrapBuffering: true, ModelLevelCooling: true}}
			manager := cliproxyauth.NewManager(nil, &cliproxyauth.FillFirstSelector{}, nil)
			manager.SetConfig(cfg)
			manager.SetRetryConfig(0, 0, 0)
			manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))
			const model = "gpt-5.4"
			for _, id := range []string{"wire-reset-a", "wire-reset-b"} {
				registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				_, err := manager.Register(ctx, &cliproxyauth.Auth{ID: id, Provider: "codex", Status: cliproxyauth.StatusActive, Metadata: map[string]any{"access_token": "fixture-token", "account_id": id}})
				if err != nil {
					t.Fatal(err)
				}
			}
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`)}
			opts := cliproxyexecutor.Options{Stream: stream, SourceFormat: sdktranslator.FromString("openai-response")}
			var payload []byte
			if stream {
				result, err := manager.ExecuteStream(ctx, []string{"codex"}, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					payload = append(payload, chunk.Payload...)
				}
			} else {
				result, err := manager.Execute(ctx, []string{"codex"}, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				payload = result.Payload
			}
			mu.Lock()
			defer mu.Unlock()
			if consumes != 1 || generations != 3 || !strings.Contains(string(payload), "reset-proof") {
				t.Fatalf("consumes=%d generations=%d response=%s", consumes, generations, payload)
			}
		})
	}
}
