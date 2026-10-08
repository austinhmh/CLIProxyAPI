package middleware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestExtractRequestBodyPrefersOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{
		requestInfo: &RequestInfo{Body: []byte("original-body")},
	}

	body := wrapper.extractRequestBody(c)
	if string(body) != "original-body" {
		t.Fatalf("request body = %q, want %q", string(body), "original-body")
	}

	c.Set(requestBodyOverrideContextKey, []byte("override-body"))
	body = wrapper.extractRequestBody(c)
	if string(body) != "override-body" {
		t.Fatalf("request body = %q, want %q", string(body), "override-body")
	}
}

func TestExtractRequestBodySupportsStringOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{body: &bytes.Buffer{}}
	c.Set(requestBodyOverrideContextKey, "override-as-string")

	body := wrapper.extractRequestBody(c)
	if string(body) != "override-as-string" {
		t.Fatalf("request body = %q, want %q", string(body), "override-as-string")
	}
}

func TestExtractResponseBodyPrefersOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{body: &bytes.Buffer{}}
	wrapper.body.WriteString("original-response")

	body := wrapper.extractResponseBody(c)
	if string(body) != "original-response" {
		t.Fatalf("response body = %q, want %q", string(body), "original-response")
	}

	c.Set(responseBodyOverrideContextKey, []byte("override-response"))
	body = wrapper.extractResponseBody(c)
	if string(body) != "override-response" {
		t.Fatalf("response body = %q, want %q", string(body), "override-response")
	}

	body[0] = 'X'
	if got := wrapper.extractResponseBody(c); string(got) != "override-response" {
		t.Fatalf("response override should be cloned, got %q", string(got))
	}
}

func TestExtractResponseBodySupportsStringOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{}
	c.Set(responseBodyOverrideContextKey, "override-response-as-string")

	body := wrapper.extractResponseBody(c)
	if string(body) != "override-response-as-string" {
		t.Fatalf("response body = %q, want %q", string(body), "override-response-as-string")
	}
}

func TestExtractBodyOverrideClonesBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	override := []byte("body-override")
	c.Set(requestBodyOverrideContextKey, override)

	body := extractBodyOverride(c, requestBodyOverrideContextKey)
	if !bytes.Equal(body, override) {
		t.Fatalf("body override = %q, want %q", string(body), string(override))
	}

	body[0] = 'X'
	if !bytes.Equal(override, []byte("body-override")) {
		t.Fatalf("override mutated: %q", string(override))
	}
}

func TestExtractWebsocketTimelineUsesOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{}
	if got := wrapper.extractWebsocketTimeline(c); got != nil {
		t.Fatalf("expected nil websocket timeline, got %q", string(got))
	}

	c.Set(websocketTimelineOverrideContextKey, []byte("timeline"))
	body := wrapper.extractWebsocketTimeline(c)
	if string(body) != "timeline" {
		t.Fatalf("websocket timeline = %q, want %q", string(body), "timeline")
	}
}

func TestFinalizeStreamingWritesAPIWebsocketTimeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	streamWriter := &testStreamingLogWriter{}
	wrapper := &ResponseWriterWrapper{
		ResponseWriter: c.Writer,
		logger:         &testRequestLogger{enabled: true},
		requestInfo: &RequestInfo{
			URL:       "/v1/responses",
			Method:    "POST",
			Headers:   map[string][]string{"Content-Type": {"application/json"}},
			RequestID: "req-1",
			Timestamp: time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC),
		},
		isStreaming:  true,
		streamWriter: streamWriter,
	}

	c.Set("API_WEBSOCKET_TIMELINE", []byte("Timestamp: 2026-04-01T12:00:00Z\nEvent: api.websocket.request\n{}"))

	if err := wrapper.Finalize(c); err != nil {
		t.Fatalf("Finalize error: %v", err)
	}
	if string(streamWriter.apiWebsocketTimeline) != "Timestamp: 2026-04-01T12:00:00Z\nEvent: api.websocket.request\n{}" {
		t.Fatalf("stream writer websocket timeline = %q", string(streamWriter.apiWebsocketTimeline))
	}
	if !streamWriter.closed {
		t.Fatal("expected stream writer to be closed")
	}
}

func TestWriteRecordsFirstDownstreamWriteWithoutStreamingLogWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)

	timingTracker := logging.NewRequestTimingTracker("req-timing", "POST /v1/responses", time.Now())
	timingTurn := timingTracker.BeginTurn("http_stream", "gpt-test")
	wrapper := &ResponseWriterWrapper{
		ResponseWriter: ginContext.Writer,
		body:           &bytes.Buffer{},
		requestInfo: &RequestInfo{
			RequestID: "req-timing",
			Timestamp: time.Now(),
			timing:    timingTracker,
		},
	}

	if _, errWrite := wrapper.Write([]byte("data: first\n\n")); errWrite != nil {
		t.Fatalf("Write error: %v", errWrite)
	}
	timingTurn.Complete("completed")

	snapshot := timingTracker.Snapshot()
	if len(snapshot.Turns) != 1 {
		t.Fatalf("turn count = %d, want 1", len(snapshot.Turns))
	}
	firstWriteCount := 0
	for _, event := range snapshot.Turns[0].Events {
		if event.Stage == logging.TimingStageFirstDownstreamWrite {
			firstWriteCount++
		}
	}
	if firstWriteCount != 1 {
		t.Fatalf("first downstream write count = %d, want 1", firstWriteCount)
	}
}

func TestWriteInitializesStreamingLoggerWithoutExplicitWriteHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Writer.Header().Set("Content-Type", "text/event-stream")

	streamWriter := &testStreamingLogWriter{}
	logger := &countingStreamingRequestLogger{streamWriter: streamWriter}
	wrapper := NewResponseWriterWrapper(ginContext.Writer, logger, &RequestInfo{
		URL:       "/v1/responses",
		Method:    http.MethodPost,
		RequestID: "req-stream-init",
		Timestamp: time.Now(),
	})

	if _, errWrite := wrapper.Write([]byte("data: first\n\n")); errWrite != nil {
		t.Fatalf("Write error: %v", errWrite)
	}
	if !wrapper.headerWritten || !wrapper.isStreaming {
		t.Fatalf("wrapper state = headerWritten:%t isStreaming:%t", wrapper.headerWritten, wrapper.isStreaming)
	}
	if logger.streamingCalls != 1 {
		t.Fatalf("streaming log initialization count = %d, want 1", logger.streamingCalls)
	}
	wrapper.WriteHeader(http.StatusAccepted)
	if logger.streamingCalls != 1 {
		t.Fatalf("duplicate WriteHeader initialized logger %d times", logger.streamingCalls)
	}
	if errFinalize := wrapper.Finalize(ginContext); errFinalize != nil {
		t.Fatalf("Finalize error: %v", errFinalize)
	}
}

func TestFlushInitializesHeadersWithNilLogger(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	wrapper := NewResponseWriterWrapper(ginContext.Writer, nil, &RequestInfo{Timestamp: time.Now()})

	wrapper.Flush()
	if !wrapper.headerWritten {
		t.Fatal("Flush did not initialize response headers")
	}
}

func TestFinalizeCompletesTimingTurnWithNilLogger(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	timingTracker := logging.NewRequestTimingTracker("req-nil-logger", "POST /v1/responses", time.Now())
	timingTracker.BeginTurn("http_stream", "gpt-test")
	wrapper := NewResponseWriterWrapper(ginContext.Writer, nil, &RequestInfo{
		RequestID: "req-nil-logger",
		Timestamp: time.Now(),
		timing:    timingTracker,
	})

	if errFinalize := wrapper.Finalize(ginContext); errFinalize != nil {
		t.Fatalf("Finalize error: %v", errFinalize)
	}
	snapshot := timingTracker.Snapshot()
	if len(snapshot.Turns) != 1 || !snapshot.Turns[0].Completed {
		t.Fatalf("timing turn was not completed: %#v", snapshot)
	}
}

type testRequestLogger struct {
	enabled bool
}

type countingStreamingRequestLogger struct {
	streamWriter   logging.StreamingLogWriter
	streamingCalls int
}

func (l *countingStreamingRequestLogger) LogRequest(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, []byte, []byte, []byte, []*interfaces.ErrorMessage, string, time.Time, time.Time) error {
	return nil
}

func (l *countingStreamingRequestLogger) LogStreamingRequest(string, string, map[string][]string, []byte, string) (logging.StreamingLogWriter, error) {
	l.streamingCalls++
	return l.streamWriter, nil
}

func (l *countingStreamingRequestLogger) IsEnabled() bool {
	return true
}

func (l *testRequestLogger) LogRequest(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, []byte, []byte, []byte, []*interfaces.ErrorMessage, string, time.Time, time.Time) error {
	return nil
}

func (l *testRequestLogger) LogStreamingRequest(string, string, map[string][]string, []byte, string) (logging.StreamingLogWriter, error) {
	return &testStreamingLogWriter{}, nil
}

func (l *testRequestLogger) IsEnabled() bool {
	return l.enabled
}

type testStreamingLogWriter struct {
	apiWebsocketTimeline []byte
	closed               bool
}

func (w *testStreamingLogWriter) WriteChunkAsync([]byte) {}

func (w *testStreamingLogWriter) WriteStatus(int, map[string][]string) error {
	return nil
}

func (w *testStreamingLogWriter) WriteAPIRequest([]byte) error {
	return nil
}

func (w *testStreamingLogWriter) WriteAPIResponse([]byte) error {
	return nil
}

func (w *testStreamingLogWriter) WriteAPIWebsocketTimeline(apiWebsocketTimeline []byte) error {
	w.apiWebsocketTimeline = bytes.Clone(apiWebsocketTimeline)
	return nil
}

func (w *testStreamingLogWriter) SetFirstChunkTimestamp(time.Time) {}

func (w *testStreamingLogWriter) Close() error {
	w.closed = true
	return nil
}

func TestHasActionableError(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		statusCode int
		ctx        context.Context
		apiErrors  []*interfaces.ErrorMessage
		want       bool
	}{
		{
			name:       "200 ok without errors",
			statusCode: http.StatusOK,
			want:       false,
		},
		{
			name:       "499 client closed request",
			statusCode: clienterror.StatusClientClosedRequest,
			want:       false,
		},
		{
			name:       "499 with context canceled api error",
			statusCode: clienterror.StatusClientClosedRequest,
			apiErrors:  []*interfaces.ErrorMessage{{StatusCode: clienterror.StatusClientClosedRequest, Error: context.Canceled}},
			want:       false,
		},
		{
			name:       "200 with canceled context",
			statusCode: http.StatusOK,
			ctx:        canceledCtx,
			want:       false,
		},
		{
			name:       "0 with canceled context",
			statusCode: 0,
			ctx:        canceledCtx,
			want:       false,
		},
		{
			name:       "400 bad request",
			statusCode: http.StatusBadRequest,
			want:       true,
		},
		{
			name:       "429 rate limit",
			statusCode: http.StatusTooManyRequests,
			want:       true,
		},
		{
			name:       "500 internal server error",
			statusCode: http.StatusInternalServerError,
			want:       true,
		},
		{
			name:       "503 with canceled context",
			statusCode: http.StatusServiceUnavailable,
			ctx:        canceledCtx,
			want:       true,
		},
		{
			name:       "200 with actionable upstream api error",
			statusCode: http.StatusOK,
			apiErrors:  []*interfaces.ErrorMessage{{StatusCode: http.StatusBadGateway, Error: errors.New("upstream failed")}},
			want:       true,
		},
		{
			name:       "200 with non-actionable cancellation api error",
			statusCode: http.StatusOK,
			apiErrors:  []*interfaces.ErrorMessage{{StatusCode: 0, Error: fmt.Errorf("read: %w", context.Canceled)}},
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if tc.ctx != nil {
				req = req.WithContext(tc.ctx)
			}
			c.Request = req

			got := hasActionableError(c, tc.statusCode, tc.apiErrors)
			if got != tc.want {
				t.Fatalf("hasActionableError(status=%d, errors=%v) = %t, want %t", tc.statusCode, tc.apiErrors, got, tc.want)
			}
		})
	}
}

type recordingRequestLogger struct {
	loggedCalls []int
	enabled     bool
}

func (l *recordingRequestLogger) LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	l.loggedCalls = append(l.loggedCalls, statusCode)
	return nil
}

func (l *recordingRequestLogger) LogStreamingRequest(string, string, map[string][]string, []byte, string) (logging.StreamingLogWriter, error) {
	return &testStreamingLogWriter{}, nil
}

func (l *recordingRequestLogger) IsEnabled() bool {
	return l.enabled
}

func (l *recordingRequestLogger) LogRequestWithOptions(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	if force || l.enabled {
		l.loggedCalls = append(l.loggedCalls, statusCode)
	}
	return nil
}

func TestFinalizeExcludes499FromForceLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	logger := &recordingRequestLogger{enabled: false}
	wrapper := &ResponseWriterWrapper{
		ResponseWriter: c.Writer,
		logger:         logger,
		logOnErrorOnly: true,
		statusCode:     clienterror.StatusClientClosedRequest,
		requestInfo: &RequestInfo{
			URL:       "/v1/responses",
			Method:    "POST",
			RequestID: "req-499",
			Timestamp: time.Now(),
		},
	}

	if err := wrapper.Finalize(c); err != nil {
		t.Fatalf("Finalize error: %v", err)
	}
	if len(logger.loggedCalls) != 0 {
		t.Fatalf("expected 0 logged calls for 499 cancellation, got %d: %v", len(logger.loggedCalls), logger.loggedCalls)
	}
}

func TestFinalizeIncludes500InForceLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	logger := &recordingRequestLogger{enabled: false}
	wrapper := &ResponseWriterWrapper{
		ResponseWriter: c.Writer,
		logger:         logger,
		logOnErrorOnly: true,
		statusCode:     http.StatusInternalServerError,
		requestInfo: &RequestInfo{
			URL:       "/v1/responses",
			Method:    "POST",
			RequestID: "req-500",
			Timestamp: time.Now(),
		},
	}

	if err := wrapper.Finalize(c); err != nil {
		t.Fatalf("Finalize error: %v", err)
	}
	if len(logger.loggedCalls) != 1 || logger.loggedCalls[0] != http.StatusInternalServerError {
		t.Fatalf("expected 1 logged call for 500 status, got: %v", logger.loggedCalls)
	}
}
