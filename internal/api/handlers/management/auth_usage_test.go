package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestListAuthFileUsage_ClaudeReturnsOneEntry(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, &coreauth.BalancedHashSelector{}, nil)
	auth := &coreauth.Auth{ID: "auth-claude-1", Provider: "claude", FileName: "claude-1.json"}
	auth.RateLimits = map[string]any{
		"7d_utilization": 12,
		"7d_reset":       "2026-08-17T05:29:20Z",
		"5h_utilization": 40,
		"5h_reset":       "2026-08-10T20:22:39Z",
		"observed_at":    "2026-08-10T18:00:00Z",
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	usage := listAuthFileUsage(t, manager)
	if len(usage) != 1 {
		t.Fatalf("len(usage) = %d, want 1: %+v", len(usage), usage)
	}
	entry := usage[0]
	if entry["name"] != "claude-1.json" {
		t.Fatalf("name = %v, want claude-1.json", entry["name"])
	}
	if entry["type"] != "claude" {
		t.Fatalf("type = %v, want claude", entry["type"])
	}
	if _, hasGroup := entry["group"]; hasGroup {
		t.Fatalf("claude entry should not have a group field: %+v", entry)
	}
}

func TestListAuthFileUsage_AntigravityReturnsTwoEntries(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, &coreauth.BalancedHashSelector{}, nil)
	auth := &coreauth.Auth{ID: "auth-antigravity-1", Provider: "antigravity", FileName: "antigravity-1.json"}
	geminiUtil, thirdPartyUtil := 3, 100
	groups := []coreauth.AntigravityQuotaGroup{
		{
			GroupID:     "gemini",
			DisplayName: "Gemini Models",
			Long:        &coreauth.AntigravityQuotaWindow{Utilization: &geminiUtil, Reset: "2026-08-17T05:29:20Z"},
		},
		{
			GroupID:     "3p",
			DisplayName: "Third-Party Models",
			Long:        &coreauth.AntigravityQuotaWindow{Utilization: &thirdPartyUtil, Reset: "2026-08-15T13:17:37Z"},
		},
	}
	if !coreauth.SetAntigravityQuotaGroups(auth, groups, time.Now()) {
		t.Fatalf("SetAntigravityQuotaGroups = false, want true")
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	usage := listAuthFileUsage(t, manager)
	if len(usage) != 2 {
		t.Fatalf("len(usage) = %d, want 2: %+v", len(usage), usage)
	}

	byName := make(map[string]map[string]any)
	for _, entry := range usage {
		name, _ := entry["name"].(string)
		byName[name] = entry
	}

	gemini, ok := byName["antigravity-1.json (gemini)"]
	if !ok {
		t.Fatalf("missing gemini entry, got names: %+v", byName)
	}
	if gemini["id"] != "auth-antigravity-1" || gemini["type"] != "antigravity-gemini" || gemini["group"] != "gemini" {
		t.Fatalf("gemini entry = %+v", gemini)
	}
	window7d, ok := gemini["usage_7d"].(map[string]any)
	if !ok || int(window7d["percent"].(float64)) != 3 {
		t.Fatalf("gemini usage_7d = %+v", gemini["usage_7d"])
	}

	thirdParty, ok := byName["antigravity-1.json (3p)"]
	if !ok {
		t.Fatalf("missing 3p entry, got names: %+v", byName)
	}
	if thirdParty["id"] != "auth-antigravity-1" || thirdParty["type"] != "antigravity-3p" || thirdParty["group"] != "3p" {
		t.Fatalf("3p entry = %+v", thirdParty)
	}
}

func TestListAuthFileUsage_ClaudeFableWindowReturnsSeparateEntry(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, &coreauth.BalancedHashSelector{}, nil)
	auth := &coreauth.Auth{ID: "auth-claude-2", Provider: "claude", FileName: "claude-2.json"}
	auth.RateLimits = map[string]any{
		"7d_utilization":    12,
		"7d_reset":          "2026-08-17T05:29:20Z",
		"5h_utilization":    40,
		"5h_reset":          "2026-08-10T20:22:39Z",
		"7d_oi_utilization": 9,
		"7d_oi_reset":       "2026-08-31T00:00:00Z",
		"7d_oi_status":      "allowed",
		"observed_at":       "2026-08-10T18:00:00Z",
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	usage := listAuthFileUsage(t, manager)
	if len(usage) != 2 {
		t.Fatalf("len(usage) = %d, want 2: %+v", len(usage), usage)
	}

	byName := make(map[string]map[string]any)
	for _, entry := range usage {
		entryName, _ := entry["name"].(string)
		byName[entryName] = entry
	}

	main, ok := byName["claude-2.json"]
	if !ok {
		t.Fatalf("missing main entry, got names: %+v", byName)
	}
	if main["type"] != "claude" {
		t.Fatalf("main type = %v, want claude", main["type"])
	}
	mainWindow7d, ok := main["usage_7d"].(map[string]any)
	if !ok || int(mainWindow7d["percent"].(float64)) != 12 {
		t.Fatalf("main usage_7d = %+v", main["usage_7d"])
	}

	fable, ok := byName["claude-2.json (claude-fable)"]
	if !ok {
		t.Fatalf("missing claude-fable entry, got names: %+v", byName)
	}
	if fable["id"] != "auth-claude-2" || fable["type"] != "claude-fable" || fable["group"] != "claude-fable" {
		t.Fatalf("fable entry = %+v", fable)
	}
	fableWindow7d, ok := fable["usage_7d"].(map[string]any)
	if !ok || int(fableWindow7d["percent"].(float64)) != 9 || fableWindow7d["reset_at"] != "2026-08-31T00:00:00Z" {
		t.Fatalf("fable usage_7d = %+v", fable["usage_7d"])
	}
	if _, has5h := fable["usage_5h"]; has5h {
		t.Fatalf("fable entry should not have usage_5h: %+v", fable)
	}
	if fable["observed_at"] != "2026-08-10T18:00:00Z" {
		t.Fatalf("fable observed_at = %v", fable["observed_at"])
	}
}

// listAuthFileUsage calls ListAuthFileUsage through the HTTP handler and
// decodes the "usage" array, exercising sorting and JSON encoding along with
// buildAuthUsageEntries.
func listAuthFileUsage(t *testing.T, manager *coreauth.Manager) []map[string]any {
	t.Helper()
	h := &Handler{authManager: manager}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/usage", nil)
	h.ListAuthFileUsage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Usage []map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return payload.Usage
}
