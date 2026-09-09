package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAICompatPriorityWarmupConfig(t *testing.T) {
	for _, test := range []struct {
		name, yamlValue, jsonValue string
		enabled                    bool
	}{
		{"default", "name: compat", `{"name":"compat"}`, false},
		{"disabled", "name: compat\npriority-cache-warmup: false", `{"name":"compat","priority-cache-warmup":false}`, false},
		{"enabled", "name: compat\npriority-cache-warmup: true", `{"name":"compat","priority-cache-warmup":true}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var yamlConfig, jsonConfig OpenAICompatibility
			if err := yaml.Unmarshal([]byte(test.yamlValue), &yamlConfig); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(test.jsonValue), &jsonConfig); err != nil {
				t.Fatal(err)
			}
			if yamlConfig.PriorityCacheWarmup != test.enabled || jsonConfig.PriorityCacheWarmup != test.enabled {
				t.Fatal("YAML/JSON opt-in parsing mismatch")
			}
			encoded, err := json.Marshal(yamlConfig)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip map[string]any
			if err := json.Unmarshal(encoded, &roundTrip); err != nil {
				t.Fatal(err)
			}
			if test.enabled && roundTrip["priority-cache-warmup"] != true {
				t.Fatal("wrong serialized field name")
			}
		})
	}
}
