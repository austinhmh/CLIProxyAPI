package auth

import (
	"errors"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type resultHeadersTestError struct {
	headers http.Header
}

func newResultHeadersTestHeader(remaining string) http.Header {
	headers := make(http.Header)
	headers.Set("X-RateLimit-Remaining", remaining)
	return headers
}

func (errResult *resultHeadersTestError) Error() string {
	return "upstream request failed"
}

func (errResult *resultHeadersTestError) Headers() http.Header {
	return errResult.headers
}

func TestHeadersFromExecResultReadsWrappedErrorHeaders(t *testing.T) {
	expectedHeaders := newResultHeadersTestHeader("7")
	wrappedError := errors.Join(errors.New("outer failure"), &resultHeadersTestError{headers: expectedHeaders})

	actualHeaders := headersFromExecResult(cliproxyexecutor.Response{}, wrappedError)
	if actualHeaders.Get("X-RateLimit-Remaining") != "7" {
		t.Fatalf("wrapped error headers = %v, want %v", actualHeaders, expectedHeaders)
	}
}

func TestHeadersFromExecResultFallsBackToResponseHeaders(t *testing.T) {
	expectedHeaders := newResultHeadersTestHeader("5")
	actualHeaders := headersFromExecResult(
		cliproxyexecutor.Response{Headers: expectedHeaders},
		errors.New("upstream request failed after response creation"),
	)

	if actualHeaders.Get("X-RateLimit-Remaining") != "5" {
		t.Fatalf("response headers = %v, want %v", actualHeaders, expectedHeaders)
	}
}

func TestHeadersFromExecResultFallsBackWhenErrorHeadersAreEmpty(t *testing.T) {
	expectedHeaders := newResultHeadersTestHeader("3")
	actualHeaders := headersFromExecResult(
		cliproxyexecutor.Response{Headers: expectedHeaders},
		&resultHeadersTestError{},
	)

	if actualHeaders.Get("X-RateLimit-Remaining") != "3" {
		t.Fatalf("response headers = %v, want %v", actualHeaders, expectedHeaders)
	}
}

func TestHeadersFromExecResultPrefersNonEmptyErrorHeaders(t *testing.T) {
	responseHeaders := newResultHeadersTestHeader("5")
	errorHeaders := newResultHeadersTestHeader("1")
	actualHeaders := headersFromExecResult(
		cliproxyexecutor.Response{Headers: responseHeaders},
		&resultHeadersTestError{headers: errorHeaders},
	)

	if actualHeaders.Get("X-RateLimit-Remaining") != "1" {
		t.Fatalf("error headers = %v, want %v", actualHeaders, errorHeaders)
	}
}
