package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
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
