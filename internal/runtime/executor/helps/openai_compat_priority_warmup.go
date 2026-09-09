package helps

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	OpenAICompatPriorityWarmupTTL      = 5 * time.Minute
	openAICompatPriorityWarmupCapacity = 65536
)

type OpenAICompatPriorityWarmupSignals uint16

const (
	OpenAICompatPrioritySignalContentRoot OpenAICompatPriorityWarmupSignals = 1 << iota
	OpenAICompatPrioritySignalPromptCacheKey
	OpenAICompatPrioritySignalConversationID
	OpenAICompatPrioritySignalSessionID
	OpenAICompatPrioritySignalThreadID
	OpenAICompatPrioritySignalSessionHeader
	OpenAICompatPrioritySignalExecutionSession
	OpenAICompatPrioritySignalLCP
	OpenAICompatPrioritySignalFinalPromptCacheKey
)

type priorityWarmupEntry struct {
	key       [sha256.Size]byte
	expiresAt time.Time
	signals   OpenAICompatPriorityWarmupSignals
}

// OpenAICompatPriorityWarmupRuntime stores successes only, ordered by last success.
// Reads neither extend the lifetime nor change the eviction order.
type OpenAICompatPriorityWarmupRuntime struct {
	mutex    sync.Mutex
	entries  map[[sha256.Size]byte]*list.Element
	order    list.List
	now      func() time.Time
	capacity int
}

func NewOpenAICompatPriorityWarmupRuntime() *OpenAICompatPriorityWarmupRuntime {
	return &OpenAICompatPriorityWarmupRuntime{
		entries:  make(map[[sha256.Size]byte]*list.Element),
		now:      time.Now,
		capacity: openAICompatPriorityWarmupCapacity,
	}
}

func (runtime *OpenAICompatPriorityWarmupRuntime) isWarm(key [sha256.Size]byte) bool {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	element := runtime.entries[key]
	if element == nil {
		return false
	}
	if !runtime.now().Before(element.Value.(priorityWarmupEntry).expiresAt) {
		runtime.removeLocked(element)
		return false
	}
	return true
}

func (runtime *OpenAICompatPriorityWarmupRuntime) markSuccess(key [sha256.Size]byte, signals OpenAICompatPriorityWarmupSignals) {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	if runtime.entries == nil {
		runtime.entries = make(map[[sha256.Size]byte]*list.Element)
	}
	if runtime.now == nil {
		runtime.now = time.Now
	}
	if runtime.capacity <= 0 {
		runtime.capacity = openAICompatPriorityWarmupCapacity
	}
	now := runtime.now()
	for oldest := runtime.order.Front(); oldest != nil; oldest = runtime.order.Front() {
		if now.Before(oldest.Value.(priorityWarmupEntry).expiresAt) {
			break
		}
		runtime.removeLocked(oldest)
	}
	if element := runtime.entries[key]; element != nil {
		entry := element.Value.(priorityWarmupEntry)
		entry.expiresAt = now.Add(OpenAICompatPriorityWarmupTTL)
		entry.signals |= signals
		element.Value = entry
		runtime.order.MoveToBack(element)
		return
	}
	for len(runtime.entries) >= runtime.capacity {
		runtime.removeLocked(runtime.order.Front())
	}
	entry := priorityWarmupEntry{key: key, expiresAt: now.Add(OpenAICompatPriorityWarmupTTL), signals: signals}
	runtime.entries[key] = runtime.order.PushBack(entry)
}

func (runtime *OpenAICompatPriorityWarmupRuntime) removeLocked(element *list.Element) {
	delete(runtime.entries, element.Value.(priorityWarmupEntry).key)
	runtime.order.Remove(element)
}

type OpenAICompatPriorityWarmupAttempt struct {
	runtime *OpenAICompatPriorityWarmupRuntime
	key     [sha256.Size]byte
	signals OpenAICompatPriorityWarmupSignals
}

// MarkSuccess commits only if cancellation has not been observed at completion.
func (attempt *OpenAICompatPriorityWarmupAttempt) MarkSuccess(ctx context.Context) {
	if attempt != nil && attempt.runtime != nil && (ctx == nil || ctx.Err() == nil) {
		attempt.runtime.markSuccess(attempt.key, attempt.signals)
		LogWithRequestID(ctx).Debugf("openai compat priority warmup: state=success_marked scope=%x signals=%d ttl=%s", attempt.key, attempt.signals, OpenAICompatPriorityWarmupTTL)
	}
}

// Prepare gates the final HTTP payload after custom headers have been applied.
// It keeps Body, GetBody, ContentLength and the logged/translated bytes consistent.
func (runtime *OpenAICompatPriorityWarmupRuntime) Prepare(request *http.Request, provider, authFallback string, attrs map[string]string, payload []byte, input cliproxyexecutor.Request, options cliproxyexecutor.Options) ([]byte, *OpenAICompatPriorityWarmupAttempt, error) {
	root := cliproxysession.OpenAIResponsesPromptRootFingerprint(payload)
	model := gjson.GetBytes(payload, "model").String()
	var attempt *OpenAICompatPriorityWarmupAttempt
	warm := false
	if runtime != nil && root != "" && model != "" && provider != "" && request.URL != nil {
		headers := make(map[string][]string)
		for _, name := range []string{"Authorization", "X-API-Key", "API-Key", "OpenAI-Organization", "OpenAI-Project"} {
			if values := request.Header.Values(name); len(values) > 0 {
				headers[http.CanonicalHeaderKey(name)] = values
			}
		}
		for attribute := range attrs {
			if !strings.HasPrefix(attribute, "header:") {
				continue
			}
			name := http.CanonicalHeaderKey(strings.TrimSpace(strings.TrimPrefix(attribute, "header:")))
			if !priorityWarmupIgnoredHeader(name) {
				if values := request.Header.Values(name); len(values) > 0 {
					headers[name] = values
				}
			}
		}
		credential := ""
		if request.Header.Get("Authorization") == "" && request.Header.Get("X-API-Key") == "" && request.Header.Get("API-Key") == "" {
			credential = authFallback
		}
		if len(headers) > 0 || credential != "" {
			// JSON framing avoids delimiter collisions; only the digest survives this call.
			encoded, err := json.Marshal([]any{provider, request.URL.String(), request.Host, model, root, credential, headers})
			if err != nil {
				return payload, nil, err
			}
			attempt = &OpenAICompatPriorityWarmupAttempt{runtime: runtime, key: sha256.Sum256(encoded), signals: priorityWarmupSignals(input, options, payload)}
			warm = runtime.isWarm(attempt.key)
		}
	}
	tier := gjson.GetBytes(payload, "service_tier")
	isPriority := tier.Type == gjson.String && tier.String() == "priority"
	if warm || !isPriority {
		if warm && isPriority {
			LogWithRequestID(request.Context()).Debugf("openai compat priority warmup: state=warm_priority_kept scope=%x", attempt.key)
		} else if !warm && attempt != nil {
			LogWithRequestID(request.Context()).Debugf("openai compat priority warmup: state=cold_without_priority scope=%x", attempt.key)
		}
		return payload, attempt, nil
	}
	updated, err := sjson.DeleteBytes(payload, "service_tier")
	if err != nil {
		return payload, nil, err
	}
	request.Body = io.NopCloser(bytes.NewReader(updated))
	request.ContentLength = int64(len(updated))
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(updated)), nil }
	request.Header.Del("Content-Length")
	if attempt == nil {
		LogWithRequestID(request.Context()).Debug("openai compat priority warmup: state=cold_unhashable_priority_removed")
	} else {
		LogWithRequestID(request.Context()).Debugf("openai compat priority warmup: state=cold_priority_removed scope=%x", attempt.key)
	}
	return updated, attempt, nil
}

func priorityWarmupSessionHeader(name string) bool {
	switch strings.ToLower(name) {
	case "x-claude-code-session-id", "session-id", "session_id", "x-http-session-id",
		"x-session-id", "x-session-affinity", "x-slot-session-id", "x-conversation-id",
		"x-thread-id", "thread-id", "x-client-request-id", "prompt-cache-key":
		return true
	default:
		return false
	}
}

func priorityWarmupIgnoredHeader(name string) bool {
	if priorityWarmupSessionHeader(name) {
		return true
	}
	switch strings.ToLower(name) {
	case "x-request-id", "x-correlation-id", "traceparent", "tracestate", "baggage",
		"content-length", "transfer-encoding", "connection", "accept", "cache-control":
		return true
	default:
		return false
	}
}

func priorityWarmupSignals(input cliproxyexecutor.Request, options cliproxyexecutor.Options, translated []byte) OpenAICompatPriorityWarmupSignals {
	signals := OpenAICompatPrioritySignalContentRoot
	for _, payload := range [][]byte{input.Payload, options.OriginalRequest} {
		for _, identity := range []struct {
			paths  []string
			signal OpenAICompatPriorityWarmupSignals
		}{
			{[]string{"prompt_cache_key"}, OpenAICompatPrioritySignalPromptCacheKey},
			{[]string{"conversation", "conversation_id", "conversationId", "metadata.conversation_id"}, OpenAICompatPrioritySignalConversationID},
			{[]string{"session_id", "sessionId", "metadata.session_id"}, OpenAICompatPrioritySignalSessionID},
			{[]string{"thread_id", "threadId", "metadata.thread_id"}, OpenAICompatPrioritySignalThreadID},
		} {
			for _, path := range identity.paths {
				if gjson.GetBytes(payload, path).Exists() {
					signals |= identity.signal
				}
			}
		}
	}
	for name := range options.Headers {
		if priorityWarmupSessionHeader(name) {
			signals |= OpenAICompatPrioritySignalSessionHeader
		}
	}
	for _, metadata := range []map[string]any{options.Metadata, input.Metadata} {
		if value, _ := metadata[cliproxyexecutor.ExecutionSessionMetadataKey].(string); value != "" {
			signals |= OpenAICompatPrioritySignalExecutionSession
		}
		if value, _ := metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey].(string); value != "" {
			signals |= OpenAICompatPrioritySignalLCP
		}
	}
	if gjson.GetBytes(translated, "prompt_cache_key").Exists() {
		signals |= OpenAICompatPrioritySignalFinalPromptCacheKey
	}
	return signals
}

// OpenAICompatResponsesSuccessfulBody requires positive completion evidence.
func OpenAICompatResponsesSuccessfulBody(payload []byte) bool {
	if !json.Valid(payload) {
		return false
	}
	response := gjson.ParseBytes(payload)
	if !response.IsObject() || response.Get("object").String() != "response" || response.Get("status").String() != "completed" {
		return false
	}
	switch response.Get("type").String() {
	case "error", "response.error", "response.failed", "response.incomplete":
		return false
	}
	for _, path := range []string{"error", "response.error"} {
		if errorValue := response.Get(path); errorValue.Exists() && errorValue.Type != gjson.Null {
			return false
		}
	}
	return true
}

func OpenAICompatResponsesSuccessfulTerminal(payload []byte, eventName string) bool {
	if !json.Valid(payload) {
		return false
	}
	if errorValue := gjson.GetBytes(payload, "error"); errorValue.Exists() && errorValue.Type != gjson.Null {
		return false
	}
	payloadType := gjson.GetBytes(payload, "type").String()
	if payloadType != "" && eventName != "" && payloadType != eventName {
		return false
	}
	if payloadType != "" {
		eventName = payloadType
	}
	if eventName != "response.completed" && eventName != "response.done" {
		return false
	}
	return OpenAICompatResponsesSuccessfulBody([]byte(gjson.GetBytes(payload, "response").Raw))
}
