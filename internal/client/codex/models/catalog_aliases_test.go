package models

import (
	"reflect"
	"testing"
)

func TestWithoutPrefixAliases(t *testing.T) {
	entries := []map[string]any{
		{"slug": "gpt-6.1-sol", "base_instructions": "canonical prompt", "supported_reasoning_levels": []string{"high"}},
		{"slug": "fleet-first/gpt-6.1-sol", "base_instructions": "account prompt"},
		{"slug": "fleet-second/gpt-6.1-sol"},
		{"slug": "vendor/gpt-6.1-sol"},
		{"slug": "fleet-first/alias-only"},
		{"slug": "fleet-public-model"},
	}
	for _, test := range []struct {
		name   string
		prefix string
		want   []string
	}{
		{"default", "", []string{"gpt-6.1-sol", "fleet-first/gpt-6.1-sol", "fleet-second/gpt-6.1-sol", "vendor/gpt-6.1-sol", "fleet-first/alias-only", "fleet-public-model"}},
		{"configured", "fleet-", []string{"gpt-6.1-sol", "vendor/gpt-6.1-sol", "fleet-first/alias-only", "fleet-public-model"}},
		{"specific-prefix", "fleet-first", []string{"gpt-6.1-sol", "fleet-second/gpt-6.1-sol", "vendor/gpt-6.1-sol", "fleet-first/alias-only", "fleet-public-model"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := make([]map[string]any, len(entries))
			for index, entry := range entries {
				before[index] = cloneCodexClientModelMap(entry)
			}
			actual := WithoutPrefixAliases(entries, test.prefix)
			var slugs []string
			for _, entry := range actual {
				slugs = append(slugs, entry["slug"].(string))
			}
			if !reflect.DeepEqual(slugs, test.want) {
				t.Fatalf("slugs = %v, want %v", slugs, test.want)
			}
			if !reflect.DeepEqual(entries, before) {
				t.Fatal("input descriptors were modified")
			}
			if !reflect.DeepEqual(actual[0], entries[0]) {
				t.Fatal("canonical descriptor changed")
			}
		})
	}
}
