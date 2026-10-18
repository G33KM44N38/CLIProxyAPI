package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCodexModelCatalogExcludePrefixConfig(t *testing.T) {
	for _, test := range []struct{ name, raw, want string }{
		{"default", "config-version: 8\n", ""},
		{"configured", "config-version: 8\nclient: {codex: {model-catalog-exclude-prefix: fleet-}}\n", "fleet-"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := ParseConfigBytes([]byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if got := parsed.Client.Codex.ModelCatalogExcludePrefix; got != test.want {
				t.Fatalf("parsed prefix = %q, want %q", got, test.want)
			}
			for _, encoding := range []struct {
				name      string
				marshal   func(any) ([]byte, error)
				unmarshal func([]byte, any) error
			}{
				{"YAML", yaml.Marshal, yaml.Unmarshal},
				{"JSON", json.Marshal, json.Unmarshal},
			} {
				body, err := encoding.marshal(parsed)
				if err != nil {
					t.Fatal(err)
				}
				var restored Config
				if err := encoding.unmarshal(body, &restored); err != nil {
					t.Fatal(err)
				}
				if got := restored.Client.Codex.ModelCatalogExcludePrefix; got != test.want {
					t.Fatalf("%s restored prefix = %q, want %q", encoding.name, got, test.want)
				}
			}
		})
	}
}
