package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
)

func TestAuthenticateManagementKey_LocalhostIPBan_BlocksCorrectKeyDuringBan(t *testing.T) {
	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}

	for i := 0; i < 5; i++ {
		allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "wrong-secret")
		if allowed {
			t.Fatalf("expected auth to be denied at attempt %d", i+1)
		}
		if statusCode != http.StatusUnauthorized || errMsg != "invalid management key" {
			t.Fatalf("unexpected auth failure at attempt %d: status=%d msg=%q", i+1, statusCode, errMsg)
		}
	}

	allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "test-secret")
	if allowed {
		t.Fatalf("expected correct key to be denied while banned")
	}
	if statusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden status while banned, got %d", statusCode)
	}
	if !strings.HasPrefix(errMsg, "IP banned due to too many failed attempts. Try again in") {
		t.Fatalf("unexpected banned message: %q", errMsg)
	}
}

func TestMiddlewareExemptsOnlyTrustedForwardedManagementClient(t *testing.T) {
	handler := &Handler{
		cfg: &config.Config{
			RemoteManagement: config.RemoteManagement{AllowRemote: true},
		},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
		banExemptIP:    "38.244.62.42",
	}
	engine := gin.New()
	if errSetTrustedProxies := engine.SetTrustedProxies([]string{"172.20.0.1"}); errSetTrustedProxies != nil {
		t.Fatalf("SetTrustedProxies() error = %v", errSetTrustedProxies)
	}
	engine.GET("/v0/management/config", handler.Middleware(), func(context *gin.Context) {
		context.Status(http.StatusOK)
	})

	requestManagementConfig := func(remoteAddress string, forwardedClientIP string, managementKey string) int {
		request := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		request.RemoteAddr = remoteAddress
		request.Header.Set("X-Forwarded-For", forwardedClientIP)
		request.Header.Set("Authorization", "Bearer "+managementKey)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		return response.Code
	}

	for attempt := 0; attempt < 5; attempt++ {
		if status := requestManagementConfig("172.20.0.1:12345", "112.116.75.84", "wrong-secret"); status != http.StatusUnauthorized {
			t.Fatalf("other client attempt %d: status = %d, want 401", attempt+1, status)
		}
	}
	if status := requestManagementConfig("172.20.0.1:12345", "112.116.75.84", "test-secret"); status != http.StatusForbidden {
		t.Fatalf("banned client status = %d, want 403", status)
	}
	for attempt := 0; attempt < 6; attempt++ {
		if status := requestManagementConfig("172.20.0.1:12345", "38.244.62.42", "wrong-secret"); status != http.StatusUnauthorized {
			t.Fatalf("exempt client attempt %d: status = %d, want 401", attempt+1, status)
		}
	}
	if status := requestManagementConfig("172.20.0.1:12345", "38.244.62.42", "test-secret"); status != http.StatusOK {
		t.Fatalf("exempt client with valid key: status = %d, want 200", status)
	}
	for attempt := 0; attempt < 5; attempt++ {
		if status := requestManagementConfig("203.0.113.9:12345", "38.244.62.42", "wrong-secret"); status != http.StatusUnauthorized {
			t.Fatalf("untrusted client attempt %d: status = %d, want 401", attempt+1, status)
		}
	}
	if status := requestManagementConfig("203.0.113.9:12345", "38.244.62.42", "test-secret"); status != http.StatusForbidden {
		t.Fatalf("untrusted client spoofing exempt IP: status = %d, want 403", status)
	}
}

func TestMiddlewareSetsSupportPluginHeader(t *testing.T) {

	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}
	middleware := h.Middleware()

	t.Run("invalid key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		c.Request.RemoteAddr = "127.0.0.1:12345"
		c.Request.Header.Set("X-Management-Key", "wrong-secret")

		middleware(c)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})

	t.Run("valid key", func(t *testing.T) {
		engine := gin.New()
		engine.GET("/v0/management/config", middleware, func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Management-Key", "test-secret")
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})
}
