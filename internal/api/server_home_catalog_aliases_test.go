package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/home"
)

func TestModelsWithClientVersionHomeCatalogExcludesConfiguredPrefixAliases(t *testing.T) {
	server := newTestServer(t)
	server.cfg.Home.Enabled = true
	engine := gin.New()
	engine.GET("/v1/models", server.unifiedModelsHandler(nil, nil))
	client := newHomeCatalogClient(t, `{
		"codex":[
			{"id":"gpt-5.5","display_name":"Canonical GPT","context_length":128000},
			{"id":"fleet-account-a/gpt-5.5"},
			{"id":"fleet-account-b/gpt-5.5"},
			{"id":"fleet-account-a/alias-only"},
			{"id":"fleet-account-a/gpt-5.5-extra"},
			{"id":"vendor/gpt-5.5"},
			{"id":"not-fleet-account/gpt-5.5"},
			{"id":"fleet-standalone"}
		]
	}`)
	previousHome := home.Current()
	home.SetCurrent(client)
	t.Cleanup(func() { home.SetCurrent(previousHome) })

	request := func(target string) []byte {
		t.Helper()
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		engine.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body=%s", target, recorder.Code, recorder.Body.String())
		}
		return recorder.Body.Bytes()
	}
	decode := func(body []byte) ([]string, map[string]map[string]any) {
		t.Helper()
		var response struct {
			Models []map[string]any `json:"models"`
		}
		if errUnmarshal := json.Unmarshal(body, &response); errUnmarshal != nil {
			t.Fatalf("decode Home catalog: %v", errUnmarshal)
		}
		slugs := make([]string, 0, len(response.Models))
		entries := make(map[string]map[string]any, len(response.Models))
		for _, entry := range response.Models {
			slug, ok := entry["slug"].(string)
			if !ok {
				t.Fatalf("Home catalog entry has no string slug: %#v", entry)
			}
			slugs = append(slugs, slug)
			entries[slug] = entry
		}
		sort.Strings(slugs)
		return slugs, entries
	}
	wantDefault := []string{
		"fleet-account-a/alias-only",
		"fleet-account-a/gpt-5.5",
		"fleet-account-a/gpt-5.5-extra",
		"fleet-account-b/gpt-5.5",
		"fleet-standalone",
		"gpt-5.5",
		"not-fleet-account/gpt-5.5",
		"vendor/gpt-5.5",
	}
	wantFiltered := []string{
		"fleet-account-a/alias-only",
		"fleet-account-a/gpt-5.5-extra",
		"fleet-standalone",
		"gpt-5.5",
		"not-fleet-account/gpt-5.5",
		"vendor/gpt-5.5",
	}

	for _, version := range []string{"", "0.163.0", "cpa"} {
		t.Run("client_version="+version, func(t *testing.T) {
			if errUnmarshal := json.Unmarshal([]byte(`{"client":{"codex":{"model-catalog-exclude-prefix":""}}}`), server.cfg); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			target := "/v1/models?client_version=" + version
			defaultSlugs, original := decode(request(target))
			if !reflect.DeepEqual(defaultSlugs, wantDefault) {
				t.Fatalf("default slugs = %v, want %v", defaultSlugs, wantDefault)
			}
			ordinaryDefault := request("/v1/models")
			var ordinary struct {
				Object string `json:"object"`
				Data   []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if errUnmarshal := json.Unmarshal(ordinaryDefault, &ordinary); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			ordinaryIDs := make([]string, 0, len(ordinary.Data))
			for _, entry := range ordinary.Data {
				ordinaryIDs = append(ordinaryIDs, entry.ID)
			}
			sort.Strings(ordinaryIDs)
			if ordinary.Object != "list" || !reflect.DeepEqual(ordinaryIDs, wantDefault) {
				t.Fatalf("ordinary Home listing = %q %v, want list %v", ordinary.Object, ordinaryIDs, wantDefault)
			}
			if errUnmarshal := json.Unmarshal([]byte(`{"client":{"codex":{"model-catalog-exclude-prefix":"fleet-"}}}`), server.cfg); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			filteredSlugs, filtered := decode(request(target))
			if !reflect.DeepEqual(filteredSlugs, wantFiltered) {
				t.Fatalf("filtered slugs = %v, want %v", filteredSlugs, wantFiltered)
			}
			for _, slug := range wantFiltered {
				if !reflect.DeepEqual(filtered[slug], original[slug]) {
					t.Errorf("descriptor changed for retained slug %s", slug)
				}
			}
			if got := request("/v1/models"); string(got) != string(ordinaryDefault) {
				t.Fatalf("ordinary Home listing changed after enabling prefix exclusion: %s", got)
			}
		})
	}
}
