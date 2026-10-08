package auth

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestBuildUsageProbeRequestSelectsOnlySafeProviderModel(t *testing.T) {
	testCases := []struct {
		name          string
		provider      string
		modelIDs      []string
		expectedModel string
		expectedOK    bool
	}{
		{
			name:          "claude selects haiku after other models",
			provider:      "claude",
			modelIDs:      []string{"deepseek-v4-flash", "claude-haiku-4-5"},
			expectedModel: "claude-haiku-4-5",
			expectedOK:    true,
		},
		{
			name:       "claude rejects models without haiku",
			provider:   "claude",
			modelIDs:   []string{"deepseek-v4-flash", "MiniMax-M3"},
			expectedOK: false,
		},
		{
			name:          "codex selects luna after other models",
			provider:      "codex",
			modelIDs:      []string{"gpt-5.6-sol", "gpt-5.6-luna"},
			expectedModel: "gpt-5.6-luna",
			expectedOK:    true,
		},
		{
			name:       "codex rejects models without luna",
			provider:   "codex",
			modelIDs:   []string{"gpt-5.6-sol", "gpt-5.6-codex"},
			expectedOK: false,
		},
	}

	modelRegistry := registry.GetGlobalRegistry()
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			clientID := "usage-refresh-test-" + strings.ReplaceAll(testCase.name, " ", "-")
			models := make([]*registry.ModelInfo, 0, len(testCase.modelIDs))
			for _, modelID := range testCase.modelIDs {
				models = append(models, &registry.ModelInfo{ID: modelID})
			}

			modelRegistry.RegisterClient(clientID, testCase.provider, models)
			t.Cleanup(func() {
				modelRegistry.UnregisterClient(clientID)
			})

			request, _, ok := buildUsageProbeRequest(&Auth{
				ID:       clientID,
				Provider: testCase.provider,
			})
			if ok != testCase.expectedOK {
				t.Fatalf("buildUsageProbeRequest() ok = %t, want %t", ok, testCase.expectedOK)
			}
			if request.Model != testCase.expectedModel {
				t.Fatalf("buildUsageProbeRequest() model = %q, want %q", request.Model, testCase.expectedModel)
			}
		})
	}
}

func TestPickProbeModelReturnsEmptyWithoutPreferredModel(t *testing.T) {
	testCases := []struct {
		name      string
		models    []*registry.ModelInfo
		preferred string
		expected  string
	}{
		{
			name:      "skips nil and empty entries before match",
			models:    []*registry.ModelInfo{nil, {}, {ID: "Claude-HAIKU-4-5"}},
			preferred: "haiku",
			expected:  "Claude-HAIKU-4-5",
		},
		{
			name:      "rejects valid models without preferred substring",
			models:    []*registry.ModelInfo{{ID: "deepseek-v4-flash"}, {ID: "MiniMax-M3"}},
			preferred: "haiku",
			expected:  "",
		},
		{
			name:      "returns empty for invalid entries",
			models:    []*registry.ModelInfo{nil, {}},
			preferred: "luna",
			expected:  "",
		},
		{
			name:      "returns empty for nil list",
			models:    nil,
			preferred: "haiku",
			expected:  "",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			actual := pickProbeModel(testCase.models, testCase.preferred)
			if actual != testCase.expected {
				t.Fatalf("pickProbeModel() = %q, want %q", actual, testCase.expected)
			}
		})
	}
}
