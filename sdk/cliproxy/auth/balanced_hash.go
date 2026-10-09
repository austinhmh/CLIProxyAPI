package auth

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// BalancedHashSelector spreads load with a deterministic hash while preferring
// fresher, higher-quota credentials and lightly penalizing recent failures.
type BalancedHashSelector struct{}

// Pick selects the highest-scoring available auth for the request.
func (s *BalancedHashSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if len(available) == 1 {
		return available[0], nil
	}

	requestKey := idempotencyKeyFromMetadata(opts.Metadata)
	if requestKey == "" {
		requestKey = fmt.Sprintf("%d", now.UnixNano())
	}
	modelKey := canonicalModelKey(model)
	if modelKey == "" {
		modelKey = strings.TrimSpace(model)
	}

	bestIndex := 0
	bestScore := -1.0
	for i := 0; i < len(available); i++ {
		candidate := available[i]
		score := balancedAuthScore(candidate, model, modelKey, requestKey, now)
		if score > bestScore {
			bestScore = score
			bestIndex = i
			continue
		}
		if score == bestScore && strings.Compare(candidate.ID, available[bestIndex].ID) < 0 {
			bestIndex = i
		}
	}
	return available[bestIndex], nil
}

func idempotencyKeyFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta["idempotency_key"]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func balancedAuthScore(auth *Auth, model, modelKey, requestKey string, now time.Time) float64 {
	const (
		weightHash      = 0.40
		weightFreshness = 0.25
		weightQuota     = 0.25
		weightPenalty   = 0.10
	)
	hashScore := normalizedHashScore(requestKey + "|" + modelKey + "|" + strings.TrimSpace(auth.ID))
	freshness := authFreshnessScore(auth, model, now)
	quota := authQuotaScore(auth, model)
	penalty := authRecentPenalty(auth, model)
	return (weightHash * hashScore) + (weightFreshness * freshness) + (weightQuota * quota) + (weightPenalty * (1.0 - penalty))
}

func normalizedHashScore(seed string) float64 {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(seed))
	sum := hasher.Sum64()
	return float64(sum) / float64(^uint64(0))
}

func authFreshnessScore(auth *Auth, model string, now time.Time) float64 {
	state := modelStateForScore(auth, model)
	if state == nil {
		return 0.5
	}
	next := state.NextRetryAfter
	if next.IsZero() {
		return 0.9
	}
	diff := next.Sub(now)
	if diff < 0 {
		diff = -diff
	}
	minutes := diff.Minutes()
	if minutes <= 0 {
		return 1.0
	}
	score := 1.0 / (1.0 + (minutes / 30.0))
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

func authQuotaScore(auth *Auth, model string) float64 {
	state := modelStateForScore(auth, model)
	quota := auth.Quota
	if state != nil {
		quota = state.Quota
	}
	if !quota.Exceeded {
		return 1.0
	}
	backoff := quota.BackoffLevel
	if backoff < 0 {
		backoff = 0
	}
	score := 1.0 - (float64(backoff) / 10.0)
	if score < 0 {
		return 0
	}
	return score
}

func authRecentPenalty(auth *Auth, model string) float64 {
	state := modelStateForScore(auth, model)
	if state == nil || state.UpdatedAt.IsZero() {
		return 0.0
	}
	// Very recent failure gets a small penalty so we spread away from hot entries.
	if state.LastError != nil && time.Since(state.UpdatedAt) < 15*time.Second {
		return 1.0
	}
	return 0.0
}

func modelStateForScore(auth *Auth, model string) *ModelState {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	if model != "" {
		if state, ok := auth.ModelStates[model]; ok && state != nil {
			return state
		}
		baseModel := canonicalModelKey(model)
		if baseModel != "" {
			if state, ok := auth.ModelStates[baseModel]; ok && state != nil {
				return state
			}
		}
	}
	return nil
}
