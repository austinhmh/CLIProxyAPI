package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestGetRoutingInsights_ReturnsHashPreview(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, &coreauth.BalancedHashSelector{}, nil)
	_, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "auth-insights-1",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": "insights@example.com"},
	})
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := &Handler{authManager: manager}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/routing-insights?window=5m&provider=claude&model=claude-sonnet-4-6&idempotency_key=test-key", nil)
	h.GetRoutingInsights(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if _, ok := payload["hash_preview"]; !ok {
		t.Fatalf("missing hash_preview: %#v", payload)
	}
	if _, ok := payload["active_session_bindings"]; !ok {
		t.Fatalf("missing active_session_bindings: %#v", payload)
	}
	if _, ok := payload["balance_metrics"]; !ok {
		t.Fatalf("missing balance_metrics: %#v", payload)
	}
}

func TestGetSessionMonitorPageUsesTextOnlyCells(t *testing.T) {
	gateway := &Handler{}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/v0/management/routing-insights/monitor", nil)
	gateway.GetSessionMonitorPage(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("monitor status = %d, want 200", recorder.Code)
	}
	page := recorder.Body.String()
	if !strings.Contains(page, "cell.textContent") || strings.Contains(page, "innerHTML") {
		t.Fatal("monitor must not inject upstream response values as HTML")
	}
	if !strings.Contains(page, "/v0/management/routing-insights?") {
		t.Fatal("monitor fetch path does not match the registered management API")
	}
}
