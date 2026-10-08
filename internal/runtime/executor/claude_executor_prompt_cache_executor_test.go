package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestApplyClaudeHeadersWithSessionOverridesConflictingClientHeader(t *testing.T) {
	resolvedSessionID := "12345678-1234-1234-1234-123456789abc"
	request := newClaudeHeaderTestRequest(t, http.Header{
		"X-Claude-Code-Session-Id": []string{"client-conflict"},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":    "key-session-alignment",
		"cloak_mode": "always",
	}}

	if errHeaders := applyClaudeHeaders(request, auth, "key-session-alignment", false, nil, []byte(`{}`), &config.Config{}, nil, false, resolvedSessionID); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	if got := request.Header.Get("X-Claude-Code-Session-Id"); got != resolvedSessionID {
		t.Fatalf("X-Claude-Code-Session-Id = %q, want %q", got, resolvedSessionID)
	}
}

func TestApplyClaudeHeadersWithSessionOverridesConflictingAuthHeader(t *testing.T) {
	resolvedSessionID := "12345678-1234-1234-1234-123456789abc"
	request := newClaudeHeaderTestRequest(t, nil)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":                         "key-session-alignment",
		"cloak_mode":                      "always",
		"header:X-Claude-Code-Session-Id": "credential-conflict",
	}}

	if errHeaders := applyClaudeHeaders(request, auth, "key-session-alignment", false, nil, []byte(`{}`), &config.Config{}, nil, false, resolvedSessionID); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	if got := request.Header.Get("X-Claude-Code-Session-Id"); got != resolvedSessionID {
		t.Fatalf("X-Claude-Code-Session-Id = %q, want %q", got, resolvedSessionID)
	}
}

func TestClaudeExecutorExecuteStreamRetainsStartedCacheStateUntilStreamEnds(t *testing.T) {
	configuredWaitSeconds := 1
	streamReader, streamWriter := io.Pipe()
	initialStreamData := []byte(`data: {"type":"message_start","message":{"id":"msg_stream","model":"claude-sonnet-4-5","usage":{"input_tokens":1,"output_tokens":0,"cache_creation_input_tokens":10}}}` + "\n\n")
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			go func() {
				_, _ = streamWriter.Write([]byte(strings.Join([]string{
					`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
					`data: {"type":"message_stop"}`,
				}, "\n\n") + "\n\n"))
				_ = streamWriter.Close()
			}()
		})
	}
	capturedBody := make(chan []byte, 1)
	defer release()
	baseURL := "https://anthropic.example.com"

	runtime := NewClaudePromptCacheRuntime()
	executor := NewClaudeExecutorWithPromptCacheRuntime(&config.Config{
		ClaudePromptCache: config.ClaudePromptCacheConfig{
			Mode:                    config.ClaudePromptCacheModeAdaptive,
			ColdStartMaxWaitSeconds: &configuredWaitSeconds,
		},
	}, runtime)
	auth := &cliproxyauth.Auth{ID: "stream-cache-auth", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": baseURL,
	}}
	payload := []byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":16,
		"tools":[{"name":"Read","input_schema":{"type":"object"}}],
		"system":[{"type":"text","text":"system"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"first"}]},
			{"role":"assistant","content":[{"type":"text","text":"answer"}]},
			{"role":"user","content":[{"type":"text","text":"second"}]}
		]
	}`)

	testContext, cancelTest := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelTest()
	requestContext := context.WithValue(testContext, "cliproxy.roundtripper", roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		capturedBody <- bytes.Clone(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(io.MultiReader(bytes.NewReader(initialStreamData), streamReader)),
			Request:    request,
		}, nil
	}))
	result, errStream := executor.ExecuteStream(requestContext, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}

	var upstreamBody []byte
	select {
	case upstreamBody = <-capturedBody:
	case <-testContext.Done():
		t.Fatalf("timed out waiting for captured upstream body: %v", testContext.Err())
	}
	// Recompute the plan directly against the runtime using the actual bytes
	// sent upstream. This reads the ground-truth breakpoint locations the first
	// planning pass already committed, so the resulting prefix keys exactly
	// match the ones registered when the in-flight request acquired its flight.
	scopeKey := claudePromptCacheScopeKey(auth, "key-123", baseURL, "claude-sonnet-4-5")
	officialAnthropic := isOfficialAnthropicBaseURL(baseURL)
	_, matchingPlan := runtime.PlanClaudePromptCache(
		scopeKey,
		upstreamBody,
		helps.ClaudePromptCacheCapabilities{
			AutomaticHistory: officialAnthropic,
			ExplicitHistory:  true,
		},
	)
	if matchingPlan == nil || len(matchingPlan.Prefixes) == 0 {
		t.Fatal("adaptive planner returned no prefixes for stream lifecycle probe")
	}
	probePlan := *matchingPlan
	probePlan.Prefixes = append(
		append([]helps.ClaudePromptCachePrefix(nil), matchingPlan.Prefixes...),
		helps.ClaudePromptCachePrefix{
			Key:   "stream-lifecycle-probe-prefix",
			Kind:  "messages",
			Depth: len(matchingPlan.Prefixes) + 100,
			TTL:   5 * time.Minute,
		},
	)
	matchingAttempt, errAcquire := runtime.Acquire(context.Background(), &probePlan, 50*time.Millisecond)
	if errAcquire != nil {
		t.Fatalf("Acquire() error = %v", errAcquire)
	}
	if !matchingAttempt.IsLeader() {
		t.Fatal("probe request did not acquire its independent cold prefix after response headers")
	}
	claimedPrefixKeys := matchingAttempt.ClaimedPrefixKeys()
	if len(claimedPrefixKeys) != 1 || claimedPrefixKeys[0] != "stream-lifecycle-probe-prefix" {
		matchingAttempt.Fail()
		t.Fatalf("probe claimed keys = %v, want only the independent probe prefix", claimedPrefixKeys)
	}
	matchingAttempt.Fail()

	release()
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
		}
	}
}

func TestClaudeExecutorAdaptiveStripsAndReplansUserCacheControl(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{
		ClaudePromptCache: config.ClaudePromptCacheConfig{Mode: config.ClaudePromptCacheModeAdaptive},
	})
	auth := &cliproxyauth.Auth{ID: "user-marked-auth"}

	// Client already placed in-body + top-level cache_control. Adaptive mode
	// must strip them and re-apply CPA breakpoints for official Anthropic:
	// tools-tail + system-tail + second-to-last user + top-level automatic.
	payload := []byte(`{
		"model":"claude-sonnet-4-5",
		"cache_control":{"type":"ephemeral"},
		"tools":[{"name":"Read","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"system":[{"type":"text","text":"system","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]},
			{"role":"assistant","content":[{"type":"text","text":"answer"}]},
			{"role":"user","content":[{"type":"text","text":"second","cache_control":{"type":"ephemeral"}}]}
		]
	}`)

	plannedBody, plan := executor.planAdaptiveClaudePromptCache(
		context.Background(),
		auth,
		"key-user-marked",
		"https://api.anthropic.com",
		"claude-sonnet-4-5",
		bytes.Clone(payload),
	)

	if plan == nil {
		t.Fatal("adaptive planner must produce a plan after stripping client cache_control")
	}
	if !plan.Summary.AutomaticHistory {
		t.Fatal("official Anthropic adaptive plan must keep automatic history")
	}
	if !gjson.GetBytes(plannedBody, "cache_control").Exists() {
		t.Fatal("official Anthropic adaptive plan must set top-level cache_control")
	}
	if !gjson.GetBytes(plannedBody, "tools.0.cache_control").Exists() {
		t.Fatal("adaptive plan missing tools-tail breakpoint")
	}
	if !gjson.GetBytes(plannedBody, "system.0.cache_control").Exists() {
		t.Fatal("adaptive plan missing system-tail breakpoint")
	}
	if !gjson.GetBytes(plannedBody, "messages.0.content.0.cache_control").Exists() {
		t.Fatal("adaptive plan missing second-to-last user breakpoint")
	}
	if gjson.GetBytes(plannedBody, "messages.2.content.0.cache_control").Exists() {
		t.Fatal("adaptive plan must not leave cache_control on the latest user turn")
	}
	if got := countCacheControls(plannedBody); got != 3 {
		t.Fatalf("in-body cache_control count = %d, want 3", got)
	}
}

func TestClaudeExecutorAdaptiveReplansOverLimitUserCacheControl(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{
		ClaudePromptCache: config.ClaudePromptCacheConfig{Mode: config.ClaudePromptCacheModeAdaptive},
	})
	auth := &cliproxyauth.Auth{ID: "over-limit-auth"}

	// Client marked 5 breakpoints -- one over Anthropic's limit. Adaptive mode
	// strips them and re-plans within the supported budget.
	payload := []byte(`{
		"model":"claude-sonnet-4-5",
		"tools":[{"name":"Read","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"system":[{"type":"text","text":"system","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]},
			{"role":"assistant","content":[{"type":"text","text":"answer","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"second","cache_control":{"type":"ephemeral"}}]}
		]
	}`)

	plannedBody, plan := executor.planAdaptiveClaudePromptCache(
		context.Background(),
		auth,
		"key-over-limit",
		"https://api.anthropic.com",
		"claude-sonnet-4-5",
		bytes.Clone(payload),
	)

	if plan == nil {
		t.Fatal("adaptive planner must produce a plan after stripping over-limit client cache_control")
	}
	if got := countCacheControls(plannedBody); got != 3 {
		t.Fatalf("in-body cache_control count = %d, want 3 after replan", got)
	}
	if !gjson.GetBytes(plannedBody, "cache_control").Exists() {
		t.Fatal("official Anthropic adaptive plan must set top-level cache_control")
	}
	if gjson.GetBytes(plannedBody, "messages.1.content.0.cache_control").Exists() {
		t.Fatal("adaptive replan must not keep cache_control on assistant content")
	}
}

func TestClaudeExecutor_PreservesResolvedSessionThroughCloakAndHeaders(t *testing.T) {
	t.Parallel()

	var upstreamBody []byte
	var upstreamSessionID string
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		upstreamBody, _ = io.ReadAll(request.Body)
		upstreamSessionID = request.Header.Get("X-Claude-Code-Session-Id")
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	requestPayload := []byte(`{"messages":[{"role":"user","content":"stable first message"}],"metadata":{"user_id":"invalid-client-value"}}`)
	_, resolvedIdentity, errResolve := cliproxyauth.ResolveClaudeRequestIdentity(requestPayload)
	if errResolve != nil {
		t.Fatalf("ResolveClaudeRequestIdentity() error = %v", errResolve)
	}

	executor := NewClaudeExecutor(&config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey:  "key-resolved-identity",
			BaseURL: server.URL,
			Cloak:   &config.CloakConfig{Mode: "always"},
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-resolved-identity",
		"base_url": server.URL,
	}}

	_, errExecute := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet",
		Payload: requestPayload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Metadata: map[string]any{
			cliproxyexecutor.ClaudeUserIDMetadataKey:    resolvedIdentity.UserID,
			cliproxyexecutor.ClaudeSessionIDMetadataKey: resolvedIdentity.SessionID,
		},
	})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}

	upstreamUserID := gjson.GetBytes(upstreamBody, "metadata.user_id").String()
	if !helps.IsValidUserID(upstreamUserID) {
		t.Fatalf("upstream metadata.user_id = %q, want valid Claude Code JSON identity", upstreamUserID)
	}
	upstreamMetadataSessionID := gjson.Get(upstreamUserID, "session_id").String()
	if upstreamMetadataSessionID != resolvedIdentity.SessionID {
		t.Fatalf("upstream metadata session_id = %q, want %q", upstreamMetadataSessionID, resolvedIdentity.SessionID)
	}
	if upstreamSessionID != resolvedIdentity.SessionID {
		t.Fatalf("upstream X-Claude-Code-Session-Id = %q, want %q", upstreamSessionID, resolvedIdentity.SessionID)
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamUsesSSEHeaders(t *testing.T) {
	type capturedHeaders struct {
		accept         string
		acceptEncoding string
	}
	headersChannel := make(chan capturedHeaders, 1)
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		headersChannel <- capturedHeaders{
			accept:         request.Header.Get("Accept"),
			acceptEncoding: request.Header.Get("Accept-Encoding"),
		}
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		_, _ = responseWriter.Write([]byte(body))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	_, errExecute := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	headers := <-headersChannel
	if headers.accept != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", headers.accept)
	}
	if headers.acceptEncoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", headers.acceptEncoding)
	}
}

func TestClaudeExecutorCountTokensPassthroughEnforcesCacheControlLimit(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		seenBody = bytes.Clone(body)
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{"input_tokens":42}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudePromptCache: config.ClaudePromptCacheConfig{Mode: config.ClaudePromptCacheModePassthrough},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"model":"claude-3-5-haiku-20241022",
		"tools":[
			{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}},
			{"name":"t3","cache_control":{"type":"ephemeral"}},
			{"name":"t4","cache_control":{"type":"ephemeral"}},
			{"name":"t5","cache_control":{"type":"ephemeral"}}
		],
		"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
	}`)

	_, errCount := executor.countTokensUpstream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-haiku-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if errCount != nil {
		t.Fatalf("CountTokens() error = %v", errCount)
	}
	if got := countCacheControls(seenBody); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if !gjson.GetBytes(seenBody, "messages.0.content.0.cache_control").Exists() {
		t.Fatalf("history cache_control was removed by the four-breakpoint limit: %s", seenBody)
	}
	if gjson.GetBytes(seenBody, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("history 1h TTL was not normalized after earlier default-TTL breakpoints: %s", seenBody)
	}
}

func TestClaudeExecutorCountTokensAdaptiveUsesLocalEstimateForCompatibleProvider(t *testing.T) {
	upstreamRequestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		upstreamRequestCount++
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{"input_tokens":42}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudePromptCache: config.ClaudePromptCacheConfig{Mode: config.ClaudePromptCacheModeAdaptive},
	})
	auth := &cliproxyauth.Auth{ID: "adaptive-count-auth", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"model":"claude-3-5-haiku-20241022",
		"tools":[{"name":"Read","input_schema":{"type":"object"}}],
		"system":[{"type":"text","text":"system"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"first"}]},
			{"role":"assistant","content":[{"type":"text","text":"answer"}]},
			{"role":"user","content":[{"type":"text","text":"second"}]}
		]
	}`)

	_, errCount := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-haiku-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if errCount != nil {
		t.Fatalf("CountTokens() error = %v", errCount)
	}
	if upstreamRequestCount != 0 {
		t.Fatalf("compatible-provider count_tokens upstream requests = %d, want 0", upstreamRequestCount)
	}
}

func TestOfficialAnthropicBaseURL(t *testing.T) {
	testCases := []struct {
		name    string
		baseURL string
		want    bool
	}{
		{name: "canonical", baseURL: "https://api.anthropic.com", want: true},
		{name: "default port", baseURL: "https://api.anthropic.com:443/", want: true},
		{name: "case insensitive", baseURL: "HTTPS://API.ANTHROPIC.COM", want: true},
		{name: "http rejected", baseURL: "http://api.anthropic.com", want: false},
		{name: "custom port rejected", baseURL: "https://api.anthropic.com:8443", want: false},
		{name: "path rejected", baseURL: "https://api.anthropic.com/v1", want: false},
		{name: "query rejected", baseURL: "https://api.anthropic.com?region=test", want: false},
		{name: "compatible provider rejected", baseURL: "https://anthropic.example.com", want: false},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isOfficialAnthropicBaseURL(testCase.baseURL); got != testCase.want {
				t.Fatalf("isOfficialAnthropicBaseURL(%q) = %v, want %v", testCase.baseURL, got, testCase.want)
			}
		})
	}
}

func TestClaudePromptCacheScopeKeyIsolatesCredentialAndEndpointChanges(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "stable-auth-id"}
	baseScope := claudePromptCacheScopeKey(
		auth,
		"api-key-one",
		"https://gateway.example.com/tenant-a?region=one",
		"claude-sonnet-4-5",
	)
	testCases := []struct {
		name    string
		apiKey  string
		baseURL string
		model   string
	}{
		{name: "credential rotated", apiKey: "api-key-two", baseURL: "https://gateway.example.com/tenant-a?region=one", model: "claude-sonnet-4-5"},
		{name: "endpoint path changed", apiKey: "api-key-one", baseURL: "https://gateway.example.com/tenant-b?region=one", model: "claude-sonnet-4-5"},
		{name: "endpoint query changed", apiKey: "api-key-one", baseURL: "https://gateway.example.com/tenant-a?region=two", model: "claude-sonnet-4-5"},
		{name: "model changed", apiKey: "api-key-one", baseURL: "https://gateway.example.com/tenant-a?region=one", model: "claude-opus-4-1"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			candidateScope := claudePromptCacheScopeKey(
				auth,
				testCase.apiKey,
				testCase.baseURL,
				testCase.model,
			)
			if candidateScope == baseScope {
				t.Fatalf("scope key did not change for %s", testCase.name)
			}
		})
	}
}

func TestClaudeCacheDiagnosticsChainsSuccessfulResponses(t *testing.T) {
	runtime := NewClaudePromptCacheRuntime()
	executor := NewClaudeExecutorWithPromptCacheRuntime(
		&config.Config{ClaudePromptCache: config.ClaudePromptCacheConfig{Diagnostics: true}},
		runtime,
	)
	auth := &cliproxyauth.Auth{ID: "diagnostic-auth"}
	body := []byte(fmt.Sprintf(
		`{"metadata":{"user_id":"user_%s_account_11111111-1111-4111-8111-111111111111_session_22222222-2222-4222-8222-222222222222"},"messages":[{"role":"user","content":"hello"}]}`,
		strings.Repeat("a", 64),
	))

	firstBody, firstBetas, firstState := executor.applyClaudeCacheDiagnostics(
		auth,
		"api-key",
		"https://api.anthropic.com",
		"claude-sonnet-4-5",
		body,
		nil,
	)
	if firstState == nil || !firstState.proxyAdded {
		t.Fatal("first diagnostics state is not proxy-owned")
	}
	previousMessageID := gjson.GetBytes(firstBody, "diagnostics.previous_message_id")
	if !previousMessageID.Exists() || previousMessageID.Type != gjson.Null {
		t.Fatalf("first previous_message_id = %s, want null", previousMessageID.Raw)
	}
	if len(firstBetas) != 1 || firstBetas[0] != claudeCacheDiagnosticsBeta {
		t.Fatalf("first betas = %v, want [%s]", firstBetas, claudeCacheDiagnosticsBeta)
	}
	executor.recordClaudeCacheDiagnostics(context.Background(), firstState, "msg_first", "tools_changed", 10)

	secondBody, secondBetas, secondState := executor.applyClaudeCacheDiagnostics(
		auth,
		"api-key",
		"https://api.anthropic.com",
		"claude-sonnet-4-5",
		body,
		[]string{claudeCacheDiagnosticsBeta},
	)
	if secondState == nil || !secondState.proxyAdded {
		t.Fatal("second diagnostics state is not proxy-owned")
	}
	if got := gjson.GetBytes(secondBody, "diagnostics.previous_message_id").String(); got != "msg_first" {
		t.Fatalf("second previous_message_id = %q, want msg_first", got)
	}
	if len(secondBetas) != 1 || secondBetas[0] != claudeCacheDiagnosticsBeta {
		t.Fatalf("second betas = %v, want one diagnostics beta", secondBetas)
	}
}

func TestClaudeCacheDiagnosticsPreservesClientOwnedObject(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{
		ClaudePromptCache: config.ClaudePromptCacheConfig{Diagnostics: true},
	})
	body := []byte(fmt.Sprintf(
		`{"metadata":{"user_id":"user_%s_account_11111111-1111-4111-8111-111111111111_session_22222222-2222-4222-8222-222222222222"},"diagnostics":{"previous_message_id":"msg_client"}}`,
		strings.Repeat("b", 64),
	))

	updatedBody, betas, state := executor.applyClaudeCacheDiagnostics(
		&cliproxyauth.Auth{ID: "client-diagnostics-auth"},
		"api-key",
		"https://api.anthropic.com",
		"claude-sonnet-4-5",
		body,
		nil,
	)

	if state == nil || state.proxyAdded {
		t.Fatal("client-owned diagnostics were marked as proxy-owned")
	}
	if got := gjson.GetBytes(updatedBody, "diagnostics.previous_message_id").String(); got != "msg_client" {
		t.Fatalf("client previous_message_id = %q, want msg_client", got)
	}
	if len(betas) != 1 || betas[0] != claudeCacheDiagnosticsBeta {
		t.Fatalf("betas = %v, want diagnostics beta", betas)
	}
}

func TestClaudeCacheDiagnosticsFallbackPersistsRuntimeDecision(t *testing.T) {
	runtime := NewClaudePromptCacheRuntime()
	executor := NewClaudeExecutorWithPromptCacheRuntime(
		&config.Config{ClaudePromptCache: config.ClaudePromptCacheConfig{Diagnostics: true}},
		runtime,
	)
	auth := &cliproxyauth.Auth{ID: "diagnostic-fallback-auth"}
	body := []byte(fmt.Sprintf(
		`{"metadata":{"user_id":"user_%s_account_11111111-1111-4111-8111-111111111111_session_22222222-2222-4222-8222-222222222222"},"messages":[{"role":"user","content":"hello"}]}`,
		strings.Repeat("d", 64),
	))

	firstBody, _, firstState := executor.applyClaudeCacheDiagnostics(
		auth,
		"api-key",
		"https://api.anthropic.com",
		"claude-sonnet-4-5",
		body,
		nil,
	)
	if firstState == nil || !gjson.GetBytes(firstBody, "diagnostics").Exists() {
		t.Fatal("first request did not receive proxy diagnostics")
	}
	executor.recordClaudeDiagnosticsFallback(
		firstState,
		[]byte(`{"error":{"message":"Unknown field: diagnostics.previous_message_id"}}`),
	)
	secondBody, secondBetas, secondState := executor.applyClaudeCacheDiagnostics(
		auth,
		"api-key",
		"https://api.anthropic.com",
		"claude-sonnet-4-5",
		body,
		nil,
	)
	if secondState != nil || gjson.GetBytes(secondBody, "diagnostics").Exists() {
		t.Fatal("unsupported diagnostics were re-injected on the next request")
	}
	if len(secondBetas) != 0 {
		t.Fatalf("diagnostics beta was retained after runtime downgrade: %v", secondBetas)
	}
}

func TestClaudeCacheDiagnosticsFallbackClearsOnlyStalePreviousMessageID(t *testing.T) {
	runtime := NewClaudePromptCacheRuntime()
	executor := NewClaudeExecutorWithPromptCacheRuntime(
		&config.Config{ClaudePromptCache: config.ClaudePromptCacheConfig{Diagnostics: true}},
		runtime,
	)
	auth := &cliproxyauth.Auth{ID: "diagnostic-chain-auth"}
	body := []byte(fmt.Sprintf(
		`{"metadata":{"user_id":"user_%s_account_11111111-1111-4111-8111-111111111111_session_22222222-2222-4222-8222-222222222222"},"messages":[{"role":"user","content":"hello"}]}`,
		strings.Repeat("e", 64),
	))

	_, _, firstState := executor.applyClaudeCacheDiagnostics(
		auth,
		"api-key",
		"https://api.anthropic.com",
		"claude-sonnet-4-5",
		body,
		nil,
	)
	executor.recordClaudeCacheDiagnostics(context.Background(), firstState, "msg_stale", "", 0)
	secondBody, _, secondState := executor.applyClaudeCacheDiagnostics(
		auth,
		"api-key",
		"https://api.anthropic.com",
		"claude-sonnet-4-5",
		body,
		nil,
	)
	if got := gjson.GetBytes(secondBody, "diagnostics.previous_message_id").String(); got != "msg_stale" {
		t.Fatalf("second previous_message_id = %q, want msg_stale", got)
	}
	executor.recordClaudeDiagnosticsFallback(
		secondState,
		[]byte(`{"error":{"message":"Invalid previous_message_id"}}`),
	)
	thirdBody, thirdBetas, thirdState := executor.applyClaudeCacheDiagnostics(
		auth,
		"api-key",
		"https://api.anthropic.com",
		"claude-sonnet-4-5",
		body,
		nil,
	)
	if thirdState == nil || !thirdState.proxyAdded {
		t.Fatal("stale chain fallback incorrectly disabled diagnostics")
	}
	previousMessageID := gjson.GetBytes(thirdBody, "diagnostics.previous_message_id")
	if !previousMessageID.Exists() || previousMessageID.Type != gjson.Null {
		t.Fatalf("third previous_message_id = %s, want null", previousMessageID.Raw)
	}
	if len(thirdBetas) != 1 || thirdBetas[0] != claudeCacheDiagnosticsBeta {
		t.Fatalf("third betas = %v, want diagnostics beta", thirdBetas)
	}
}
