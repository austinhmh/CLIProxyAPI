package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestReconcileClaudeCloakDisplayAfterThinkingOverride(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5", "claude-fable-5-1"} {
		t.Run(model, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				thinking string
				injected bool
				touched  bool
				want     string
			}{
				{name: "disabled removes synthetic display", thinking: `{"type":"disabled","display":"updates"}`, injected: true},
				{name: "missing type removes synthetic display", thinking: `{"display":"updates"}`, injected: true},
				{name: "adaptive keeps synthetic display", thinking: `{"type":"adaptive","display":"updates"}`, injected: true, want: "updates"},
				{name: "enabled keeps synthetic display", thinking: `{"type":"enabled","budget_tokens":2048,"display":"updates"}`, injected: true, want: "updates"},
				{name: "caller display stays caller owned", thinking: `{"type":"disabled","display":"summarized"}`, want: "summarized"},
				{name: "payload display stays operator owned", thinking: `{"type":"disabled","display":"omitted"}`, injected: true, touched: true, want: "omitted"},
				{name: "payload deletion is not refilled", thinking: `{"type":"adaptive"}`, injected: true, touched: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body, _ := sjson.SetBytes([]byte(`{}`), "model", model)
					body, _ = sjson.SetRawBytes(body, "thinking", []byte(tc.thinking))
					out := reconcileClaudeCodeFableModelAfterPayload(body, claudeCodeFableState{injectedDisplay: tc.injected}, false, tc.touched, true, false)
					display := gjson.GetBytes(out, "thinking.display")
					if display.String() != tc.want || (tc.want == "" && display.Exists()) {
						t.Fatalf("display=%s, want %q; body=%s", display.Raw, tc.want, out)
					}
				})
			}
		})
	}
}

func TestReconcileClaudeCloakDisplayRemovesSyntheticDisplayForNonProgressModel(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","thinking":{"type":"adaptive","display":"updates"}}`)
	out := reconcileClaudeCodeFableModelAfterPayload(body, claudeCodeFableState{injectedDisplay: true}, false, false, true, false)
	if display := gjson.GetBytes(out, "thinking.display"); display.Exists() {
		t.Fatalf("synthetic display survived for non-progress model: %s", out)
	}
}

func TestClaudeExecutorThinkingDisplayUpstreamDefaults(test *testing.T) {
	for _, model := range []string{"claude-sonnet-5-5", "claude-opus-5-5"} {
		for _, streaming := range []bool{false, true} {
			for _, scenario := range []struct {
				name            string
				baseURL         string
				oauth           bool
				callerDisplay   string
				overrideDisplay string
				filterDisplay   bool
				wantDisplay     string
			}{
				{name: "default official OAuth", oauth: true, wantDisplay: "updates"},
				{name: "explicit official API key", baseURL: "https://api.anthropic.com", wantDisplay: "updates"},
				{name: "third party API key", baseURL: "https://gateway.example", wantDisplay: "summarized"},
				{name: "third party OAuth", baseURL: "https://gateway.example", oauth: true, wantDisplay: "summarized"},
				{name: "lookalike hostname", baseURL: "https://api.anthropic.com.gateway.example", wantDisplay: "summarized"},
				{name: "explicit updates", baseURL: "https://gateway.example", callerDisplay: "updates", wantDisplay: "updates"},
				{name: "explicit summarized", baseURL: "https://gateway.example", callerDisplay: "summarized", wantDisplay: "summarized"},
				{name: "explicit omitted", baseURL: "https://gateway.example", callerDisplay: "omitted", wantDisplay: "omitted"},
				{name: "payload override", baseURL: "https://gateway.example", overrideDisplay: "omitted", wantDisplay: "omitted"},
				{name: "payload filter", baseURL: "https://gateway.example", filterDisplay: true},
			} {
				test.Run(fmt.Sprintf("%s/stream=%t/%s", model, streaming, scenario.name), func(subtest *testing.T) {
					configuration := &config.Config{}
					modelRules := []config.PayloadModelRule{{Name: model, Protocol: "claude"}}
					if scenario.overrideDisplay != "" {
						configuration.Payload.Override = []config.PayloadRule{{
							Models: modelRules,
							Params: map[string]any{"thinking.display": scenario.overrideDisplay},
						}}
					}
					if scenario.filterDisplay {
						configuration.Payload.Filter = []config.PayloadFilterRule{{
							Models: modelRules,
							Params: []string{"thinking.display"},
						}}
					}
					auth := directClaudeAPIKeyAuth()
					if scenario.oauth {
						auth = directClaudeOAuthAuth()
					}
					auth.Attributes["base_url"] = scenario.baseURL
					auth.Attributes["cloak_mode"] = "always"
					payload := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":1024,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hello"}]}`, model))
					if scenario.callerDisplay != "" {
						var errSet error
						payload, errSet = sjson.SetBytes(payload, "thinking.display", scenario.callerDisplay)
						if errSet != nil {
							subtest.Fatal(errSet)
						}
					}
					var sentBody []byte
					var sentHeaders http.Header
					transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
						var errRead error
						sentBody, errRead = io.ReadAll(request.Body)
						if errRead != nil {
							return nil, errRead
						}
						sentHeaders = request.Header.Clone()
						contentType := "application/json"
						responseBody := fmt.Sprintf(`{"id":"msg_display","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`, model)
						if streaming {
							contentType = "text/event-stream"
							responseBody = fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_display\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n", model) +
								"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
								"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
						}
						return &http.Response{
							StatusCode: http.StatusOK,
							Header:     http.Header{"Content-Type": {contentType}},
							Body:       io.NopCloser(strings.NewReader(responseBody)),
							Request:    request,
						}, nil
					})
					requestContext := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
					executor := NewClaudeExecutor(configuration)
					request := cliproxyexecutor.Request{Model: model, Payload: payload}
					options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: streaming}
					if streaming {
						result, errExecute := executor.ExecuteStream(requestContext, auth, request, options)
						if errExecute != nil {
							subtest.Fatalf("ExecuteStream() error: %v", errExecute)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								subtest.Fatalf("stream chunk error: %v", chunk.Err)
							}
						}
					} else if _, errExecute := executor.Execute(requestContext, auth, request, options); errExecute != nil {
						subtest.Fatalf("Execute() error: %v", errExecute)
					}
					display := gjson.GetBytes(sentBody, "thinking.display")
					if display.String() != scenario.wantDisplay || (scenario.wantDisplay == "" && display.Exists()) {
						subtest.Fatalf("thinking.display = %s, want %q", display.Raw, scenario.wantDisplay)
					}
					hasUpdatesBeta := strings.Contains(sentHeaders.Get("Anthropic-Beta"), claudeThinkingDisplayUpdatesBeta)
					if hasUpdatesBeta != (scenario.wantDisplay == "updates") {
						subtest.Fatalf("updates beta = %t for display %q", hasUpdatesBeta, scenario.wantDisplay)
					}
				})
			}
		}
	}
}
