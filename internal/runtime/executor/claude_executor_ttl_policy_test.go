package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeExecutorPreservesOneHourCacheTTLOnWire(t *testing.T) {
	for _, testCase := range []struct {
		name                string
		stream              bool
		payload             string
		wantAutomaticMarker bool
	}{
		{
			name:    "non-stream block-level TTL",
			payload: `{"model":"claude-sonnet-5","max_tokens":16,"system":"shared prompt","messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`,
		},
		{
			name:    "stream block-level TTL",
			stream:  true,
			payload: `{"model":"claude-sonnet-5","max_tokens":16,"system":"shared prompt","messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`,
		},
		{
			name:                "non-stream automatic TTL",
			payload:             `{"model":"claude-sonnet-5","max_tokens":16,"cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[{"role":"user","content":"hello"}]}`,
			wantAutomaticMarker: true,
		},
		{
			name:                "stream automatic TTL",
			stream:              true,
			payload:             `{"model":"claude-sonnet-5","max_tokens":16,"cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[{"role":"user","content":"hello"}]}`,
			wantAutomaticMarker: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var upstreamBody []byte
			var upstreamHeaders http.Header
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(request.Body)
				if errRead != nil {
					t.Errorf("read upstream request: %v", errRead)
				}
				upstreamHeaders = request.Header.Clone()
				if testCase.stream {
					response.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(response, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					return
				}
				response.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(response, `{"id":"msg_1","type":"message","model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer server.Close()

			executor := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"api_key":    "test-api-key",
				"base_url":   server.URL,
				"cloak_mode": "always",
			}}
			request := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(testCase.payload)}
			options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
			if testCase.stream {
				result, errStream := executor.ExecuteStream(context.Background(), auth, request, options)
				if errStream != nil {
					t.Fatalf("ExecuteStream() error: %v", errStream)
				}
				if result == nil || result.Chunks == nil {
					t.Fatal("ExecuteStream() returned no response stream")
				}
				for range result.Chunks {
				}
			} else if _, errExecute := executor.Execute(context.Background(), auth, request, options); errExecute != nil {
				t.Fatalf("Execute() error: %v", errExecute)
			}

			if got := gjson.GetBytes(upstreamBody, "system.1.cache_control.ttl").String(); got != "1h" {
				t.Fatalf("injected system breakpoint TTL = %q, want 1h", got)
			}
			if errValidate := validateClaudeCacheControlTTL(upstreamBody); errValidate != nil {
				t.Fatalf("invalid final upstream TTL layout: %v", errValidate)
			}
			if testCase.wantAutomaticMarker {
				if got := gjson.GetBytes(upstreamBody, "cache_control.ttl").String(); got != "1h" {
					t.Fatalf("automatic cache TTL = %q, want 1h", got)
				}
			} else {
				foundMessageTTL := false
				for _, message := range gjson.GetBytes(upstreamBody, "messages").Array() {
					for _, block := range message.Get("content").Array() {
						foundMessageTTL = foundMessageTTL || block.Get("cache_control.ttl").String() == "1h"
					}
				}
				if !foundMessageTTL {
					t.Fatal("client block-level 1h cache TTL was lost")
				}
			}
			if strings.Contains(upstreamHeaders.Get("Anthropic-Beta"), claudeExtendedCacheTTLBeta) {
				t.Fatal("API-key request unexpectedly gained the legacy extended-cache beta")
			}
		})
	}
}

func TestClaudeAdaptiveCachePreservesExplicitTTL(t *testing.T) {
	configuration := &config.Config{ClaudePromptCache: config.ClaudePromptCacheConfig{Mode: config.ClaudePromptCacheModeAdaptive}}
	executor := NewClaudeExecutorWithPromptCacheRuntime(configuration, NewClaudePromptCacheRuntime())
	for _, payload := range [][]byte{
		[]byte(`{"cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[{"role":"user","content":"hello"}]}`),
		[]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`),
	} {
		out, plan := executor.planAdaptiveClaudePromptCache(context.Background(), nil, "test-api-key", "https://api.anthropic.com", "claude-sonnet-5", payload)
		if plan != nil || !bytes.Equal(out, payload) {
			t.Fatalf("adaptive planner rewrote caller TTL: plan=%v, payload=%s", plan, out)
		}
	}

	explicitPayload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}]}`)
	out, plan := executor.planAdaptiveClaudePromptCache(withExplicitPromptCacheMode(context.Background(), true), nil, "test-api-key", "https://api.anthropic.com", "claude-sonnet-5", explicitPayload)
	if plan != nil || !bytes.Equal(out, explicitPayload) {
		t.Fatalf("adaptive planner ignored explicit mode: plan=%v, payload=%s", plan, out)
	}
}

func TestClaudeExecutorNormalizesExplicitCacheMode(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		upstreamBody, _ = io.ReadAll(request.Body)
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"id":"msg_1","type":"message","model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-api-key", "base_url": server.URL}}
	payload := []byte(`{"model":"claude-sonnet-5","max_tokens":16,"prompt_cache_options":{"mode":"explicit"},"system":[{"type":"text","text":"static","cache_control":{"type":"ephemeral","ttl":"5m"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	request := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: payload}
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}

	if _, errExecute := NewClaudeExecutor(&config.Config{}).Execute(context.Background(), auth, request, options); errExecute != nil {
		t.Fatalf("Execute() error: %v", errExecute)
	}
	if got := gjson.GetBytes(upstreamBody, "system.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("explicit-mode system TTL = %q, want 1h", got)
	}
	if got := gjson.GetBytes(upstreamBody, "messages.0.content.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("explicit-mode message TTL = %q, want 1h", got)
	}
	if gjson.GetBytes(upstreamBody, "prompt_cache_options").Exists() {
		t.Fatal("internal prompt cache options leaked upstream")
	}
}

func TestClaudeExecutorCountTokensPreservesOneHourCacheTTL(t *testing.T) {
	var upstreamBody []byte
	var upstreamHeaders http.Header
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		upstreamBody, _ = io.ReadAll(request.Body)
		upstreamHeaders = request.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"input_tokens":12}`)),
			Request:    request,
		}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-api-key", "base_url": "https://api.anthropic.com"}}
	payload := []byte(`{"model":"claude-sonnet-5","max_tokens":16,"system":[{"type":"text","text":"static","cache_control":{"type":"ephemeral","ttl":"5m"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	request := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: payload}
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}

	if _, errCount := NewClaudeExecutor(&config.Config{}).CountTokens(ctx, auth, request, options); errCount != nil {
		t.Fatalf("CountTokens() error: %v", errCount)
	}
	if got := gjson.GetBytes(upstreamBody, "system.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("count_tokens system TTL = %q, want 1h", got)
	}
	if got := gjson.GetBytes(upstreamBody, "messages.0.content.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("count_tokens message TTL = %q, want 1h", got)
	}
	if strings.Contains(upstreamHeaders.Get("Anthropic-Beta"), claudeExtendedCacheTTLBeta) {
		t.Fatal("API-key count_tokens request unexpectedly gained the legacy cache beta")
	}
}

func TestClaudeExecutorRejectsTTLConflictIntroducedByPayloadRules(t *testing.T) {
	upstreamCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		upstreamCalled = true
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	configuration := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "claude-sonnet-5"}},
			Params: map[string]any{"messages.0.content.0.cache_control.ttl": "1h"},
		}},
	}}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-api-key", "base_url": server.URL}}
	payload := []byte(`{"model":"claude-sonnet-5","max_tokens":16,"system":[{"type":"text","text":"static","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`)
	request := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: payload}
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}

	_, errExecute := NewClaudeExecutor(configuration).Execute(context.Background(), auth, request, options)
	var statusCoder interface{ StatusCode() int }
	if !errors.As(errExecute, &statusCoder) || statusCoder.StatusCode() != http.StatusBadRequest {
		t.Fatalf("Execute() error = %v, want final request-level 400", errExecute)
	}
	if upstreamCalled {
		t.Fatal("an invalid configured TTL layout reached the upstream")
	}
}

func TestClaudeHeaderPreservesCallerExtendedCacheBetaForAPIKey(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://gateway.example/v1/messages", nil)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-api-key"}}
	incomingHeaders := http.Header{}
	incomingHeaders.Set("Anthropic-Beta", claudeExtendedCacheTTLBeta)
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)

	if errHeaders := applyClaudeHeaders(request, auth, "test-api-key", false, nil, body, nil, incomingHeaders, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error: %v", errHeaders)
	}
	if got := request.Header.Get("Anthropic-Beta"); !strings.Contains(got, claudeExtendedCacheTTLBeta) {
		t.Fatalf("caller beta missing from upstream request: %q", got)
	}
}

func TestStripClaudeCacheControlTTLIncludesAutomaticBreakpoint(t *testing.T) {
	payload := []byte(`{"cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[{"content":[{"cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	out := stripClaudeCacheControlTTL(payload)
	if helps.ClaudePayloadHas1hTTL(out) || !gjson.GetBytes(out, "cache_control.type").Exists() {
		t.Fatalf("stripClaudeCacheControlTTL() did not clear top-level and block TTL: %s", out)
	}
}
