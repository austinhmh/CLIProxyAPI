package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudePromptCacheSelectedPlanRespectsBreakpointBudget(t *testing.T) {
	runtime := NewClaudePromptCacheRuntime()
	payload := []byte(`{
		"tools":[{"name":"Read","input_schema":{"type":"object"}}],
		"system":[{"type":"text","text":"system prompt"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"first"}]},
			{"role":"assistant","content":[{"type":"text","text":"reply"}]},
			{"role":"user","content":[{"type":"text","text":"second"}]}
		]
	}`)

	plannedPayload, plan := runtime.PlanClaudePromptCache("selected-cache-scope", payload, ClaudePromptCacheCapabilities{
		AutomaticHistory: true,
		ExplicitHistory:  true,
	})
	if plan == nil || !plan.Summary.AutomaticHistory {
		t.Fatalf("missing automatic cache plan: %+v", plan)
	}
	explicitBreakpoints, invalidPaths := collectClaudeCacheBreakpoints(plannedPayload)
	if len(invalidPaths) != 0 || len(explicitBreakpoints)+1 > ClaudePromptCacheMaxBreakpoints {
		t.Fatalf("invalid cache breakpoint layout: explicit=%d invalid=%v", len(explicitBreakpoints), invalidPaths)
	}
	if !gjson.GetBytes(plannedPayload, "cache_control").Exists() {
		t.Fatal("official Anthropic cache plan omitted automatic history")
	}
	if gjson.GetBytes(plannedPayload, "diagnostics").Exists() {
		t.Fatal("cache planner unexpectedly added diagnostics")
	}
}
