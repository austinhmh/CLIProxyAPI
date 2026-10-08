package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const (
	// usageRefreshCheckInterval is how often the prober scans all auths for a
	// stale rate-limit snapshot. The staleness threshold is much coarser
	// (usageRefreshStaleAfter), so this only needs to be frequent enough to
	// notice a newly-stale auth within a reasonable margin of that threshold.
	usageRefreshCheckInterval = 5 * time.Minute
	// usageRefreshStaleAfter is how old a RateLimits snapshot must be (or
	// missing entirely) before the prober refreshes it with a probe call.
	usageRefreshStaleAfter = 30 * time.Minute
	// usageRefreshBetweenProbes spaces out consecutive probe calls so a cold
	// start (every auth stale at once) doesn't burst-call every credential.
	usageRefreshBetweenProbes = 5 * time.Second
)

// usageRefreshProviders lists the providers the prober will probe: claude and
// codex have rate-limit header tracking wired up in rate_limit_headers.go,
// and antigravity has a dedicated quota endpoint wired up in
// antigravity_quota.go (its usage is not reported via response headers).
// Probing any other provider would send a real API call for data the prober
// has no parser for.
var usageRefreshProviders = map[string]bool{
	"claude":      true,
	"codex":       true,
	"antigravity": true,
}

// StartUsageRefresh launches a background loop that periodically sends a
// minimal probe request through Claude/Codex auths whose cached RateLimits
// snapshot (see rate_limit_headers.go) is missing or older than
// usageRefreshStaleAfter, purely to harvest fresh usage headers for auths
// that have not served real traffic recently. Only one loop is kept alive;
// starting a new one cancels the previous run.
func (m *Manager) StartUsageRefresh(parent context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = usageRefreshCheckInterval
	}

	m.mu.Lock()
	cancelPrev := m.usageRefreshCancel
	m.usageRefreshCancel = nil
	m.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}

	ctx, cancelCtx := context.WithCancel(parent)
	m.mu.Lock()
	m.usageRefreshCancel = cancelCtx
	m.mu.Unlock()

	go m.runUsageRefreshLoop(ctx, interval)
}

// StopUsageRefresh cancels the background usage-refresh loop, if running.
func (m *Manager) StopUsageRefresh() {
	m.mu.Lock()
	cancelPrev := m.usageRefreshCancel
	m.usageRefreshCancel = nil
	m.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}
}

func (m *Manager) runUsageRefreshLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.runUsageRefreshPass(ctx)
		}
	}
}

// runUsageRefreshPass scans all auths once and probes every stale Claude/Codex
// credential, sequentially and with a short delay between calls.
func (m *Manager) runUsageRefreshPass(ctx context.Context) {
	now := time.Now()
	m.mu.RLock()
	due := make([]string, 0)
	for id, a := range m.auths {
		if needsUsageProbe(a, now) {
			due = append(due, id)
		}
	}
	m.mu.RUnlock()

	if len(due) == 0 {
		return
	}
	if log.IsLevelEnabled(log.DebugLevel) {
		log.Debugf("usage-refresh prober: %d auth(s) due for a probe", len(due))
	}

	for i, authID := range due {
		if ctx.Err() != nil {
			return
		}
		m.probeUsage(ctx, authID)
		if i < len(due)-1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(usageRefreshBetweenProbes):
			}
		}
	}
}

// needsUsageProbe reports whether auth is a credential from usageRefreshProviders
// whose cached rate-limit snapshot is missing or stale, and is not currently
// unauthorized (probing that would waste a call and could surface an error
// with nothing useful to do about it).
func needsUsageProbe(a *Auth, now time.Time) bool {
	if a == nil {
		return false
	}
	if !usageRefreshProviders[a.Provider] {
		return false
	}
	if hasUnauthorizedAuthFailure(a) {
		return false
	}
	observedAt, ok := parseRateLimitObservedAt(a.RateLimits)
	if !ok {
		return true
	}
	return now.Sub(observedAt) > usageRefreshStaleAfter
}

// probeUsage sends one minimal real request through the auth's executor to
// harvest fresh rate-limit response headers, then applies them the same way
// MarkResult would. It intentionally bypasses MarkResult: a probe failure
// must not trip cooldown/disabled state on a credential that real traffic
// may still be relying on, and the probe itself is not "real" usage worth
// counting toward Success/Failed telemetry.
func (m *Manager) probeUsage(ctx context.Context, authID string) {
	m.mu.RLock()
	a := m.auths[authID]
	var exec ProviderExecutor
	if a != nil {
		exec = m.executors[a.Provider]
	}
	m.mu.RUnlock()

	if a == nil || exec == nil {
		return
	}
	if !needsUsageProbe(a, time.Now()) {
		// Superseded by real traffic (or a concurrent probe) since it was queued.
		return
	}

	if a.Provider == "antigravity" {
		m.probeAntigravityUsage(ctx, authID, a, exec)
		return
	}

	req, opts, ok := buildUsageProbeRequest(a)
	if !ok {
		log.Debugf("usage-refresh prober: no probe model available for auth %s (%s)", authID, a.Provider)
		return
	}

	resp, execErr := exec.Execute(ctx, a, req, opts)
	headers := headersFromExecResult(resp, execErr)
	if len(headers) == 0 {
		log.Debugf("usage-refresh prober: probe for auth %s (%s) returned no usage headers (err=%v)", authID, a.Provider, execErr)
		return
	}

	m.mu.Lock()
	if current := m.auths[authID]; current != nil {
		applyRateLimitHeaders(current, headers, time.Now())
	}
	m.mu.Unlock()
}

// probeAntigravityUsage fetches a fresh quota snapshot for an antigravity auth
// via the dedicated retrieveUserQuotaSummary endpoint: antigravity does not
// report usage on response headers like Claude/Codex (see
// usageRefreshProviders), so it needs its own request/response handling
// instead of buildUsageProbeRequest's Execute()+headers path. Like the
// Claude/Codex path, failures are silent aside from a debug log: a probe is
// not "real" traffic and must not affect the auth's availability or cooldown
// state.
func (m *Manager) probeAntigravityUsage(ctx context.Context, authID string, a *Auth, exec ProviderExecutor) {
	projectID := antigravityQuotaProjectID(a)
	if projectID == "" {
		log.Debugf("usage-refresh prober: antigravity auth %s has no project_id yet, skipping probe", authID)
		return
	}

	body, errMarshal := json.Marshal(map[string]string{"project": projectID})
	if errMarshal != nil {
		log.Debugf("usage-refresh prober: antigravity auth %s: marshal quota request error: %v", authID, errMarshal)
		return
	}

	var groups []AntigravityQuotaGroup
	for _, base := range antigravityQuotaBaseURLCandidates(a) {
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, base+antigravityQuotaPath, bytes.NewReader(body))
		if errReq != nil {
			log.Debugf("usage-refresh prober: antigravity auth %s: build quota request error: %v", authID, errReq)
			continue
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, errDo := exec.HttpRequest(ctx, a, httpReq)
		if errDo != nil {
			log.Debugf("usage-refresh prober: antigravity auth %s: quota request to %s failed: %v", authID, base, errDo)
			continue
		}
		respBody, ok := readAntigravityQuotaResponse(resp)
		if !ok {
			log.Debugf("usage-refresh prober: antigravity auth %s: quota response from %s not usable", authID, base)
			continue
		}
		if g, parsed := parseAntigravityQuotaSummary(respBody); parsed {
			groups = g
			break
		}
	}

	if len(groups) == 0 {
		log.Debugf("usage-refresh prober: probe for auth %s (antigravity) returned no usage groups", authID)
		return
	}

	m.mu.Lock()
	if current := m.auths[authID]; current != nil {
		SetAntigravityQuotaGroups(current, groups, time.Now())
	}
	m.mu.Unlock()
}

// readAntigravityQuotaResponse reads and closes resp's body, reporting
// ok=false for a nil response, a non-2xx status, or a body read error.
func readAntigravityQuotaResponse(resp *http.Response) ([]byte, bool) {
	if resp == nil {
		return nil, false
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("usage-refresh prober: close antigravity quota response body error: %v", errClose)
		}
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, false
	}
	data, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, false
	}
	return data, true
}

// buildUsageProbeRequest constructs a minimal native-format request for the
// auth's provider, using only a provider-specific low-cost probe model from
// the models currently registered for this auth (see internal/registry, the
// same source GET /v0/management/auth-files/models reads from). It reports
// ok=false when no safe probe model is registered, in which case the caller
// skips the probe rather than falling back to an arbitrary business model.
//
// Both branches use a real, un-cached provider call (not a token-count/local
// endpoint): Claude's count_tokens endpoint is not confirmed to return the
// same Anthropic-Ratelimit-Unified-* headers as a real Messages call, and
// Codex's CountTokens never reaches the upstream at all, so neither is a
// reliable header source.
func buildUsageProbeRequest(a *Auth) (cliproxyexecutor.Request, cliproxyexecutor.Options, bool) {
	models := registry.GetGlobalRegistry().GetModelsForClient(a.ID)
	if len(models) == 0 {
		return cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, false
	}

	switch a.Provider {
	case "claude":
		model := pickProbeModel(models, "haiku")
		if model == "" {
			return cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, false
		}
		req := cliproxyexecutor.Request{
			Model:   model,
			Payload: []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`),
		}
		opts := cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatClaude,
			ResponseFormat: sdktranslator.FormatClaude,
		}
		return req, opts, true
	case "codex":
		model := pickProbeModel(models, "luna")
		if model == "" {
			return cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, false
		}
		req := cliproxyexecutor.Request{
			Model:   model,
			Payload: []byte(`{"input":"hi","max_output_tokens":64}`),
		}
		opts := cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatCodex,
			ResponseFormat: sdktranslator.FormatCodex,
		}
		return req, opts, true
	default:
		return cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, false
	}
}

// pickProbeModel returns a model whose ID contains preferred using a
// case-insensitive substring match. It returns an empty string when no safe
// probe model is registered instead of falling back to an arbitrary model.
func pickProbeModel(models []*registry.ModelInfo, preferred string) string {
	for _, m := range models {
		if m == nil || m.ID == "" {
			continue
		}
		if strings.Contains(strings.ToLower(m.ID), preferred) {
			return m.ID
		}
	}
	return ""
}
