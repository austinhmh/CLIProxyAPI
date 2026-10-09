package auth

import (
	"sort"
	"strings"
	"time"
)

// SessionAffinitySnapshotItem describes one sticky session binding.
type SessionAffinitySnapshotItem struct {
	SessionID  string    `json:"session_id"`
	AuthID     string    `json:"auth_id"`
	Provider   string    `json:"provider"`
	ModelKey   string    `json:"model_key"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// HashPreviewItem provides per-auth balanced-hash score diagnostics.
type HashPreviewItem struct {
	AuthID      string  `json:"auth_id"`
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	HashScore   float64 `json:"hash_score"`
	Freshness   float64 `json:"freshness_score"`
	Quota       float64 `json:"quota_score"`
	Penalty     float64 `json:"penalty_score"`
	Total       float64 `json:"total_score"`
	Blocked     bool    `json:"blocked"`
	BlockReason string  `json:"block_reason,omitempty"`
}

// SessionAffinitySnapshot returns active sticky-session bindings touched within window.
// On current main, sticky state lives in SessionAffinitySelector's SessionCache.
func (m *Manager) SessionAffinitySnapshot(window time.Duration) []SessionAffinitySnapshotItem {
	if m == nil {
		return nil
	}
	if window <= 0 {
		window = 5 * time.Minute
	}

	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()

	affinity, ok := selector.(*SessionAffinitySelector)
	if !ok || affinity == nil || affinity.cache == nil {
		return nil
	}
	return affinity.Snapshot(window)
}

// Snapshot returns active session bindings last refreshed within window.
func (s *SessionAffinitySelector) Snapshot(window time.Duration) []SessionAffinitySnapshotItem {
	if s == nil || s.cache == nil {
		return nil
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	cutoff := time.Now().UTC().Add(-window)
	ttl := s.cache.ttl
	if ttl <= 0 {
		ttl = time.Hour
	}

	type groupedBinding struct {
		sessionID  string
		provider   string
		modelKey   string
		authID     string
		lastSeenAt time.Time
	}

	s.cache.mu.Lock()
	now := time.Now()
	grouped := make(map[string]groupedBinding)
	for key, entry := range s.cache.entries {
		if !now.Before(entry.expiresAt) {
			s.cache.removeAliasGroupLocked(entry)
			continue
		}
		lastSeen := entry.expiresAt.Add(-ttl)
		if lastSeen.Before(cutoff) {
			continue
		}
		provider, sessionID, modelKey := splitSessionCacheKey(key)
		groupKey := entry.authID + "|" + entry.expiresAt.UTC().Format(time.RFC3339Nano)
		if existing, exists := grouped[groupKey]; exists {
			// Prefer a shorter / clearer session id when aliases share a binding.
			if len(sessionID) > 0 && (existing.sessionID == "" || len(sessionID) < len(existing.sessionID)) {
				existing.sessionID = sessionID
				existing.provider = provider
				existing.modelKey = modelKey
				grouped[groupKey] = existing
			}
			continue
		}
		grouped[groupKey] = groupedBinding{
			sessionID:  sessionID,
			provider:   provider,
			modelKey:   modelKey,
			authID:     strings.TrimSpace(entry.authID),
			lastSeenAt: lastSeen.UTC(),
		}
	}
	s.cache.mu.Unlock()

	items := make([]SessionAffinitySnapshotItem, 0, len(grouped))
	for _, binding := range grouped {
		items = append(items, SessionAffinitySnapshotItem{
			SessionID:  binding.sessionID,
			AuthID:     binding.authID,
			Provider:   binding.provider,
			ModelKey:   binding.modelKey,
			LastSeenAt: binding.lastSeenAt,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].LastSeenAt.Equal(items[j].LastSeenAt) {
			return items[i].SessionID < items[j].SessionID
		}
		return items[i].LastSeenAt.After(items[j].LastSeenAt)
	})
	return items
}

func splitSessionCacheKey(key string) (provider, sessionID, modelKey string) {
	parts := strings.Split(key, "::")
	if len(parts) < 3 {
		return "", strings.TrimSpace(key), ""
	}
	provider = strings.TrimSpace(parts[0])
	modelKey = strings.TrimSpace(parts[len(parts)-1])
	sessionID = strings.TrimSpace(strings.Join(parts[1:len(parts)-1], "::"))
	return provider, sessionID, modelKey
}

// BalancedHashPreview returns score breakdown for eligible auths.
func (m *Manager) BalancedHashPreview(provider, model, requestKey string) []HashPreviewItem {
	if m == nil {
		return nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	modelKey := canonicalModelKey(model)
	if modelKey == "" {
		modelKey = model
	}
	requestKey = strings.TrimSpace(requestKey)
	if requestKey == "" {
		requestKey = time.Now().UTC().Format("2006-01-02T15:04")
	}
	now := time.Now()

	m.mu.RLock()
	candidates := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if provider != "" && strings.ToLower(strings.TrimSpace(auth.Provider)) != provider {
			continue
		}
		candidates = append(candidates, auth)
	}
	m.mu.RUnlock()

	items := make([]HashPreviewItem, 0, len(candidates))
	for _, auth := range candidates {
		hashScore := normalizedHashScore(requestKey + "|" + modelKey + "|" + strings.TrimSpace(auth.ID))
		freshness := authFreshnessScore(auth, model, now)
		quota := authQuotaScore(auth, model)
		penalty := authRecentPenalty(auth, model)
		total := (0.40 * hashScore) + (0.25 * freshness) + (0.25 * quota) + (0.10 * (1.0 - penalty))
		blocked, reason, _ := isAuthBlockedForModel(auth, model, now)

		item := HashPreviewItem{
			AuthID:    strings.TrimSpace(auth.ID),
			Provider:  strings.ToLower(strings.TrimSpace(auth.Provider)),
			Model:     model,
			HashScore: hashScore,
			Freshness: freshness,
			Quota:     quota,
			Penalty:   penalty,
			Total:     total,
			Blocked:   blocked,
		}
		if blocked {
			switch reason {
			case blockReasonCooldown:
				item.BlockReason = "cooldown"
			case blockReasonDisabled:
				item.BlockReason = "disabled"
			case blockReasonOther:
				item.BlockReason = "other"
			default:
				item.BlockReason = "unknown"
			}
		}
		items = append(items, item)
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Total == items[j].Total {
			return items[i].AuthID < items[j].AuthID
		}
		return items[i].Total > items[j].Total
	})
	return items
}
