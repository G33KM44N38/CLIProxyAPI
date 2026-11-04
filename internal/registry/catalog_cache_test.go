package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCatalogCacheRestartHelper(t *testing.T) {
	if os.Getenv("CPA_CACHE_HELPER") != "1" {
		return
	}
	ConfigureModelCatalogCache(os.Getenv("CPA_CACHE_ROOT"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	StartModelsUpdater(ctx)
	StartCodexClientModelsUpdater(ctx)
	want := os.Getenv("CPA_CACHE_PRESENT") == "1"
	if got := LookupModelInfo("fork-restart-model") != nil; got != want {
		t.Fatalf("general catalog restored = %v, want %v", got, want)
	}
	if got := bytes.Contains(GetCodexClientModelsJSON(), []byte("fork-restart-native")); got != want {
		t.Fatalf("native catalog restored = %v, want %v", got, want)
	}
}

func cacheRestart(t *testing.T, root string, present bool) {
	t.Helper()
	value := "0"
	if present {
		value = "1"
	}
	command := exec.Command(os.Args[0], "-test.run=^TestCatalogCacheRestartHelper$")
	command.Env = append(os.Environ(), "CPA_CACHE_HELPER=1", "CPA_CACHE_ROOT="+root, "CPA_CACHE_PRESENT="+value)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh process did not recover catalogs: %v\n%s", err, output)
	}
}

func seedDiskCatalogs(t *testing.T, root string) map[string][]byte {
	t.Helper()
	previous := modelCatalogDiskCache.Load()
	t.Cleanup(func() { modelCatalogDiskCache.Store(previous) })
	ConfigureModelCatalogCache(root)
	var general staticModelsJSON
	if err := json.Unmarshal(embeddedModelsJSON, &general); err != nil {
		t.Fatal(err)
	}
	model := *general.CodexPro[0]
	model.ID = "fork-restart-model"
	general.CodexPro = append(general.CodexPro, &model)
	generalData, err := json.Marshal(general)
	if err != nil {
		t.Fatal(err)
	}
	var native codexClientModelsPayload
	if err := json.Unmarshal(embeddedCodexClientModelsJSON, &native); err != nil {
		t.Fatal(err)
	}
	copy := make(map[string]any)
	for key, value := range native.Models[0] {
		copy[key] = value
	}
	copy["slug"] = "fork-restart-native"
	native.Models = append(native.Models, copy)
	nativeData, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"models.json": generalData, "codex_client_models.json": nativeData}
	for name, data := range files {
		if err := saveCachedCatalog(name, data); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("cache permissions are not private: %v", err)
		}
	}
	return files
}

func TestCatalogCacheSurvivesRestartWithoutNetwork(t *testing.T) {
	root := t.TempDir()
	seedDiskCatalogs(t, root)
	cacheRestart(t, root, true)
	cacheRestart(t, t.TempDir(), false)
}

func TestCatalogCacheFailedRefreshKeepsLastValidatedBytes(t *testing.T) {
	root := t.TempDir()
	files := seedDiskCatalogs(t, root)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer server.Close()
	oldModels, oldNative := modelsURLs, codexClientModelsURLs
	modelsURLs, codexClientModelsURLs = []string{server.URL}, []string{server.URL}
	t.Cleanup(func() { modelsURLs, codexClientModelsURLs = oldModels, oldNative })
	tryRefreshModels(context.Background(), "cache test")
	tryRefreshCodexClientModels(context.Background(), "cache test")
	for name, original := range files {
		if err := saveCachedCatalog(name, []byte(`{}`)); err == nil {
			t.Fatal("invalid catalog accepted by disk cache")
		}
		actual, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !bytes.Equal(actual, original) {
			t.Fatalf("failed refresh replaced last valid %s", name)
		}
	}
	cacheRestart(t, root, true)
}

func TestCatalogCacheCorruptionFallsBackToEmbeddedModels(t *testing.T) {
	root := t.TempDir()
	seedDiskCatalogs(t, root)
	for _, name := range []string{"models.json", "codex_client_models.json"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(`{"broken":`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cacheRestart(t, root, false)
}
