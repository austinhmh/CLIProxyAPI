package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestSelectedOpenAICompatibilityResponsesRouting(t *testing.T) {
	for _, testCase := range []struct {
		name               string
		useChatCompletions bool
		wantPath           string
		responseBody       string
	}{
		{
			name:         "native Responses default",
			wantPath:     "/v1/responses",
			responseBody: `{"id":"resp_1","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`,
		},
		{
			name:               "explicit Chat compatibility",
			useChatCompletions: true,
			wantPath:           "/v1/chat/completions",
			responseBody:       `{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			requestPath := ""
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requestPath = request.URL.Path
				_, _ = io.Copy(io.Discard, request.Body)
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(testCase.responseBody))
			}))
			t.Cleanup(upstream.Close)

			configuration := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
				Name:               "selected-provider",
				UseChatCompletions: testCase.useChatCompletions,
			}}}
			executor := NewOpenAICompatExecutor("selected-provider", configuration)
			auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
				"base_url":    upstream.URL + "/v1",
				"api_key":     "test-key",
				"compat_name": "selected-provider",
			}}
			_, errExecute := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "selected-model",
				Payload: []byte(`{"model":"selected-model","input":[{"role":"user","content":"hello"}]}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
			if errExecute != nil {
				t.Fatalf("execute Responses request: %v", errExecute)
			}
			if requestPath != testCase.wantPath {
				t.Fatalf("request path = %q, want %q", requestPath, testCase.wantPath)
			}
		})
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
