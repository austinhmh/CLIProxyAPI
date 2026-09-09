package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const priorityWarmupCompletedResponse = `{"id":"resp_test","object":"response","status":"completed","service_tier":"default","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
const priorityWarmupRequest = `{"model":"test-model","input":"hello","service_tier":"priority"}`

func newPriorityWarmupTestExecutor(endpoint string, enabled bool) (*OpenAICompatExecutor, *cliproxyauth.Auth) {
	executor := NewOpenAICompatExecutor("compat", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{Name: "compat", PriorityCacheWarmup: enabled}},
	})
	auth := &cliproxyauth.Auth{ID: "test-auth", Provider: "compat", Attributes: map[string]string{
		"base_url": endpoint + "/v1", "api_key": "test-key", "compat_name": "compat",
	}}
	return executor, auth
}

func executePriorityWarmupTest(ctx context.Context, executor *OpenAICompatExecutor, auth *cliproxyauth.Auth, payload string, streaming bool, headers http.Header) ([]byte, error) {
	request := cliproxyexecutor.Request{Model: "test-model", Payload: []byte(payload)}
	options := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse,
		OriginalRequest: request.Payload, Stream: streaming, Headers: headers,
	}
	if !streaming {
		response, err := executor.Execute(ctx, auth, request, options)
		return response.Payload, err
	}
	result, err := executor.ExecuteStream(ctx, auth, request, options)
	if err != nil {
		return nil, err
	}
	var output strings.Builder
	var streamError error
	for chunk := range result.Chunks {
		output.Write(chunk.Payload)
		if chunk.Err != nil {
			streamError = chunk.Err
		}
	}
	return []byte(output.String()), streamError
}

func TestOpenAICompatPriorityCompletionGate(t *testing.T) {
	for _, test := range []struct {
		name        string
		streaming   bool
		status      int
		body        string
		readFailure bool
		warm        bool
	}{
		{"completed", false, 200, priorityWarmupCompletedResponse, false, true},
		{"queued", false, 200, `{"object":"response","status":"queued"}`, false, false},
		{"incomplete", false, 200, `{"object":"response","status":"incomplete"}`, false, false},
		{"failed", false, 200, `{"object":"response","status":"failed"}`, false, false},
		{"error", false, 200, `{"error":{"message":"failed"}}`, false, false},
		{"http failure", false, 500, `{"error":{"message":"failed"}}`, false, false},
		{"rate limited", false, 429, `{"error":{"message":"limited"}}`, false, false},
		{"read failure", false, 200, priorityWarmupCompletedResponse, true, false},
		{"stream completed", true, 200, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + priorityWarmupCompletedResponse + "}\n\n", false, true},
		{"stream done", true, 200, "data: {\"type\":\"response.done\",\"response\":" + priorityWarmupCompletedResponse + "}\n\n", false, true},
		{"stream incomplete", true, 200, "data: {\"type\":\"response.incomplete\",\"response\":{\"object\":\"response\",\"status\":\"incomplete\"}}\n\n", false, false},
		{"stream missing status", true, 200, "data: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\"}}\n\n", false, false},
		{"stream error", true, 200, "event: error\ndata: {\"error\":{\"message\":\"failed\"}}\n\n", false, false},
		{"stream premature done", true, 200, "data: [DONE]\n\n", false, false},
		{"stream no terminal", true, 200, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", false, false},
		{"stream read failure", true, 200, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			bodies := make(chan []byte, 3)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
					return
				}
				bodies <- body
				if calls.Add(1) == 1 {
					if test.streaming {
						writer.Header().Set("Content-Type", "text/event-stream")
					} else {
						writer.Header().Set("Content-Type", "application/json")
					}
					if test.readFailure {
						writer.Header().Set("Content-Length", fmt.Sprint(len(test.body)+100))
					}
					writer.WriteHeader(test.status)
					_, _ = io.WriteString(writer, test.body)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, priorityWarmupCompletedResponse)
			}))
			defer server.Close()
			executor, auth := newPriorityWarmupTestExecutor(server.URL, true)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			response, firstErr := executePriorityWarmupTest(ctx, executor, auth, priorityWarmupRequest, test.streaming, nil)
			if test.warm && firstErr != nil {
				t.Fatal(firstErr)
			}
			if test.warm && !strings.Contains(string(response), `"service_tier":"default"`) {
				t.Fatal("upstream tier was not preserved")
			}
			if _, err := executePriorityWarmupTest(ctx, executor, auth, priorityWarmupRequest, false, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case first := <-bodies:
				if gjson.GetBytes(first, "service_tier").Exists() {
					t.Fatal("first request retained priority")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case second := <-bodies:
				if got := gjson.GetBytes(second, "service_tier").String() == "priority"; got != test.warm {
					t.Fatalf("second priority = %v, want %v", got, test.warm)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestOpenAICompatPriorityConcurrentColdRequests(t *testing.T) {
	const concurrency = 16
	arrivals := make(chan []byte, concurrency+1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		arrivals <- body
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, priorityWarmupCompletedResponse)
	}))
	defer server.Close()
	defer releaseOnce.Do(func() { close(release) })
	executor, auth := newPriorityWarmupTestExecutor(server.URL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results := make(chan error, concurrency)
	for index := 0; index < concurrency; index++ {
		go func() {
			_, err := executePriorityWarmupTest(ctx, executor, auth, priorityWarmupRequest, false, nil)
			results <- err
		}()
	}
	for index := 0; index < concurrency; index++ {
		select {
		case body := <-arrivals:
			if gjson.GetBytes(body, "service_tier").Exists() {
				t.Fatal("concurrent cold request retained priority")
			}
		case <-ctx.Done():
			t.Fatal("cold requests waited for an owner: ", ctx.Err())
		}
	}
	releaseOnce.Do(func() { close(release) })
	for index := 0; index < concurrency; index++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := executePriorityWarmupTest(ctx, executor, auth, priorityWarmupRequest, false, nil); err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(<-arrivals, "service_tier").String() != "priority" {
		t.Fatal("success did not enable later priority")
	}
}

func TestOpenAICompatPriorityDefaultPrewarmsAndHeadersIsolate(t *testing.T) {
	bodies := make(chan []byte, 5)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		bodies <- body
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, priorityWarmupCompletedResponse)
	}))
	defer server.Close()
	executor, auth := newPriorityWarmupTestExecutor(server.URL, true)
	auth.Attributes["header:Authorization"] = "$X-Upstream-Key"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, test := range []struct {
		payload, credential string
		priority            bool
	}{
		{`{"model":"test-model","input":"hello"}`, "Bearer first", false},
		{priorityWarmupRequest, "Bearer first", true},
		{priorityWarmupRequest, "Bearer second", false},
		{priorityWarmupRequest, "Bearer second", true},
	} {
		if _, err := executePriorityWarmupTest(ctx, executor, auth, test.payload, false, http.Header{"X-Upstream-Key": {test.credential}}); err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(<-bodies, "service_tier").String() == "priority"; got != test.priority {
			t.Fatalf("priority = %v, want %v", got, test.priority)
		}
	}
}

func TestOpenAICompatPriorityCancellationDoesNotWarm(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint("streaming=", streaming), func(t *testing.T) {
			var calls atomic.Int32
			started := make(chan struct{})
			bodies := make(chan []byte, 2)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return
				}
				bodies <- body
				if calls.Add(1) == 1 {
					if streaming {
						writer.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
					} else {
						writer.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(writer, `{"object":"response","status":`)
					}
					writer.(http.Flusher).Flush()
					close(started)
					<-request.Context().Done()
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, priorityWarmupCompletedResponse)
			}))
			defer server.Close()
			executor, auth := newPriorityWarmupTestExecutor(server.URL, true)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			requestContext, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			finished := make(chan struct{})
			go func() {
				_, _ = executePriorityWarmupTest(requestContext, executor, auth, priorityWarmupRequest, streaming, nil)
				close(finished)
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			cancelRequest()
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("cancelled executor did not finish")
			}
			if _, err := executePriorityWarmupTest(ctx, executor, auth, priorityWarmupRequest, false, nil); err != nil {
				t.Fatal(err)
			}
			for index := 0; index < 2; index++ {
				if gjson.GetBytes(<-bodies, "service_tier").Exists() {
					t.Fatal("cancelled attempt warmed next request")
				}
			}
		})
	}
}

func TestOpenAICompatPriorityCredentialAttemptsAndFailureRetention(t *testing.T) {
	bodies := make(chan []byte, 6)
	var failNext atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		bodies <- body
		writer.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") == "Bearer failing" || failNext.Swap(false) {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(writer, `{"error":{"message":"failed"}}`)
			return
		}
		_, _ = io.WriteString(writer, priorityWarmupCompletedResponse)
	}))
	defer server.Close()
	executor, auth := newPriorityWarmupTestExecutor(server.URL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, test := range []struct {
		credential                      string
		forceFailure, priority, failure bool
	}{
		{"failing", false, false, true},
		{"working", false, false, false},
		{"failing", false, false, true},
		{"working", true, true, true},
		{"working", false, true, false},
	} {
		auth.Attributes["api_key"] = test.credential
		failNext.Store(test.forceFailure)
		_, err := executePriorityWarmupTest(ctx, executor, auth, priorityWarmupRequest, false, nil)
		if (err != nil) != test.failure {
			t.Fatalf("error = %v, want failure %v", err, test.failure)
		}
		if got := gjson.GetBytes(<-bodies, "service_tier").String() == "priority"; got != test.priority {
			t.Fatalf("priority = %v, want %v", got, test.priority)
		}
	}
}

func TestOpenAICompatPriorityDisabledAndCompactUnchanged(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		alt     string
	}{
		{"disabled", false, ""}, {"compact", true, "responses/compact"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bodies := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
					return
				}
				bodies <- body
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, priorityWarmupCompletedResponse)
			}))
			defer server.Close()
			executor, auth := newPriorityWarmupTestExecutor(server.URL, test.enabled)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: "test-model", Payload: []byte(priorityWarmupRequest)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Alt: test.alt})
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(<-bodies, "service_tier").String() != "priority" {
				t.Fatal("out-of-scope request changed")
			}
		})
	}
}
