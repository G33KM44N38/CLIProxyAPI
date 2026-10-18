package openai

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func TestOpenAIModelsCodexCatalogExcludesConfiguredPrefixAliases(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	publicIDs := []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "codex-auto-review", "gpt-6-luna", "gpt-6-sol", "gpt-6.1-sol", "gpt-image-1.5", "gpt-image-2", "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"}
	registered := make([]*registry.ModelInfo, 0, len(publicIDs)*4)
	for _, id := range publicIDs {
		registered = append(registered, &registry.ModelInfo{ID: id, Object: "model", OwnedBy: "openai", Type: "codex"})
		for account := 0; account < 3; account++ {
			registered = append(registered, &registry.ModelInfo{ID: fmt.Sprintf("fleet-%032x/%s", account, id), Object: "model", OwnedBy: "openai", Type: "codex"})
		}
	}
	modelRegistry.RegisterClient(t.Name(), "codex", registered)
	t.Cleanup(func() { modelRegistry.UnregisterClient(t.Name()) })
	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil)
	handler := NewOpenAIAPIHandler(base)
	request := func(target string) []byte {
		t.Helper()
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest("GET", target, nil)
		handler.OpenAIModels(context)
		if recorder.Code != 200 {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		return recorder.Body.Bytes()
	}
	decode := func(body []byte) map[string]map[string]any {
		t.Helper()
		var payload struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		bySlug := make(map[string]map[string]any)
		for _, model := range payload.Models {
			bySlug[model["slug"].(string)] = model
		}
		return bySlug
	}
	baseline := request("/v1/models?client_version=0.163.0")
	if len(baseline) <= 1<<20 {
		t.Fatalf("fixture = %d bytes, want oversized catalog", len(baseline))
	}
	original := decode(baseline)
	if len(original) != 52 {
		t.Fatalf("default models = %d, want 52", len(original))
	}
	openAI := request("/v1/models")
	updated, err := config.ParseConfigBytes([]byte("client: {codex: {model-catalog-exclude-prefix: fleet-}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	base.UpdateClients(&updated.SDKConfig)
	filtered := request("/v1/models?client_version=0.163.0")
	if len(filtered) > 1<<20 {
		t.Fatalf("filtered catalog = %d bytes, want <= 1048576", len(filtered))
	}
	actual := decode(filtered)
	if len(actual) != 13 {
		t.Fatalf("filtered models = %d, want 13", len(actual))
	}
	for _, id := range publicIDs {
		if !reflect.DeepEqual(actual[id], original[id]) {
			t.Errorf("descriptor changed for %s", id)
		}
	}
	if got := request("/v1/models"); string(got) != string(openAI) {
		t.Fatal("ordinary OpenAI discovery changed")
	}
	if got := len(modelRegistry.GetAvailableModels("openai")); got != 52 {
		t.Fatalf("registered routes = %d, want 52", got)
	}
	base.UpdateClients(&config.SDKConfig{})
	if got := request("/v1/models?client_version=0.163.0"); string(got) != string(baseline) {
		t.Fatal("clearing prefix exclusion did not restore the original native catalog")
	}
}
