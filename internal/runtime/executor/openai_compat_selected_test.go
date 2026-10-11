package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestSelectedOpenAICompatibilityResponsesRouting(t *testing.T) {
	for _, testCase := range []struct {
		name               string
		useChatCompletions bool
		wantPath           string
		responseBody       string
		streamBody         string
	}{
		{
			name:         "native Responses default",
			wantPath:     "/v1/responses",
			responseBody: `{"id":"resp_1","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`,
			streamBody:   "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[]}}\n\n",
		},
		{
			name:               "explicit Chat compatibility",
			useChatCompletions: true,
			wantPath:           "/v1/chat/completions",
			responseBody:       `{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			streamBody:         "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		},
	} {
		for _, stream := range []bool{false, true} {
			for _, ruleCase := range []struct {
				name         string
				payloadRules config.PayloadConfig
				expectedTier string
			}{
				{name: "preserve", expectedTier: "priority"},
				{
					name: "override",
					payloadRules: config.PayloadConfig{Override: []config.PayloadRule{{
						Models: []config.PayloadModelRule{{Name: "*"}},
						Params: map[string]any{"service_tier": "default"},
					}}},
					expectedTier: "default",
				},
				{
					name: "filter",
					payloadRules: config.PayloadConfig{Filter: []config.PayloadFilterRule{{
						Models: []config.PayloadModelRule{{Name: "*"}},
						Params: []string{"service_tier"},
					}}},
				},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", testCase.name, stream, ruleCase.name), func(t *testing.T) {
					capturedRequests := make(chan struct {
						path string
						body []byte
					}, 1)
					upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						requestBody, errRead := io.ReadAll(request.Body)
						if errRead != nil {
							http.Error(writer, errRead.Error(), http.StatusBadRequest)
							return
						}
						capturedRequests <- struct {
							path string
							body []byte
						}{path: request.URL.Path, body: requestBody}
						if stream {
							writer.Header().Set("Content-Type", "text/event-stream")
							_, _ = writer.Write([]byte(testCase.streamBody))
							return
						}
						writer.Header().Set("Content-Type", "application/json")
						_, _ = writer.Write([]byte(testCase.responseBody))
					}))
					t.Cleanup(upstream.Close)
					configuration := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
						Name:               "selected-provider",
						UseChatCompletions: testCase.useChatCompletions,
					}}}
					configuration.Payload = ruleCase.payloadRules
					executor := NewOpenAICompatExecutor("selected-provider", configuration)
					auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
						"base_url": upstream.URL + "/v1", "api_key": "test-key", "compat_name": "selected-provider",
					}}
					outboundRequest := cliproxyexecutor.Request{
						Model:   "selected-model",
						Payload: []byte(fmt.Sprintf(`{"model":"selected-model","input":"hello","service_tier":"priority","stream":%t}`, stream)),
					}
					options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream}
					if stream {
						result, errExecute := executor.ExecuteStream(context.Background(), auth, outboundRequest, options)
						if errExecute != nil {
							t.Fatalf("execute stream: %v", errExecute)
						}
						var output bytes.Buffer
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatalf("stream chunk: %v", chunk.Err)
							}
							output.Write(chunk.Payload)
						}
						if !bytes.Contains(output.Bytes(), []byte("response.completed")) {
							t.Fatal("missing Responses terminal event")
						}
					} else if _, errExecute := executor.Execute(context.Background(), auth, outboundRequest, options); errExecute != nil {
						t.Fatalf("execute request: %v", errExecute)
					}
					capturedRequest := <-capturedRequests
					if capturedRequest.path != testCase.wantPath {
						t.Fatalf("request path = %q, want %q", capturedRequest.path, testCase.wantPath)
					}
					actualTier := gjson.GetBytes(capturedRequest.body, "service_tier")
					if actualTier.Exists() != (ruleCase.expectedTier != "") || actualTier.String() != ruleCase.expectedTier {
						t.Fatalf("final upstream service_tier = %s, want %q", actualTier.Raw, ruleCase.expectedTier)
					}
				})
			}
		}
	}
}

func TestSelectedOpenAICompatibilityNativeResponsesTerminalEvent(t *testing.T) {
	requestPath := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestPath = request.URL.Path
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[]}}\n\n"))
	}))
	t.Cleanup(upstream.Close)

	executor := NewOpenAICompatExecutor("selected-provider", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": upstream.URL + "/v1", "api_key": "test-key"}}
	result, errExecute := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "selected-model",
		Payload: []byte(`{"model":"selected-model","input":"hello","stream":true}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
	if errExecute != nil {
		t.Fatalf("execute native Responses stream: %v", errExecute)
	}
	var output bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("native Responses stream ended with error: %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if requestPath != "/v1/responses" || !bytes.Contains(output.Bytes(), []byte("response.completed")) {
		t.Fatalf("native Responses path=%q output=%q", requestPath, output.String())
	}
}

func TestSelectedOpenAICompatibilityUsageAccounting(t *testing.T) {
	const responsesUsage = `{"input_tokens":10,"output_tokens":6,"total_tokens":16,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":2}}`
	const chatUsage = `{"prompt_tokens":10,"completion_tokens":6,"total_tokens":16,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":2}}`
	nativeResponse := `{"id":"resp_usage","object":"response","model":"selected-model","status":"completed","service_tier":"default","output":[],"usage":` + responsesUsage + `}`
	nativeCompleted := "event: response.completed\ndata: " + `{"type":"response.completed","response":` + nativeResponse + `}` + "\n\n"
	earlyTier := "event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp_usage","model":"selected-model","service_tier":"priority","usage":null}}` + "\n\n"
	chatResponse := `{"id":"chatcmpl_usage","object":"chat.completion","model":"selected-model","service_tier":"default","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":` + chatUsage + `}`
	chatStream := "data: " + `{"id":"chatcmpl_usage","object":"chat.completion.chunk","model":"selected-model","service_tier":"default","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":` + chatUsage + `}` + "\n\ndata: [DONE]\n\n"
	missingUsage := "event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_usage","object":"response","model":"selected-model","status":"completed","output":[]}}` + "\n\n"

	for _, testCase := range []struct {
		name               string
		stream             bool
		useChatCompletions bool
		body               string
		wantUsage          bool
		wantResponseTier   string
	}{
		{name: "native completed and final tier", stream: true, body: earlyTier + nativeCompleted, wantUsage: true, wantResponseTier: "default"},
		{name: "native done", stream: true, body: strings.ReplaceAll(nativeCompleted, "response.completed", "response.done"), wantUsage: true, wantResponseTier: "default"},
		{name: "native incomplete", stream: true, body: strings.ReplaceAll(nativeCompleted, "completed", "incomplete"), wantUsage: true, wantResponseTier: "default"},
		{name: "native terminal at EOF", stream: true, body: strings.TrimSuffix(nativeCompleted, "\n\n"), wantUsage: true, wantResponseTier: "default"},
		{name: "native missing usage and tier", stream: true, body: missingUsage},
		{name: "native nonstream", body: nativeResponse, wantUsage: true, wantResponseTier: "default"},
		{name: "chat stream", stream: true, useChatCompletions: true, body: chatStream, wantUsage: true, wantResponseTier: "default"},
		{name: "chat nonstream", useChatCompletions: true, body: chatResponse, wantUsage: true, wantResponseTier: "default"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			alias := t.Name()
			capture := &multiProviderUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
			coreusage.RegisterNamedPlugin(t.Name(), capture)
			t.Cleanup(func() {
				coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{})
			})
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if testCase.stream {
					writer.Header().Set("Content-Type", "text/event-stream")
				} else {
					writer.Header().Set("Content-Type", "application/json")
				}
				_, _ = io.WriteString(writer, testCase.body)
			}))
			t.Cleanup(upstream.Close)

			configuration := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
				Name: "selected-provider", UseChatCompletions: testCase.useChatCompletions,
			}}}
			executor := NewOpenAICompatExecutor("selected-provider", configuration)
			auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
				"base_url": upstream.URL + "/v1", "api_key": "test-key", "compat_name": "selected-provider",
			}}
			ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
			ctx = coreusage.WithServiceTier(ctx, "priority")
			ctx = coreusage.WithStream(ctx, testCase.stream)
			request := cliproxyexecutor.Request{
				Model:   "selected-model",
				Payload: []byte(fmt.Sprintf(`{"model":"selected-model","input":"hello","service_tier":"priority","stream":%t}`, testCase.stream)),
			}
			options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: testCase.stream}
			var responsePayload []byte
			if testCase.stream {
				result, errExecute := executor.ExecuteStream(ctx, auth, request, options)
				if errExecute != nil {
					t.Fatalf("execute stream: %v", errExecute)
				}
				var output bytes.Buffer
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk: %v", chunk.Err)
					}
					output.Write(chunk.Payload)
				}
				for _, line := range bytes.Split(output.Bytes(), []byte("\n")) {
					if !bytes.HasPrefix(line, []byte("data:")) {
						continue
					}
					payload := bytes.TrimSpace(line[len("data:"):])
					switch gjson.GetBytes(payload, "type").String() {
					case "response.completed", "response.done", "response.incomplete":
						responsePayload = []byte(gjson.GetBytes(payload, "response").Raw)
					}
				}
			} else {
				result, errExecute := executor.Execute(ctx, auth, request, options)
				if errExecute != nil {
					t.Fatalf("execute request: %v", errExecute)
				}
				responsePayload = result.Payload
			}
			if !gjson.ValidBytes(responsePayload) {
				t.Fatal("missing valid client response")
			}

			record := capture.await(t)
			if record.Failed || record.ExecutorType != "OpenAICompatExecutor" || record.Stream != testCase.stream {
				t.Fatalf("unexpected request outcome: %+v", record)
			}
			if record.ServiceTier != "priority" || record.ResponseServiceTier != testCase.wantResponseTier {
				t.Fatalf("requested/response tiers = %q/%q", record.ServiceTier, record.ResponseServiceTier)
			}
			if tier := gjson.GetBytes(responsePayload, "service_tier").String(); tier != testCase.wantResponseTier {
				t.Fatalf("client response tier = %q, want %q", tier, testCase.wantResponseTier)
			}
			actualCounts := map[string]int64{
				"input_tokens":                           record.Detail.InputTokens,
				"output_tokens":                          record.Detail.OutputTokens,
				"total_tokens":                           record.Detail.TotalTokens,
				"input_tokens_details.cached_tokens":     record.Detail.CachedTokens,
				"output_tokens_details.reasoning_tokens": record.Detail.ReasoningTokens,
			}
			expectedCounts := map[string]int64{
				"input_tokens": 10, "output_tokens": 6, "total_tokens": 16,
				"input_tokens_details.cached_tokens": 4, "output_tokens_details.reasoning_tokens": 2,
			}
			clientUsage := gjson.GetBytes(responsePayload, "usage")
			if clientUsage.Exists() != testCase.wantUsage {
				t.Fatalf("client usage exists = %v, want %v", clientUsage.Exists(), testCase.wantUsage)
			}
			for field, expectedCount := range expectedCounts {
				if !testCase.wantUsage {
					expectedCount = 0
				}
				if actualCounts[field] != expectedCount {
					t.Fatalf("record %s = %d, want %d", field, actualCounts[field], expectedCount)
				}
				if testCase.wantUsage && clientUsage.Get(field).Int() != expectedCount {
					t.Fatalf("client %s = %d, want %d", field, clientUsage.Get(field).Int(), expectedCount)
				}
			}
			if testCase.wantUsage && (!record.Detail.TokenBreakdown.Valid() || record.Detail.CacheReadTokens != 4) {
				t.Fatalf("missing normalized token breakdown: %+v", record.Detail)
			}

			// FIFO delivery makes this a deterministic barrier for duplicate records.
			coreusage.PublishRecord(ctx, coreusage.Record{Alias: alias, Model: "usage-test-barrier"})
			if next := capture.await(t); next.Model != "usage-test-barrier" {
				t.Fatalf("unexpected duplicate usage record: %+v", next)
			}
		})
	}
}
