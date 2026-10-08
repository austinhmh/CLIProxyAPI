package management

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ListAuthFileUsage returns the most recently observed rate-limit/usage snapshot
// for each auth credential. Claude/Codex usage comes from upstream response
// headers (Claude: Anthropic-Ratelimit-Unified-*; Codex: X-Codex-Primary/Secondary-*).
// Claude's second 7d window (Anthropic-Ratelimit-Unified-7d_oi-*, reported for
// the claude-fable model family) is split into its own entry tagged with type
// "claude-fable", mirroring how antigravity splits its quota groups.
// Antigravity usage comes from a dedicated retrieveUserQuotaSummary probe (see
// sdk/cliproxy/auth/antigravity_quota.go) and, since that endpoint reports
// independent quota groups (currently "gemini" and "3p"), yields one entry per
// group for a single antigravity auth rather than one entry per auth, each
// tagged with its own "type" (e.g. "antigravity-gemini", "antigravity-3p").
// The data is in-memory only (sdk/cliproxy/auth.Auth.RateLimits, not persisted to
// disk) and reflects whatever was last observed since the process started; auths
// with no recorded usage yet are omitted from the response.
func (h *Handler) ListAuthFileUsage(c *gin.Context) {
	if h == nil {
		c.JSON(500, gin.H{"error": "handler not initialized"})
		return
	}
	if h.authManager == nil {
		c.JSON(200, gin.H{"usage": []gin.H{}})
		return
	}
	auths := h.authManager.List()
	usage := make([]gin.H, 0, len(auths))
	for _, auth := range auths {
		usage = append(usage, buildAuthUsageEntries(auth)...)
	}
	sort.Slice(usage, func(i, j int) bool {
		nameI, _ := usage[i]["name"].(string)
		nameJ, _ := usage[j]["name"].(string)
		return strings.ToLower(nameI) < strings.ToLower(nameJ)
	})
	c.JSON(200, gin.H{"usage": usage})
}

// buildAuthUsageEntries returns the usage entries for one auth: zero when it
// has no recorded usage yet, one for Codex, one per antigravity quota group,
// or for Claude one main entry plus one per extra tracked window group (e.g.
// the 7d_oi window reported for the claude-fable family — see
// coreauth.ClaudeRateLimitFableGroup).
func buildAuthUsageEntries(auth *coreauth.Auth) []gin.H {
	if auth == nil || len(auth.RateLimits) == 0 {
		return nil
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = auth.ID
	}
	if name == "" {
		return nil
	}

	switch strings.TrimSpace(auth.Provider) {
	case "claude":
		entries := authUsageEntrySlice(auth, name,
			rateLimitWindow(auth.RateLimits, "7d_utilization", "7d_reset"),
			rateLimitWindow(auth.RateLimits, "5h_utilization", "5h_reset"))
		return append(entries, claudeGroupUsageEntries(auth, name)...)
	case "codex":
		// Codex's "primary" window is the long/weekly window and "secondary" is
		// the short window; both map onto the 7d/5h response shape for display.
		return authUsageEntrySlice(auth, name,
			rateLimitWindow(auth.RateLimits, "primary_used_percent", "primary_reset_at"),
			rateLimitWindow(auth.RateLimits, "secondary_used_percent", "secondary_reset_at"))
	case "antigravity":
		return antigravityUsageEntries(auth, name)
	default:
		return nil
	}
}

// authUsageEntrySlice builds the single Claude/Codex usage entry, wrapped in a
// slice so callers can treat every provider's result uniformly. It returns nil
// when neither window has data.
func authUsageEntrySlice(auth *coreauth.Auth, name string, window7d, window5h gin.H) []gin.H {
	if window7d == nil && window5h == nil {
		return nil
	}
	entry := gin.H{
		"id":   auth.ID,
		"name": name,
		"type": strings.TrimSpace(auth.Provider),
	}
	if window7d != nil {
		entry["usage_7d"] = window7d
	}
	if window5h != nil {
		entry["usage_5h"] = window5h
	}
	if observedAt, ok := auth.RateLimits["observed_at"].(string); ok && observedAt != "" {
		entry["observed_at"] = observedAt
	}
	return []gin.H{entry}
}

// claudeGroupUsageEntries builds one usage entry per extra Claude window group
// stored alongside the main 5h/7d snapshot. Anthropic reports a second 7d
// window for the claude-fable model family under the 7d_oi header prefix; like
// the antigravity groups it is split into its own entry named
// "<name> (<group>)" with its own "type" so the independent tracks don't
// collide under the same entry.
func claudeGroupUsageEntries(auth *coreauth.Auth, name string) []gin.H {
	window := rateLimitWindow(auth.RateLimits, "7d_oi_utilization", "7d_oi_reset")
	if window == nil {
		return nil
	}
	entry := gin.H{
		"id":    auth.ID,
		"name":  fmt.Sprintf("%s (%s)", name, coreauth.ClaudeRateLimitFableGroup),
		"type":  coreauth.ClaudeRateLimitFableGroup,
		"group": coreauth.ClaudeRateLimitFableGroup,
	}
	entry["usage_7d"] = window
	if observedAt, ok := auth.RateLimits["observed_at"].(string); ok && observedAt != "" {
		entry["observed_at"] = observedAt
	}
	return []gin.H{entry}
}

// antigravityUsageEntries builds one usage entry per antigravity quota group
// (see coreauth.AntigravityQuotaGroups), naming each "<name> (<GroupID>)" (e.g.
// "xxx.json (gemini)", "xxx.json (3p)") and tagging each with its own
// "type" ("antigravity-<GroupID>", e.g. "antigravity-gemini",
// "antigravity-3p") so the independent quota tracks don't collide under the
// same entry. Groups with neither window populated are skipped.
func antigravityUsageEntries(auth *coreauth.Auth, name string) []gin.H {
	groups := coreauth.AntigravityQuotaGroups(auth)
	if len(groups) == 0 {
		return nil
	}
	observedAt, _ := auth.RateLimits["observed_at"].(string)

	entries := make([]gin.H, 0, len(groups))
	for _, group := range groups {
		window7d := antigravityQuotaWindow(group.Long)
		window5h := antigravityQuotaWindow(group.Short)
		if window7d == nil && window5h == nil {
			continue
		}
		entry := gin.H{
			"id":    auth.ID,
			"name":  fmt.Sprintf("%s (%s)", name, group.GroupID),
			"type":  fmt.Sprintf("antigravity-%s", group.GroupID),
			"group": group.GroupID,
		}
		if window7d != nil {
			entry["usage_7d"] = window7d
		}
		if window5h != nil {
			entry["usage_5h"] = window5h
		}
		if observedAt != "" {
			entry["observed_at"] = observedAt
		}
		entries = append(entries, entry)
	}
	return entries
}

// antigravityQuotaWindow converts a typed antigravity quota window into the
// same {percent, reset_at} shape rateLimitWindow produces for Claude/Codex. It
// returns nil when w is nil or has neither field populated.
func antigravityQuotaWindow(w *coreauth.AntigravityQuotaWindow) gin.H {
	if w == nil {
		return nil
	}
	window := gin.H{}
	if w.Utilization != nil {
		window["percent"] = *w.Utilization
	}
	if w.Reset != "" {
		window["reset_at"] = w.Reset
	}
	if len(window) == 0 {
		return nil
	}
	return window
}

// rateLimitWindow extracts a {percent, reset_at} pair from a rate-limit snapshot.
// It returns nil when neither value is present.
func rateLimitWindow(snapshot map[string]any, percentKey, resetKey string) gin.H {
	window := gin.H{}
	if percent, ok := rateLimitInt(snapshot, percentKey); ok {
		window["percent"] = percent
	}
	if resetAt, ok := snapshot[resetKey].(string); ok && resetAt != "" {
		window["reset_at"] = resetAt
	}
	if len(window) == 0 {
		return nil
	}
	return window
}

// rateLimitInt reads a numeric rate-limit snapshot value. Values are normally
// stored as int (see setRateLimitInt in rate_limit_headers.go), but this also
// accepts float64/string in case the value ever round-trips through JSON or an
// upstream header did not contain a plain integer.
func rateLimitInt(snapshot map[string]any, key string) (int, bool) {
	switch v := snapshot[key].(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n, true
		}
	}
	return 0, false
}
