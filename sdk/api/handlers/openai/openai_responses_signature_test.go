package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func newResponsesCaptureRouter(t *testing.T, modelName string) (*compactCaptureExecutor, *gin.Engine) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	executor := &compactCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: modelName + "-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: modelName}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	handler := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", handler.Responses)
	return executor, router
}

func TestOpenAIResponsesPOSTPrefersUpstreamWebsocket(t *testing.T) {
	const modelName = "test-responses-websocket-preference"
	executor, router := newResponsesCaptureRouter(t, modelName)

	requestBody := `{"model":"` + modelName + `","stream":false,"input":"hello"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls)
	}
	if !executor.preferUpstreamWebsocket {
		t.Fatal("POST /v1/responses did not prefer an upstream websocket")
	}
}

func TestOpenAIResponsesStreamingPOSTPrefersUpstreamWebsocket(t *testing.T) {
	const modelName = "test-responses-stream-websocket-preference"
	executor, router := newResponsesCaptureRouter(t, modelName)

	requestBody := `{"model":"` + modelName + `","stream":true,"input":"hello"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if executor.streamCalls != 1 {
		t.Fatalf("stream executor calls = %d, want 1", executor.streamCalls)
	}
	if !executor.preferUpstreamWebsocket {
		t.Fatal("streaming POST /v1/responses did not prefer an upstream websocket")
	}
	if !strings.Contains(response.Body.String(), `"type":"response.completed"`) {
		t.Fatalf("stream response did not contain completion event: %s", response.Body.String())
	}
}

func TestOpenAIResponsesForwardsInvalidReasoningEncryptedContentToExecutor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &compactCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "signature-auth-responses", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-signature-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)

	body := `{"model":"test-signature-model","stream":false,"input":[{"id":"rs_bad","type":"reasoning","encrypted_content":"gAAAAABqFTIa\u2026abc","summary":[]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls)
	}
}

func TestOpenAIResponsesCompactForwardsInvalidReasoningEncryptedContentToExecutor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &compactCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "signature-auth-compact", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-signature-compact-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses/compact", h.Compact)

	body := `{"model":"test-signature-compact-model","input":[{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls)
	}
	if executor.alt != "responses/compact" {
		t.Fatalf("alt = %q, want responses/compact", executor.alt)
	}
}
