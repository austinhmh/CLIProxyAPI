package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

type testRefreshEvaluator struct{}

func (testRefreshEvaluator) ShouldRefresh(time.Time, *Auth) bool { return false }

func setRefreshLeadFactory(t *testing.T, provider string, factory func() *time.Duration) {
	t.Helper()
	key := strings.ToLower(strings.TrimSpace(provider))
	refreshLeadMu.Lock()
	prev, hadPrev := refreshLeadFactories[key]
	if factory == nil {
		delete(refreshLeadFactories, key)
	} else {
		refreshLeadFactories[key] = factory
	}
	refreshLeadMu.Unlock()
	t.Cleanup(func() {
		refreshLeadMu.Lock()
		if hadPrev {
			refreshLeadFactories[key] = prev
		} else {
			delete(refreshLeadFactories, key)
		}
		refreshLeadMu.Unlock()
	})
}

func TestDisabledAuthStopsAutomaticRefresh(t *testing.T) {
	now := time.Now().UTC()
	expiry := now.Add(30 * time.Minute)
	lead := time.Hour
	setRefreshLeadFactory(t, "disabled-schedule", func() *time.Duration {
		refreshLead := lead
		return &refreshLead
	})

	testCases := []struct {
		name        string
		disableAuth func(*Auth)
	}{
		{
			name: "disabled flag",
			disableAuth: func(auth *Auth) {
				auth.Disabled = true
			},
		},
		{
			name: "disabled status",
			disableAuth: func(auth *Auth) {
				auth.Status = StatusDisabled
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			auth := &Auth{
				ID:       "disabled-schedule-" + strings.ReplaceAll(testCase.name, " ", "-"),
				Provider: "disabled-schedule",
				Status:   StatusActive,
				Metadata: map[string]any{
					"email":      "x@example.com",
					"expires_at": expiry.Format(time.RFC3339),
				},
			}
			manager := NewManager(nil, nil, nil)

			if _, shouldSchedule := nextRefreshCheckAt(now, auth, 15*time.Minute); !shouldSchedule {
				t.Fatal("active auth should be eligible for automatic refresh scheduling")
			}
			if !manager.shouldRefresh(auth, now) {
				t.Fatal("active auth should be eligible for refresh before the disabled state is applied")
			}

			testCase.disableAuth(auth)

			if next, shouldSchedule := nextRefreshCheckAt(now, auth, 15*time.Minute); shouldSchedule || !next.IsZero() {
				t.Fatalf("disabled auth schedule = (%s, %t), want (zero, false)", next, shouldSchedule)
			}
			if manager.shouldRefresh(auth, now) {
				t.Fatal("disabled auth should not be eligible for refresh")
			}
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("register disabled auth: %v", errRegister)
			}
			if manager.markRefreshPending(auth.ID, now) {
				t.Fatal("disabled auth should not be marked as pending refresh")
			}
			storedAuth, ok := manager.GetByID(auth.ID)
			if !ok || storedAuth == nil {
				t.Fatal("registered disabled auth is missing")
			}
			if !storedAuth.NextRefreshAfter.IsZero() {
				t.Fatalf("disabled auth NextRefreshAfter = %s, want zero", storedAuth.NextRefreshAfter)
			}
		})
	}
}

func TestNextRefreshCheckAt_APIKeyUnschedule(t *testing.T) {
	now := time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)
	auth := &Auth{ID: "a1", Provider: "test", Attributes: map[string]string{"api_key": "k"}}
	if _, ok := nextRefreshCheckAt(now, auth, 15*time.Minute); ok {
		t.Fatalf("nextRefreshCheckAt() ok = true, want false")
	}
}

func TestNextRefreshCheckAt_NextRefreshAfterGate(t *testing.T) {
	now := time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)
	nextAfter := now.Add(30 * time.Minute)
	auth := &Auth{
		ID:               "a1",
		Provider:         "test",
		NextRefreshAfter: nextAfter,
		Metadata:         map[string]any{"email": "x@example.com"},
	}
	got, ok := nextRefreshCheckAt(now, auth, 15*time.Minute)
	if !ok {
		t.Fatalf("nextRefreshCheckAt() ok = false, want true")
	}
	if !got.Equal(nextAfter) {
		t.Fatalf("nextRefreshCheckAt() = %s, want %s", got, nextAfter)
	}
}

func TestNextRefreshCheckAt_PreferredInterval_PicksEarliestCandidate(t *testing.T) {
	now := time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(20 * time.Minute)
	auth := &Auth{
		ID:              "a1",
		Provider:        "test",
		LastRefreshedAt: now,
		Metadata: map[string]any{
			"email":                    "x@example.com",
			"expires_at":               expiry.Format(time.RFC3339),
			"refresh_interval_seconds": 900, // 15m
		},
	}
	got, ok := nextRefreshCheckAt(now, auth, 15*time.Minute)
	if !ok {
		t.Fatalf("nextRefreshCheckAt() ok = false, want true")
	}
	want := expiry.Add(-15 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("nextRefreshCheckAt() = %s, want %s", got, want)
	}
}

func TestNextRefreshCheckAt_ProviderLead_Expiry(t *testing.T) {
	now := time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(time.Hour)
	lead := 10 * time.Minute
	setRefreshLeadFactory(t, "provider-lead-expiry", func() *time.Duration {
		d := lead
		return &d
	})

	auth := &Auth{
		ID:       "a1",
		Provider: "provider-lead-expiry",
		Metadata: map[string]any{
			"email":      "x@example.com",
			"expires_at": expiry.Format(time.RFC3339),
		},
	}

	got, ok := nextRefreshCheckAt(now, auth, 15*time.Minute)
	if !ok {
		t.Fatalf("nextRefreshCheckAt() ok = false, want true")
	}
	want := expiry.Add(-lead)
	if !got.Equal(want) {
		t.Fatalf("nextRefreshCheckAt() = %s, want %s", got, want)
	}
}

func TestNextRefreshCheckAt_RefreshEvaluatorFallback(t *testing.T) {
	now := time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)
	interval := 15 * time.Minute
	auth := &Auth{
		ID:       "a1",
		Provider: "test",
		Metadata: map[string]any{"email": "x@example.com"},
		Runtime:  testRefreshEvaluator{},
	}
	got, ok := nextRefreshCheckAt(now, auth, interval)
	if !ok {
		t.Fatalf("nextRefreshCheckAt() ok = false, want true")
	}
	want := now.Add(interval)
	if !got.Equal(want) {
		t.Fatalf("nextRefreshCheckAt() = %s, want %s", got, want)
	}
}
