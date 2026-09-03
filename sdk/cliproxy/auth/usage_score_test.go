package auth

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func floatsClose(a, b, tolerance float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= tolerance
}

func TestUsageWindowScore_FullHeadroom(t *testing.T) {
	t.Parallel()

	now := time.Now()
	if got := usageWindowScore(0, "", now); got != 1 {
		t.Fatalf("usageWindowScore(0, \"\", now) = %v, want 1 (full headroom, no reset)", got)
	}
}

func TestUsageWindowScore_ExhaustedNoReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	if got := usageWindowScore(100, "", now); got != 0 {
		t.Fatalf("usageWindowScore(100, \"\", now) = %v, want 0", got)
	}
}

func TestUsageWindowScore_ExhaustedWithImminentReset(t *testing.T) {
	t.Parallel()

	// A fully exhausted window resetting in 2 minutes must score close to a
	// fresh window: urgency dominates headroom instead of being diluted by it.
	now := time.Now()
	resetAt := now.Add(2 * time.Minute).UTC().Format(time.RFC3339)
	got := usageWindowScore(100, resetAt, now)
	if got <= 0.9 || got > 1 {
		t.Fatalf("usageWindowScore(100, +2m, now) = %v, want in (0.9, 1]", got)
	}
}

func TestUsageWindowScore_DistantResetBarelyHelps(t *testing.T) {
	t.Parallel()

	now := time.Now()
	resetAt := now.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	got := usageWindowScore(100, resetAt, now)
	if got <= 0 || got >= 0.01 {
		t.Fatalf("usageWindowScore(100, +30d, now) = %v, want in (0, 0.01)", got)
	}
}

func TestUsageWindowScore_ClampsOverage(t *testing.T) {
	t.Parallel()

	now := time.Now()
	if got := usageWindowScore(113, "", now); got != 0 {
		t.Fatalf("usageWindowScore(113, \"\", now) = %v, want 0 (clamped)", got)
	}
}

func TestUsageWindowScore_PastResetNoBonus(t *testing.T) {
	t.Parallel()

	now := time.Now()
	resetAt := now.Add(-time.Minute).UTC().Format(time.RFC3339)
	if got := usageWindowScore(100, resetAt, now); got != 0 {
		t.Fatalf("usageWindowScore(100, past reset, now) = %v, want 0", got)
	}
}

// TestUsageWindowScore_TypicalScenarios tabulates representative
// utilization/reset combinations to show the max(headroom, urgency) scoring
// (see usageWindowScore) in action, e.g. "90% used but resets in 10m" scores
// far higher than "90% used but resets tomorrow" (0.86 vs 0.10) even though
// headroom is identical in both, because an imminent reset overwhelms
// headroom instead of being averaged with it.
func TestUsageWindowScore_TypicalScenarios(t *testing.T) {
	t.Parallel()

	// NOTE: truncate so resetAt's seconds-only RFC3339 formatting round-trips exactly.
	now := time.Now().Truncate(time.Second)
	tests := []struct {
		name        string
		utilization int
		resetIn     time.Duration // 0 means no reset info at all
		want        float64
	}{
		{name: "90% used, resets tomorrow", utilization: 90, resetIn: 24 * time.Hour, want: 0.100000},
		{name: "90% used, resets in 10m", utilization: 90, resetIn: 10 * time.Minute, want: 0.857143},
		{name: "90% used, no reset info", utilization: 90, resetIn: 0, want: 0.100000},
		{name: "50% used, resets in 7d", utilization: 50, resetIn: 7 * 24 * time.Hour, want: 0.500000},
		{name: "10% used, resets in 7d", utilization: 10, resetIn: 7 * 24 * time.Hour, want: 0.900000},
		{name: "100% used, resets in 1h", utilization: 100, resetIn: time.Hour, want: 0.500000},
		{name: "100% used, resets in 5m", utilization: 100, resetIn: 5 * time.Minute, want: 0.923077},
		{name: "0% used, no reset info", utilization: 0, resetIn: 0, want: 1.000000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var resetAt string
			if tc.resetIn > 0 {
				resetAt = now.Add(tc.resetIn).UTC().Format(time.RFC3339)
			}
			got := usageWindowScore(tc.utilization, resetAt, now)
			if !floatsClose(got, tc.want, 1e-4) {
				t.Fatalf("usageWindowScore(%d%%, +%s) = %v, want ~%v", tc.utilization, tc.resetIn, got, tc.want)
			}
		})
	}
}

// TestUsageWindowScore_UrgencyOverridesHeadroomNearReset holds utilization
// fixed at 95% (0.05 headroom) and sweeps the reset distance from minutes to
// days. While reset is imminent, urgency overwhelms the poor headroom; once
// reset is far enough out that urgency drops below 0.05, the score falls back
// to plain headroom and stays flat.
func TestUsageWindowScore_UrgencyOverridesHeadroomNearReset(t *testing.T) {
	t.Parallel()

	// NOTE: truncate so resetAt's seconds-only RFC3339 formatting round-trips exactly.
	now := time.Now().Truncate(time.Second)
	tests := []struct {
		name    string
		resetIn time.Duration
		want    float64
	}{
		{name: "10m", resetIn: 10 * time.Minute, want: 0.857143},
		{name: "1h", resetIn: time.Hour, want: 0.500000},
		{name: "12h", resetIn: 12 * time.Hour, want: 0.076923},
		{name: "1d", resetIn: 24 * time.Hour, want: 0.050000},
		{name: "3d", resetIn: 3 * 24 * time.Hour, want: 0.050000},
		{name: "7d", resetIn: 7 * 24 * time.Hour, want: 0.050000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetAt := now.Add(tc.resetIn).UTC().Format(time.RFC3339)
			got := usageWindowScore(95, resetAt, now)
			if !floatsClose(got, tc.want, 1e-4) {
				t.Fatalf("usageWindowScore(95%%, +%s) = %v, want ~%v", tc.resetIn, got, tc.want)
			}
		})
	}
}

// TestUsageWindowScore_DominanceMatrix has no assertions: it's a tool for
// inspecting how max(headroom, resetBonus) behaves, not a regression check.
// Run it with:
//
//	go test ./sdk/cliproxy/auth/... -run TestUsageWindowScore_DominanceMatrix -v
//
// to print the score grid directly instead of hand-computing points after
// every change to usageWindowScore or resetBonus. Utilization rows are
// weighted toward the high end and reset columns toward the near term,
// because that is the only region where resetBonus can exceed headroom and
// take over the score; elsewhere the grid is flat and headroom-only.
func TestUsageWindowScore_DominanceMatrix(t *testing.T) {
	t.Parallel()

	now := time.Now()
	utilizations := []int{0, 50, 80, 90, 95, 98, 99, 100}
	resets := []struct {
		label string
		in    time.Duration
	}{
		{"now", 0},
		{"10m", 10 * time.Minute},
		{"1h", time.Hour},
		{"6h", 6 * time.Hour},
		{"12h", 12 * time.Hour},
		{"1d", 24 * time.Hour},
		{"3d", 3 * 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nscore = max(headroom, resetBonus)\n")
	fmt.Fprintf(&b, "%-6s", "util\\t")
	for _, r := range resets {
		fmt.Fprintf(&b, "%8s", r.label)
	}
	fmt.Fprintln(&b)
	for _, u := range utilizations {
		fmt.Fprintf(&b, "%-6s", fmt.Sprintf("%d%%", u))
		for _, r := range resets {
			var resetAt string
			if r.in > 0 {
				resetAt = now.Add(r.in).UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(&b, "%8.4f", usageWindowScore(u, resetAt, now))
		}
		fmt.Fprintln(&b)
	}
	t.Log(b.String())
}

func TestUsageAwareScore_NoData_ReturnsFallback(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "a", Provider: "gemini"}
	if got := usageAwareScore(auth, time.Now()); got != defaultUsageAwareScore {
		t.Fatalf("usageAwareScore(no data) = %v, want %v", got, defaultUsageAwareScore)
	}
}

func TestUsageAwareScore_UnparsedRateLimitValue_ReturnsFallback(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "a", Provider: "claude", RateLimits: map[string]any{"7d_utilization": "n/a"}}
	if got := usageAwareScore(auth, time.Now()); got != defaultUsageAwareScore {
		t.Fatalf("usageAwareScore(unparsed) = %v, want %v", got, defaultUsageAwareScore)
	}
}

func TestUsageAwareScore_ClaudeIgnores5hWindow(t *testing.T) {
	t.Parallel()

	// The 5h and 7d windows aren't on the same scale, so a lone 5h reading must
	// not feed the score; only 7d (and 7d_oi) windows should.
	auth := &Auth{ID: "a", Provider: "claude", RateLimits: map[string]any{"5h_utilization": 95}}
	if got := usageAwareScore(auth, time.Now()); got != defaultUsageAwareScore {
		t.Fatalf("usageAwareScore(5h only) = %v, want %v", got, defaultUsageAwareScore)
	}
}

func TestUsageAwareScore_ClaudeMultiWindowTakesWorst(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{
		ID:       "a",
		Provider: "claude",
		RateLimits: map[string]any{
			"7d_utilization":    20,
			"7d_oi_utilization": 80,
		},
	}
	got := usageAwareScore(auth, now)
	want := usageWindowScore(80, "", now)
	if got != want {
		t.Fatalf("usageAwareScore(mixed windows) = %v, want worst window score %v", got, want)
	}
}

func TestUsageAwareScore_CodexWindows(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{
		ID:       "a",
		Provider: "codex",
		RateLimits: map[string]any{
			"primary_used_percent":   10,
			"secondary_used_percent": 5,
		},
	}
	got := usageAwareScore(auth, now)
	// primary (10%) has less headroom than secondary (5%), so it is the
	// binding/worst window and should determine the score.
	want := usageWindowScore(10, "", now)
	if got != want {
		t.Fatalf("usageAwareScore(codex) = %v, want %v", got, want)
	}
}

func TestUsageAwareScore_AntigravityWindows(t *testing.T) {
	t.Parallel()

	now := time.Now()
	weeklyUtil := 30
	shortUtil := 90
	auth := &Auth{ID: "a", Provider: "antigravity"}
	groups := []AntigravityQuotaGroup{
		{
			GroupID: "gemini",
			Long:    &AntigravityQuotaWindow{Utilization: &weeklyUtil},
			Short:   &AntigravityQuotaWindow{Utilization: &shortUtil},
		},
	}
	if !SetAntigravityQuotaGroups(auth, groups, now) {
		t.Fatalf("SetAntigravityQuotaGroups() = false, want true")
	}
	got := usageAwareScore(auth, now)
	want := usageWindowScore(shortUtil, "", now)
	if got != want {
		t.Fatalf("usageAwareScore(antigravity) = %v, want worst window score %v", got, want)
	}
}

func TestUsageAwareScore_AntigravityWindowsWithNilUtilization(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{ID: "a", Provider: "antigravity"}
	groups := []AntigravityQuotaGroup{
		{GroupID: "gemini", Long: &AntigravityQuotaWindow{Reset: now.Add(time.Hour).UTC().Format(time.RFC3339)}},
	}
	if !SetAntigravityQuotaGroups(auth, groups, now) {
		t.Fatalf("SetAntigravityQuotaGroups() = false, want true")
	}
	// The only window has no Utilization value, so it contributes nothing and the
	// credential must fall back to the neutral score rather than treating a
	// reset-only window as 0% or 100% utilized.
	if got := usageAwareScore(auth, now); got != defaultUsageAwareScore {
		t.Fatalf("usageAwareScore(nil utilization) = %v, want %v", got, defaultUsageAwareScore)
	}
}
