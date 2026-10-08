package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"golang.org/x/net/context"
)

const clientModelAccessDeniedCode = "model_not_allowed"

// validateClientModelAccess checks the requested models against the caller's configured
// api-key-model-rules. It returns nil when access is allowed (including when no rules apply)
// and a 403 ErrorMessage when any requested model is excluded for the calling client key.
func (h *BaseAPIHandler) validateClientModelAccess(ctx context.Context, models ...string) *interfaces.ErrorMessage {
	if h == nil || h.Cfg == nil || len(h.Cfg.APIKeyModelRules) == 0 {
		return nil
	}

	apiKey := clientAPIKeyFromContext(ctx)
	if apiKey == "" {
		return nil
	}

	for _, rule := range h.Cfg.APIKeyModelRules {
		if rule.APIKey != apiKey {
			continue
		}
		for _, model := range models {
			if isClientModelExcluded(model, rule.ExcludedModels) {
				return newClientModelAccessDeniedError(model)
			}
		}
	}

	return nil
}

// clientAPIKeyFromContext returns the raw client API key set by AuthMiddleware, if any.
func clientAPIKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginContext, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginContext == nil {
		return ""
	}
	value, exists := ginContext.Get("userApiKey")
	if !exists || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// isClientModelExcluded reports whether model matches any of the configured wildcard patterns,
// checking the raw model, its reasoning-suffix-stripped form, and its prefix-stripped form.
func isClientModelExcluded(model string, patterns []string) bool {
	modelCandidates := clientModelCandidates(model)
	for _, pattern := range patterns {
		for _, candidate := range modelCandidates {
			if util.MatchWildcard(pattern, candidate) {
				return true
			}
		}
	}
	return false
}

func clientModelCandidates(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}

	candidates := make([]string, 0, 3)
	appendCandidate := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return
		}
		for _, existing := range candidates {
			if existing == value {
				return
			}
		}
		candidates = append(candidates, value)
	}

	appendCandidate(model)
	parsedModel := thinking.ParseSuffix(model)
	appendCandidate(parsedModel.ModelName)

	baseModel := strings.TrimSpace(parsedModel.ModelName)
	if slashIndex := strings.LastIndex(baseModel, "/"); slashIndex >= 0 {
		appendCandidate(baseModel[slashIndex+1:])
	}

	return candidates
}

func newClientModelAccessDeniedError(model string) *interfaces.ErrorMessage {
	message := fmt.Sprintf("model %q is not allowed for this API key", strings.TrimSpace(model))
	responseBody, errMarshal := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Message: message,
			Type:    "permission_error",
			Code:    clientModelAccessDeniedCode,
		},
	})
	if errMarshal != nil {
		responseBody = []byte(`{"error":{"message":"model access denied","type":"permission_error","code":"model_not_allowed"}}`)
	}

	return &interfaces.ErrorMessage{
		StatusCode:     http.StatusForbidden,
		Error:          fmt.Errorf("%s", message),
		DirectResponse: true,
		Body:           responseBody,
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
	}
}

// clientModelAccessDeniedStream wraps a denial ErrorMessage into the streaming
// (channel-based) response shape used by the streaming execution entry points.
func clientModelAccessDeniedStream(errMsg *interfaces.ErrorMessage) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	errorChannel := make(chan *interfaces.ErrorMessage, 1)
	errorChannel <- errMsg
	close(errorChannel)
	return nil, nil, errorChannel
}
