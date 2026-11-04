package models

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestForkCatalogDoesNotCloneNativePromptsOntoSyntheticModels(t *testing.T) {
	canonical := BuildResponseForClient([]map[string]any{{"id": "gpt-5.5"}}, nil, false, "0.165.0")["models"].([]map[string]any)[0]
	available := []map[string]any{{"id": "gpt-5.5"}}
	for i := 0; i < 60; i++ {
		available = append(available, map[string]any{"id": fmt.Sprintf("external-model-%02d", i), "context_length": 123456})
	}
	payload := BuildResponseForClient(available, nil, false, "0.165.0")
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= 1<<20 {
		t.Fatalf("native catalog is %d bytes, exceeds Codex's 1 MiB limit", len(encoded))
	}
	entries := payload["models"].([]map[string]any)
	if len(entries) != 61 {
		t.Fatalf("catalog lost models: got %d, want 61", len(entries))
	}
	for _, entry := range entries {
		if entry["slug"] == "gpt-5.5" {
			if !reflect.DeepEqual(entry, canonical) {
				t.Fatal("native descriptor or instructions changed")
			}
			continue
		}
		if entry["context_window"] != 123456 {
			t.Fatalf("synthetic model capabilities changed: %v", entry["context_window"])
		}
		for _, field := range []string{"apply_patch_tool_type", "upgrade", "availability_nux"} {
			value, exists := entry[field]
			if !exists || value != nil {
				t.Fatalf("required optional field %s must be present as null", field)
			}
		}
	}
}
