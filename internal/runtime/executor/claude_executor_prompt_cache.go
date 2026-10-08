package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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

const claudeCacheDiagnosticsBeta = "cache-diagnosis-2026-04-07"

type claudeCacheDiagnosticsState struct {
	key               string
	scopeKey          string
	previousMessageID string
	generation        uint64
	proxyAdded        bool
}

// ClaudePromptCacheRuntime is shared by Claude executors created for one service.
type ClaudePromptCacheRuntime = helps.ClaudePromptCacheRuntime

// NewClaudePromptCacheRuntime creates service-scoped prompt-cache state.
func NewClaudePromptCacheRuntime() *ClaudePromptCacheRuntime {
	return helps.NewClaudePromptCacheRuntime()
}

// NewClaudeExecutorWithPromptCacheRuntime preserves cache knowledge when an
// executor is rebound during configuration or credential reloads.
func NewClaudeExecutorWithPromptCacheRuntime(
	cfg *config.Config,
	promptCacheRuntime *ClaudePromptCacheRuntime,
) *ClaudeExecutor {
	if promptCacheRuntime == nil {
		promptCacheRuntime = NewClaudePromptCacheRuntime()
	}
	return &ClaudeExecutor{
		cfg:                cfg,
		promptCacheRuntime: promptCacheRuntime,
	}
}

func (e *ClaudeExecutor) claudePromptCacheMode() string {
	if e == nil || e.cfg == nil {
		return config.ClaudePromptCacheModeLegacy
	}
	return e.cfg.ClaudePromptCache.EffectiveMode()
}

func (e *ClaudeExecutor) applyLegacyClaudePromptCache(body []byte) []byte {
	if countCacheControls(body) == 0 {
		body = ensureCacheControl(body)
	}
	body = enforceCacheControlLimit(body, helps.ClaudePromptCacheMaxBreakpoints)
	return normalizeCacheControlTTL(body)
}

func (e *ClaudeExecutor) planAdaptiveClaudePromptCache(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	apiKey string,
	baseURL string,
	baseModel string,
	body []byte,
) ([]byte, *helps.ClaudePromptCachePlan) {
	if e == nil || e.promptCacheRuntime == nil || e.claudePromptCacheMode() != config.ClaudePromptCacheModeAdaptive {
		return body, nil
	}
	// Adaptive mode always owns breakpoint placement: drop any client-provided
	// cache_control (top-level or in-body) and re-apply CPA's strategy.
	// Official Anthropic gets tools-tail + system-tail + second-to-last user +
	// top-level automatic history so conversation forks can still read the
	// stable history prefix.
	body = helps.StripAllClaudeCacheControls(body)
	officialAnthropic := isOfficialAnthropicBaseURL(baseURL)
	scopeKey := claudePromptCacheScopeKey(auth, apiKey, baseURL, baseModel)
	body = normalizeCacheControlTTL(body)
	plannedBody, plan := e.promptCacheRuntime.PlanClaudePromptCache(
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

func (e *ClaudeExecutor) acquireClaudePromptCacheAttempt(
	ctx context.Context,
	plan *helps.ClaudePromptCachePlan,
) (*helps.ClaudePromptCacheAttempt, error) {
	if e == nil || e.promptCacheRuntime == nil || plan == nil {
		return nil, nil
	}
	waitSeconds := 0
	if e.cfg != nil {
		waitSeconds = e.cfg.ClaudePromptCache.EffectiveColdStartMaxWaitSeconds()
	}
	return e.promptCacheRuntime.Acquire(ctx, plan, time.Duration(waitSeconds)*time.Second)
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

func claudePromptCacheScopeKey(
	auth *cliproxyauth.Auth,
	apiKey string,
	baseURL string,
	baseModel string,
) string {
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

func (e *ClaudeExecutor) applyClaudeCacheDiagnostics(
	auth *cliproxyauth.Auth,
	apiKey string,
	baseURL string,
	baseModel string,
	body []byte,
	extraBetas []string,
) ([]byte, []string, *claudeCacheDiagnosticsState) {
	if e == nil || e.cfg == nil || !e.cfg.ClaudePromptCache.Diagnostics ||
		e.promptCacheRuntime == nil || !isOfficialAnthropicBaseURL(baseURL) {
		return body, extraBetas, nil
	}
	sessionID := claudeSessionIDFromPayload(body)
	if sessionID == "" {
		return body, extraBetas, nil
	}
	scopeKey := claudePromptCacheScopeKey(auth, apiKey, baseURL, baseModel)
	diagnosticHash := sha256.Sum256([]byte(scopeKey + "\x00" + sessionID))
	diagnosticKey := hex.EncodeToString(diagnosticHash[:])
	state := &claudeCacheDiagnosticsState{key: diagnosticKey, scopeKey: scopeKey}

	if gjson.GetBytes(body, "diagnostics").Exists() {
		extraBetas = appendUniqueClaudeBeta(extraBetas, claudeCacheDiagnosticsBeta)
		return body, extraBetas, state
	}
	if !e.promptCacheRuntime.DiagnosticAllowed(scopeKey) {
		return body, extraBetas, nil
	}
	extraBetas = appendUniqueClaudeBeta(extraBetas, claudeCacheDiagnosticsBeta)
	diagnosticsObject := []byte(`{"previous_message_id":null}`)
	previousMessageID, generation := e.promptCacheRuntime.BeginDiagnostic(diagnosticKey)
	state.generation = generation
	state.previousMessageID = previousMessageID
	if previousMessageID != "" {
		diagnosticsObject, _ = sjson.SetBytes(diagnosticsObject, "previous_message_id", previousMessageID)
	}
	updatedBody, errSet := sjson.SetRawBytes(body, "diagnostics", diagnosticsObject)
	if errSet != nil {
		return body, extraBetas, nil
	}
	state.proxyAdded = true
	return updatedBody, extraBetas, state
}

func appendUniqueClaudeBeta(betas []string, requiredBeta string) []string {
	requiredBeta = strings.TrimSpace(requiredBeta)
	if requiredBeta == "" {
		return betas
	}
	for _, beta := range betas {
		if strings.TrimSpace(beta) == requiredBeta {
			return betas
		}
	}
	return append(betas, requiredBeta)
}

func (e *ClaudeExecutor) recordClaudeCacheDiagnostics(
	ctx context.Context,
	state *claudeCacheDiagnosticsState,
	messageID string,
	cacheMissReason string,
	cacheMissedInputTokens int64,
) {
	if e == nil || e.promptCacheRuntime == nil || state == nil {
		return
	}
	if !state.proxyAdded {
		return
	}
	e.promptCacheRuntime.RecordDiagnosticMessageID(state.key, state.generation, messageID)
	if cacheMissReason == "" {
		return
	}
	helps.LogWithRequestID(ctx).WithFields(log.Fields{
		"component":                 "claude_prompt_cache",
		"diagnostic_reason":         cacheMissReason,
		"cache_missed_input_tokens": cacheMissedInputTokens,
	}).Debug("claude executor: received prompt-cache diagnostics")
}

func (e *ClaudeExecutor) recordClaudeDiagnosticsFallback(
	state *claudeCacheDiagnosticsState,
	errorBody []byte,
) {
	if e == nil || e.promptCacheRuntime == nil || state == nil || !state.proxyAdded {
		return
	}
	errorMessage := strings.ToLower(strings.TrimSpace(gjson.GetBytes(errorBody, "error.message").String()))
	if errorMessage == "" {
		errorMessage = strings.ToLower(strings.TrimSpace(gjson.GetBytes(errorBody, "message").String()))
	}
	if strings.Contains(errorMessage, "previous_message_id") {
		unsupportedField := containsAnyClaudeDiagnosticsRejectionMarker(
			errorMessage,
			"unknown field",
			"unrecognized",
			"unsupported",
			"not supported",
			"not permitted",
			"not allowed",
			"extra inputs",
		)
		if !unsupportedField && state.previousMessageID != "" {
			e.promptCacheRuntime.InvalidateDiagnosticMessageID(
				state.key,
				state.generation,
				state.previousMessageID,
			)
			return
		}
	}
	e.promptCacheRuntime.DisableDiagnostic(state.scopeKey)
}

func applyResolvedClaudeRequestIdentity(body []byte, opts cliproxyexecutor.Options) ([]byte, error) {
	if opts.Metadata == nil {
		return body, nil
	}
	userID, _ := opts.Metadata[cliproxyexecutor.ClaudeUserIDMetadataKey].(string)
	identity, valid := cliproxyauth.ParseClaudeUserID(strings.TrimSpace(userID))
	if !valid {
		return body, nil
	}
	updatedBody, errApply := cliproxyauth.ApplyClaudeRequestIdentity(body, identity)
	if errApply != nil {
		return nil, fmt.Errorf("apply resolved Claude request identity: %w", errApply)
	}
	return updatedBody, nil
}

func claudeSessionIDFromPayload(body []byte) string {
	userID := gjson.GetBytes(body, "metadata.user_id").String()
	identity, valid := cliproxyauth.ParseClaudeUserID(userID)
	if !valid {
		return ""
	}
	return identity.SessionID
}

func (e *ClaudeExecutor) executeClaudeMessagesHTTPRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	reporter *helps.UsageReporter,
	apiKey string,
	requestURL string,
	stream bool,
	extraBetas []string,
	body []byte,
	cacheDiagnosticsState *claudeCacheDiagnosticsState,
	resignCCH bool,
	incomingHeaders http.Header,
) (*http.Response, []byte, *claudeCacheDiagnosticsState, error) {
	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	if reporter != nil {
		httpClient = reporter.TrackHTTPClient(httpClient)
		reporter.StartResponseTTFT()
	}
	requestBody := body
	requestBetas := append([]string(nil), extraBetas...)
	diagnosticsState := cacheDiagnosticsState

	for requestAttempt := 0; requestAttempt < 2; requestAttempt++ {
		httpRequest, errRequest := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			requestURL,
			bytes.NewReader(requestBody),
		)
		if errRequest != nil {
			return nil, requestBody, diagnosticsState, errRequest
		}
		if errHeaders := applyClaudeHeaders(
			httpRequest,
			auth,
			apiKey,
			stream,
			requestBetas,
			requestBody,
			e.cfg,
			incomingHeaders,
			false,
			claudeSessionIDFromPayload(requestBody),
		); errHeaders != nil {
			return nil, requestBody, diagnosticsState, errHeaders
		}
		recordClaudeUpstreamRequest(ctx, e.cfg, auth, e.upstreamRequestLogProvider(), requestURL, httpRequest, requestBody)

		httpResponse, errHTTP := httpClient.Do(httpRequest)
		if errHTTP != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errHTTP)
			return nil, requestBody, diagnosticsState, errHTTP
		}
		helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResponse.StatusCode, httpResponse.Header.Clone())
		if httpResponse.StatusCode >= 200 && httpResponse.StatusCode < 300 {
			if reporter != nil {
				reporter.ObserveResponse(httpResponse)
			}
			return httpResponse, requestBody, diagnosticsState, nil
		}

		errorBody, errRead := readClaudeErrorResponse(ctx, e.cfg, httpResponse)
		if errRead != nil {
			return nil, requestBody, diagnosticsState, statusErr{
				code:    httpResponse.StatusCode,
				msg:     errRead.Error(),
				headers: httpResponse.Header.Clone(),
			}
		}
		if requestAttempt == 0 && shouldRetryClaudeRequestWithoutDiagnostics(
			httpResponse.StatusCode,
			errorBody,
			diagnosticsState,
		) {
			e.recordClaudeDiagnosticsFallback(diagnosticsState, errorBody)
			requestBody, _ = sjson.DeleteBytes(requestBody, "diagnostics")
			requestBetas = removeClaudeBeta(requestBetas, claudeCacheDiagnosticsBeta)
			if resignCCH {
				var errSign error
				requestBody, errSign = signAnthropicMessagesBody(requestBody)
				if errSign != nil {
					return nil, requestBody, diagnosticsState, fmt.Errorf("resign Claude CCH after diagnostics fallback: %w", errSign)
				}
			}
			diagnosticsState = nil
			helps.LogWithRequestID(ctx).Debug(
				"claude executor: retrying once without proxy-added cache diagnostics",
			)
			continue
		}

		helps.LogWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s",
			httpResponse.StatusCode,
			helps.SummarizeErrorBody(httpResponse.Header.Get("Content-Type"), errorBody),
		)
		return nil, requestBody, diagnosticsState, statusErr{
			code:    httpResponse.StatusCode,
			msg:     string(errorBody),
			headers: httpResponse.Header.Clone(),
		}
	}

	return nil, requestBody, diagnosticsState, statusErr{
		code: http.StatusBadGateway,
		msg:  "claude executor: diagnostics fallback exhausted",
	}
}

func recordClaudeUpstreamRequest(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	provider string,
	requestURL string,
	httpRequest *http.Request,
	body []byte,
) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, cfg, helps.UpstreamRequestLog{
		URL:       requestURL,
		Method:    http.MethodPost,
		Headers:   httpRequest.Header.Clone(),
		Body:      body,
		Provider:  provider,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

func readClaudeErrorResponse(
	ctx context.Context,
	cfg *config.Config,
	httpResponse *http.Response,
) ([]byte, error) {
	decodedBody, errDecode := decodeResponseBody(
		httpResponse.Body,
		httpResponse.Header.Get("Content-Encoding"),
	)
	if errDecode != nil {
		helps.RecordAPIResponseError(ctx, cfg, errDecode)
		if httpResponse.Body != nil {
			_ = httpResponse.Body.Close()
		}
		return nil, fmt.Errorf("failed to decode error response body: %w", errDecode)
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	errorBody, errRead := io.ReadAll(decodedBody)
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, cfg, errRead)
		return nil, fmt.Errorf("failed to read error response body: %w", errRead)
	}
	helps.AppendAPIResponseChunk(ctx, cfg, errorBody)
	return errorBody, nil
}

func shouldRetryClaudeRequestWithoutDiagnostics(
	statusCode int,
	errorBody []byte,
	state *claudeCacheDiagnosticsState,
) bool {
	if statusCode != http.StatusBadRequest || state == nil || !state.proxyAdded {
		return false
	}
	errorMessage := gjson.GetBytes(errorBody, "error.message").String()
	if strings.TrimSpace(errorMessage) == "" {
		errorMessage = gjson.GetBytes(errorBody, "message").String()
	}
	lowerErrorMessage := strings.ToLower(strings.TrimSpace(errorMessage))
	if lowerErrorMessage == "" {
		return false
	}

	if strings.Contains(lowerErrorMessage, "previous_message_id") {
		return containsAnyClaudeDiagnosticsRejectionMarker(
			lowerErrorMessage,
			"invalid",
			"not found",
			"not_found",
			"unknown",
			"does not exist",
		)
	}
	if !strings.Contains(lowerErrorMessage, "diagnostics") &&
		!strings.Contains(lowerErrorMessage, "cache-diagnosis") {
		return false
	}
	return containsAnyClaudeDiagnosticsRejectionMarker(
		lowerErrorMessage,
		"unknown field",
		"unrecognized",
		"unsupported",
		"not supported",
		"not permitted",
		"not allowed",
		"extra inputs",
		"invalid beta",
	)
}

func containsAnyClaudeDiagnosticsRejectionMarker(message string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func removeClaudeBeta(betas []string, betaToRemove string) []string {
	filteredBetas := make([]string, 0, len(betas))
	for _, beta := range betas {
		if strings.TrimSpace(beta) == betaToRemove {
			continue
		}
		filteredBetas = append(filteredBetas, beta)
	}
	return filteredBetas
}
