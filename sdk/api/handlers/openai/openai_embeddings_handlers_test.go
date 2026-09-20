package openai

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestOpenAIEmbeddingProviderRequiresOpenAICompatibleModel(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	compatibleClientID := "test-embeddings-openai-compatible"
	nativeClientID := "test-embeddings-native-provider"
	modelRegistry.RegisterClient(compatibleClientID, "custom-compatible", []*registry.ModelInfo{
		{ID: "test-embedding-compatible-model", Type: "openai-compatibility"},
	})
	modelRegistry.RegisterClient(nativeClientID, "gemini", []*registry.ModelInfo{
		{ID: "test-embedding-native-model", Type: "gemini"},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(compatibleClientID)
		modelRegistry.UnregisterClient(nativeClientID)
	})

	if got := openAIEmbeddingProvider("test-embedding-compatible-model"); got != "custom-compatible" {
		t.Fatalf("compatible provider = %q, want custom-compatible", got)
	}
	if got := openAIEmbeddingProvider("test-embedding-native-model"); got != "" {
		t.Fatalf("native provider = %q, want empty", got)
	}
}
