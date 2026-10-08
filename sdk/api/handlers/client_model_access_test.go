package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func newClientModelAccessContext(t *testing.T, apiKey string) context.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginContext.Set("userApiKey", apiKey)

	return context.WithValue(context.Background(), "gin", ginContext)
}

func TestValidateClientModelAccessMatchesCanonicalModelVariants(t *testing.T) {
	handler := &BaseAPIHandler{
		Cfg: &sdkconfig.SDKConfig{
			APIKeyModelRules: []sdkconfig.APIKeyModelRule{
				{
					APIKey:         "sk-client-a",
					ExcludedModels: []string{"gpt-5", "claude-opus-*"},
				},
			},
		},
	}
	ctx := newClientModelAccessContext(t, "sk-client-a")

	if errMsg := handler.validateClientModelAccess(ctx, "gpt-5(high)"); errMsg == nil {
		t.Fatal("expected reasoning suffix form to be denied")
	}

	if errMsg := handler.validateClientModelAccess(ctx, "client-alias", "team/claude-opus-4-8"); errMsg == nil {
		t.Fatal("expected normalized upstream model to be denied")
	}

	if errMsg := handler.validateClientModelAccess(ctx, "gpt-4"); errMsg != nil {
		t.Fatalf("unexpected denial for allowed model: %+v", errMsg)
	}

	otherClientContext := newClientModelAccessContext(t, "sk-client-b")
	if errMsg := handler.validateClientModelAccess(otherClientContext, "gpt-5"); errMsg != nil {
		t.Fatalf("unexpected denial for another client key: %+v", errMsg)
	}
}

func TestValidateClientModelAccessWithoutRulesAllowsEverything(t *testing.T) {
	handler := &BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{}}
	ctx := newClientModelAccessContext(t, "sk-client-a")

	if errMsg := handler.validateClientModelAccess(ctx, "gpt-5"); errMsg != nil {
		t.Fatalf("unexpected denial when no rules configured: %+v", errMsg)
	}
}

func TestExecuteModelRejectsClientExcludedModelBeforeExecutor(t *testing.T) {
	model := "gpt-5"
	executor := &modelExecutionCaptureExecutor{}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{
		APIKeyModelRules: []sdkconfig.APIKeyModelRule{
			{
				APIKey:         "sk-client-a",
				ExcludedModels: []string{model},
			},
		},
	})

	ctx := newClientModelAccessContext(t, "sk-client-a")
	_, errMsg := handler.ExecuteModel(ctx, ModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         model,
		Body:          []byte(`{"model":"gpt-5"}`),
	})
	if errMsg == nil {
		t.Fatal("expected model access denial")
	}
	if errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusForbidden)
	}
	if !errMsg.DirectResponse {
		t.Fatal("expected direct model access error response")
	}
	if !bytes.Contains(errMsg.Body, []byte(`"code":"model_not_allowed"`)) {
		t.Fatalf("error body = %s, want model_not_allowed", errMsg.Body)
	}

	capturedRequest, _ := executor.captured()
	if capturedRequest.Model != "" {
		t.Fatalf("executor received denied request for model %q", capturedRequest.Model)
	}
}

func TestExecuteModelStreamRejectsClientExcludedModelBeforeExecutor(t *testing.T) {
	model := "gpt-5"
	executor := &modelExecutionCaptureExecutor{}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{
		APIKeyModelRules: []sdkconfig.APIKeyModelRule{
			{
				APIKey:         "sk-client-a",
				ExcludedModels: []string{model},
			},
		},
	})

	ctx := newClientModelAccessContext(t, "sk-client-a")
	_, errMsg := handler.ExecuteModelStream(ctx, ModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         model,
		Stream:        true,
		Body:          []byte(`{"model":"gpt-5","stream":true}`),
	})
	if errMsg == nil {
		t.Fatal("expected model access denial")
	}
	if errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusForbidden)
	}

	capturedRequest, _ := executor.captured()
	if capturedRequest.Model != "" {
		t.Fatalf("executor received denied stream request for model %q", capturedRequest.Model)
	}
}

func TestExecuteCountWithAuthManagerRejectsClientExcludedModelBeforeExecutor(t *testing.T) {
	model := "gpt-5"
	executor := &modelExecutionCaptureExecutor{}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{
		APIKeyModelRules: []sdkconfig.APIKeyModelRule{
			{
				APIKey:         "sk-client-a",
				ExcludedModels: []string{model},
			},
		},
	})

	ctx := newClientModelAccessContext(t, "sk-client-a")
	_, _, errMsg := handler.ExecuteCountWithAuthManager(ctx, "openai", model, []byte(`{"model":"gpt-5"}`), "")
	if errMsg == nil {
		t.Fatal("expected model access denial")
	}
	if errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusForbidden)
	}
}
