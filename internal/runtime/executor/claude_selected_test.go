package executor

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/tidwall/gjson"
)

func TestSelectedClaudePromptCacheRuntimeSurvivesExecutorReplacement(t *testing.T) {
	runtime := NewClaudePromptCacheRuntime()
	configuration := &config.Config{ClaudePromptCache: config.ClaudePromptCacheConfig{Mode: config.ClaudePromptCacheModeAdaptive}}
	firstExecutor := NewClaudeExecutorWithPromptCacheRuntime(configuration, runtime)
	replacementExecutor := NewClaudeExecutorWithPromptCacheRuntime(configuration, runtime)
	if firstExecutor.promptCacheRuntime != runtime || replacementExecutor.promptCacheRuntime != runtime {
		t.Fatal("Claude executor replacement did not reuse service-owned cache runtime")
	}
	if firstExecutor.oauthToolAliases == nil || replacementExecutor.oauthToolAliases == nil {
		t.Fatal("Claude executor replacement lost the upstream OAuth tool alias store")
	}
	if gotMode := replacementExecutor.claudePromptCacheMode(); gotMode != config.ClaudePromptCacheModeAdaptive {
		t.Fatalf("cache mode = %q, want adaptive", gotMode)
	}
	if gotMode := NewClaudeExecutor(&config.Config{}).claudePromptCacheMode(); gotMode != config.ClaudePromptCacheModeLegacy {
		t.Fatalf("default cache mode = %q, want legacy", gotMode)
	}
}

func TestSelectedClaudeHistoryCompactionRaisesOnlyExplicitSmallLimit(t *testing.T) {
	registryRef := registry.GetGlobalRegistry()
	const clientID = "selected-claude-history-compaction-client"
	const modelID = "selected-claude-history-compaction-model"
	registryRef.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:                  modelID,
		Type:                "claude",
		OwnedBy:             "anthropic",
		Object:              "model",
		Created:             time.Now().Unix(),
		MaxCompletionTokens: 128000,
		UserDefined:         true,
	}})
	t.Cleanup(func() { registryRef.UnregisterClient(clientID) })

	for _, testCase := range []struct {
		name       string
		payload    string
		wantTokens int64
	}{
		{name: "history compaction", payload: `{"max_tokens":8192,"messages":[{"role":"user","content":"<conversation_transcript>history</conversation_transcript>"}]}`, wantTokens: 128000},
		{name: "ordinary request", payload: `{"max_tokens":8192,"messages":[{"role":"user","content":"hello"}]}`, wantTokens: 8192},
		{name: "already sufficient", payload: `{"max_tokens":128000,"messages":[{"role":"user","content":"<conversation_transcript>history</conversation_transcript>"}]}`, wantTokens: 128000},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			updatedPayload := raiseMaxTokensForConversationCompaction([]byte(testCase.payload), modelID)
			if gotTokens := gjson.GetBytes(updatedPayload, "max_tokens").Int(); gotTokens != testCase.wantTokens {
				t.Fatalf("max_tokens = %d, want %d", gotTokens, testCase.wantTokens)
			}
		})
	}
}
