package helps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatPriorityPrepareExactTierAndUnhashableRoot(t *testing.T) {
	runtime := NewOpenAICompatPriorityWarmupRuntime()
	for _, test := range []struct {
		payload             string
		removed, hasAttempt bool
	}{
		{`{"model":"test","input":"hello","service_tier":"priority"}`, true, true},
		{`{"model":"test","input":"hello","service_tier":"PRIORITY"}`, false, true},
		{`{"model":"test","input":"hello","service_tier":" priority "}`, false, true},
		{`{"model":"test","input":"hello","service_tier":"default"}`, false, true},
		{`{"model":"test","input":"hello","service_tier":123}`, false, true},
		{`{"model":"test","input":[],"service_tier":"priority"}`, true, false},
		{`{"model":"test","input":[],"service_tier":"default"}`, false, false},
	} {
		payload := []byte(test.payload)
		request, err := http.NewRequest(http.MethodPost, "https://example.test/v1/responses", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		updated, attempt, err := runtime.Prepare(request, "provider", "auth", nil, payload, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if (attempt != nil) != test.hasAttempt {
			t.Fatalf("attempt mismatch for %s", test.payload)
		}
		if test.removed {
			if gjson.GetBytes(updated, "service_tier").Exists() {
				t.Fatalf("priority not removed: %s", test.payload)
			}
		} else if !bytes.Equal(payload, updated) {
			t.Fatalf("non-priority body changed: %s", test.payload)
		}
	}
}

func TestOpenAICompatPriorityRuntimeSlidingExpiry(t *testing.T) {
	clock := time.Unix(1000, 0)
	runtime := NewOpenAICompatPriorityWarmupRuntime()
	runtime.now = func() time.Time { return clock }
	key := sha256.Sum256([]byte("root"))
	if runtime.isWarm(key) {
		t.Fatal("new root is warm")
	}
	runtime.markSuccess(key, OpenAICompatPrioritySignalContentRoot)
	clock = clock.Add(4 * time.Minute)
	if !runtime.isWarm(key) {
		t.Fatal("success expired early")
	}
	runtime.markSuccess(key, OpenAICompatPrioritySignalSessionID)
	clock = clock.Add(4 * time.Minute)
	if !runtime.isWarm(key) {
		t.Fatal("success did not slide expiry")
	}
	entry := runtime.entries[key].Value.(priorityWarmupEntry)
	if entry.signals != OpenAICompatPrioritySignalContentRoot|OpenAICompatPrioritySignalSessionID {
		t.Fatal("success did not merge signals")
	}
	clock = clock.Add(time.Minute)
	if runtime.isWarm(key) || len(runtime.entries) != 0 {
		t.Fatal("lookup refreshed lifetime or failed to remove expired entry")
	}
}

func TestOpenAICompatPriorityRuntimeCapacityAndConcurrency(t *testing.T) {
	runtime := NewOpenAICompatPriorityWarmupRuntime()
	runtime.capacity = 2
	first := sha256.Sum256([]byte("first"))
	second := sha256.Sum256([]byte("second"))
	third := sha256.Sum256([]byte("third"))
	var workers sync.WaitGroup
	for index := 0; index < 64; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if runtime.isWarm(first) {
				t.Error("cold lookup created warm state")
			}
		}()
	}
	workers.Wait()
	if len(runtime.entries) != 0 {
		t.Fatal("cold lookup inserted warming state")
	}
	for index := 0; index < 64; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			runtime.markSuccess(first, OpenAICompatPrioritySignalContentRoot)
			if !runtime.isWarm(first) {
				t.Error("concurrent success missing")
			}
		}()
	}
	workers.Wait()
	runtime.markSuccess(second, 0)
	if !runtime.isWarm(first) {
		t.Fatal("missing first success")
	}
	runtime.markSuccess(third, 0)
	if runtime.isWarm(first) || !runtime.isWarm(second) || !runtime.isWarm(third) {
		t.Fatal("capacity must evict oldest success, not least recently read")
	}
}

func TestOpenAICompatPriorityPrepareScopeAndBody(t *testing.T) {
	runtime := NewOpenAICompatPriorityWarmupRuntime()
	payload := []byte(`{"model":"Model","input":"hello","service_tier":"priority"}`)
	prepare := func(body []byte, authorization, provider, endpoint, host string, headers http.Header, attrs map[string]string) ([]byte, *OpenAICompatPriorityWarmupAttempt) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Host = host
		request.Header = headers.Clone()
		if request.Header == nil {
			request.Header = make(http.Header)
		}
		request.Header.Set("Authorization", authorization)
		updated, attempt, err := runtime.Prepare(request, provider, "auth", attrs, body, cliproxyexecutor.Request{Payload: body}, cliproxyexecutor.Options{})
		if err != nil {
			t.Fatal(err)
		}
		actual, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := request.GetBody()
		if err != nil {
			t.Fatal(err)
		}
		defer replay.Close()
		replayed, err := io.ReadAll(replay)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, updated) || !bytes.Equal(replayed, updated) || request.ContentLength != int64(len(updated)) {
			t.Fatal("request body/replay/length diverged from translated bytes")
		}
		return updated, attempt
	}
	endpoint := "https://example.test/v1/responses"
	first, attempt := prepare(payload, "Bearer first", "provider", endpoint, "example.test", nil, nil)
	if gjson.GetBytes(first, "service_tier").Exists() || attempt == nil {
		t.Fatal("first request was not gated")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	attempt.MarkSuccess(cancelled)
	if runtime.isWarm(attempt.key) {
		t.Fatal("cancelled request marked success")
	}
	attempt.MarkSuccess(context.Background())
	same, _ := prepare(payload, "Bearer first", "provider", endpoint, "example.test", nil, nil)
	if !bytes.Equal(same, payload) {
		t.Fatal("warm request changed")
	}
	for _, test := range []struct {
		name                                 string
		body                                 []byte
		credential, provider, endpoint, host string
	}{
		{"credential", payload, "Bearer second", "provider", endpoint, "example.test"},
		{"provider", payload, "Bearer first", "other", endpoint, "example.test"},
		{"url", payload, "Bearer first", "provider", endpoint + "?tenant=other", "example.test"},
		{"host", payload, "Bearer first", "provider", endpoint, "other.test"},
		{"model case", []byte(`{"model":"model","input":"hello","service_tier":"priority"}`), "Bearer first", "provider", endpoint, "example.test"},
		{"root", []byte(`{"model":"Model","input":"other","service_tier":"priority"}`), "Bearer first", "provider", endpoint, "example.test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			updated, _ := prepare(test.body, test.credential, test.provider, test.endpoint, test.host, nil, nil)
			if gjson.GetBytes(updated, "service_tier").Exists() {
				t.Fatal("different scope inherited warm state")
			}
		})
	}
	withSession := []byte(`{"model":"Model","input":"hello","service_tier":"priority","prompt_cache_key":"different","session_id":"different"}`)
	updated, _ := prepare(withSession, "Bearer first", "provider", endpoint, "example.test", http.Header{"X-Session-Id": {"changed"}}, map[string]string{"header:X-Session-ID": "$session"})
	if !gjson.GetBytes(updated, "service_tier").Exists() {
		t.Fatal("identity fields split the content root")
	}
	updated, _ = prepare(payload, "Bearer first", "provider", endpoint, "example.test", http.Header{"X-Tenant": {"new"}}, map[string]string{"header:X-Tenant": "$tenant"})
	if gjson.GetBytes(updated, "service_tier").Exists() {
		t.Fatal("custom upstream scope inherited warm state")
	}
}

func TestOpenAICompatPrioritySuccessfulResponses(t *testing.T) {
	for _, test := range []struct {
		body    string
		success bool
	}{
		{`{"object":"response","status":"completed","error":null}`, true},
		{`{}`, false}, {`null`, false}, {`[]`, false}, {`invalid`, false},
		{`{"object":"response"}`, false},
		{`{"object":"response","status":"queued"}`, false},
		{`{"object":"response","status":"in_progress"}`, false},
		{`{"object":"response","status":"incomplete"}`, false},
		{`{"object":"response","status":"failed"}`, false},
		{`{"object":"response","status":"cancelled"}`, false},
		{`{"object":"response","status":"completed","error":{"message":"failure"}}`, false},
	} {
		if actual := OpenAICompatResponsesSuccessfulBody([]byte(test.body)); actual != test.success {
			t.Errorf("success(%s) = %v", test.body, actual)
		}
	}
	for _, test := range []struct {
		event, payload string
		success        bool
	}{
		{"response.completed", `{"response":{"object":"response","status":"completed"}}`, true},
		{"", `{"type":"response.done","response":{"object":"response","status":"completed"}}`, true},
		{"response.done", `{"response":{"object":"response","status":"in_progress"}}`, false},
		{"response.incomplete", `{"response":{"object":"response","status":"completed"}}`, false},
		{"response.done", `{"response":{"object":"response"}}`, false},
		{"response.incomplete", `{"type":"response.completed","response":{"object":"response","status":"completed"}}`, false},
	} {
		if actual := OpenAICompatResponsesSuccessfulTerminal([]byte(test.payload), test.event); actual != test.success {
			t.Errorf("terminal(%s, %s) = %v", test.event, test.payload, actual)
		}
	}
}
