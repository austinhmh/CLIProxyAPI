package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type claudePromptCacheProgressReadCloser struct {
	io.ReadCloser
	attempt *helps.ClaudePromptCacheAttempt
}

func (reader *claudePromptCacheProgressReadCloser) Read(buffer []byte) (int, error) {
	readBytes, errRead := reader.ReadCloser.Read(buffer)
	if readBytes > 0 && reader.attempt != nil {
		reader.attempt.MarkResponseProgress()
	}
	return readBytes, errRead
}

type ClaudePromptCacheRuntime = helps.ClaudePromptCacheRuntime

func NewClaudePromptCacheRuntime() *ClaudePromptCacheRuntime {
	return helps.NewClaudePromptCacheRuntime()
}

// NewClaudeExecutorWithPromptCacheRuntime retains cache knowledge across executor replacements.
func NewClaudeExecutorWithPromptCacheRuntime(cfg *config.Config, runtime *ClaudePromptCacheRuntime) *ClaudeExecutor {
	executor := NewClaudeExecutor(cfg)
	if runtime != nil {
		executor.promptCacheRuntime = runtime
	}
	return executor
}

func (executor *ClaudeExecutor) claudePromptCacheMode() string {
	if executor == nil || executor.cfg == nil {
		return config.ClaudePromptCacheModeLegacy
	}
	return executor.cfg.ClaudePromptCache.EffectiveMode()
}

func (executor *ClaudeExecutor) planAdaptiveClaudePromptCache(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	apiKey string,
	baseURL string,
	baseModel string,
	body []byte,
) ([]byte, *helps.ClaudePromptCachePlan) {
	if executor == nil || executor.promptCacheRuntime == nil || executor.claudePromptCacheMode() != config.ClaudePromptCacheModeAdaptive {
		return body, nil
	}

	body = helps.StripAllClaudeCacheControls(body)
	officialAnthropic := isOfficialAnthropicBaseURL(baseURL)
	scopeKey := claudePromptCacheScopeKey(auth, apiKey, baseURL, baseModel)
	body = normalizeCacheControlTTL(body)
	plannedBody, plan := executor.promptCacheRuntime.PlanClaudePromptCache(
		scopeKey,
		body,
		helps.ClaudePromptCacheCapabilities{
			AutomaticHistory: officialAnthropic,
			ExplicitHistory:  true,
		},
	)
	if plan != nil {
		helps.LogWithRequestID(ctx).WithFields(log.Fields{
			"component":            "claude_prompt_cache",
			"mode":                 config.ClaudePromptCacheModeAdaptive,
			"official_anthropic":   officialAnthropic,
			"existing_breakpoints": plan.Summary.ExistingBreakpoints,
			"added_breakpoints":    plan.Summary.AddedBreakpoints,
			"removed_breakpoints":  plan.Summary.RemovedBreakpoints,
			"final_breakpoints":    plan.Summary.FinalBreakpoints,
			"tool_count":           plan.Summary.CurrentToolCount,
			"automatic_history":    plan.Summary.AutomaticHistory,
		}).Debug("claude executor: planned prompt cache")
	}
	return plannedBody, plan
}

func (executor *ClaudeExecutor) acquireClaudePromptCacheAttempt(
	ctx context.Context,
	plan *helps.ClaudePromptCachePlan,
) (*helps.ClaudePromptCacheAttempt, error) {
	if executor == nil || executor.promptCacheRuntime == nil || plan == nil {
		return nil, nil
	}
	waitSeconds := 0
	if executor.cfg != nil {
		waitSeconds = executor.cfg.ClaudePromptCache.EffectiveColdStartMaxWaitSeconds()
	}
	return executor.promptCacheRuntime.Acquire(ctx, plan, time.Duration(waitSeconds)*time.Second)
}

func isOfficialAnthropicBaseURL(baseURL string) bool {
	parsedURL, errParse := url.Parse(strings.TrimSpace(baseURL))
	if errParse != nil || parsedURL == nil {
		return false
	}
	if !strings.EqualFold(parsedURL.Scheme, "https") || !strings.EqualFold(parsedURL.Hostname(), "api.anthropic.com") {
		return false
	}
	if port := parsedURL.Port(); port != "" && port != "443" {
		return false
	}
	return strings.Trim(parsedURL.EscapedPath(), "/") == "" && parsedURL.RawQuery == ""
}

func claudePromptCacheScopeKey(auth *cliproxyauth.Auth, apiKey, baseURL, baseModel string) string {
	credentialIdentity := ""
	if auth != nil {
		credentialIdentity = strings.TrimSpace(auth.ID)
	}
	credentialSecretHash := sha256.Sum256([]byte(apiKey))
	endpoint := strings.TrimSpace(baseURL)
	if parsedURL, errParse := url.Parse(strings.TrimSpace(baseURL)); errParse == nil && parsedURL != nil {
		parsedURL.Scheme = strings.ToLower(parsedURL.Scheme)
		parsedURL.Host = strings.ToLower(parsedURL.Host)
		parsedURL.Fragment = ""
		endpoint = parsedURL.String()
	}
	scopeMaterial := strings.Join([]string{
		"claude-prompt-cache-scope-v1",
		credentialIdentity,
		hex.EncodeToString(credentialSecretHash[:]),
		endpoint,
		strings.TrimSpace(baseModel),
	}, "\x00")
	scopeHash := sha256.Sum256([]byte(scopeMaterial))
	return hex.EncodeToString(scopeHash[:])
}
