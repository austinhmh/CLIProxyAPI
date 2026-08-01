package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type unauthorizedRefreshExecutor struct {
	id string

	mu             sync.Mutex
	executeCalls   []string
	streamCalls    []string
	refreshCalls   int
	tokenInvalid   map[string]struct{}
	refreshFail    bool
	refreshTokens  map[string]string
	refreshStarted chan struct{}
	allowRefresh   <-chan struct{}
}

func (e *unauthorizedRefreshExecutor) Identifier() string { return e.id }

func (e *unauthorizedRefreshExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeCalls = append(e.executeCalls, auth.ID)
	token := authAccessToken(auth)
	_, invalid := e.tokenInvalid[token]
	e.mu.Unlock()
	if invalid {
		return cliproxyexecutor.Response{}, &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "Your authentication token has been invalidated. Please try signing in again.",
		}
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID + ":" + token)}, nil
}

func (e *unauthorizedRefreshExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamCalls = append(e.streamCalls, auth.ID)
	token := authAccessToken(auth)
	_, invalid := e.tokenInvalid[token]
	e.mu.Unlock()
	if invalid {
		return nil, &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "Your authentication token has been invalidated. Please try signing in again.",
		}
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID + ":" + token)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Auth": {auth.ID}}, Chunks: ch}, nil
}

func (e *unauthorizedRefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.refreshCalls++
	refreshFail := e.refreshFail
	refreshToken := e.refreshTokens[auth.ID]
	refreshStarted := e.refreshStarted
	allowRefresh := e.allowRefresh
	e.mu.Unlock()

	if refreshStarted != nil {
		select {
		case refreshStarted <- struct{}{}:
		default:
		}
	}
	if allowRefresh != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-allowRefresh:
		}
	}
	if refreshFail {
		return nil, &Error{HTTPStatus: http.StatusUnauthorized, Message: "refresh token invalid"}
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if refreshToken == "" {
		refreshToken = "refreshed-access-token"
	}
	auth.Metadata["access_token"] = refreshToken
	return auth, nil
}

func (e *unauthorizedRefreshExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (e *unauthorizedRefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *unauthorizedRefreshExecutor) ExecuteCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.executeCalls))
	copy(out, e.executeCalls)
	return out
}

func (e *unauthorizedRefreshExecutor) StreamCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.streamCalls))
	copy(out, e.streamCalls)
	return out
}

func (e *unauthorizedRefreshExecutor) RefreshCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refreshCalls
}

func newUnauthorizedRefreshFixture(t *testing.T, refreshFail bool) (*Manager, *unauthorizedRefreshExecutor, *Auth, *Auth, string) {
	t.Helper()

	model := "gpt-5.5"
	primary := &Auth{
		ID:       "aa-primary",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token":  "stale-access-token",
			"refresh_token": "primary-refresh-token",
		},
	}
	backup := &Auth{
		ID:       "bb-backup",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token":  "backup-access-token",
			"refresh_token": "backup-refresh-token",
		},
	}

	executor := &unauthorizedRefreshExecutor{
		id: "codex",
		tokenInvalid: map[string]struct{}{
			"stale-access-token": {},
		},
		refreshFail: refreshFail,
		refreshTokens: map[string]string{
			primary.ID: "fresh-access-token",
		},
	}

	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(primary.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(backup.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(primary.ID)
		reg.UnregisterClient(backup.ID)
	})

	if _, errRegister := m.Register(context.Background(), primary); errRegister != nil {
		t.Fatalf("register primary: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), backup); errRegister != nil {
		t.Fatalf("register backup: %v", errRegister)
	}

	return m, executor, primary, backup, model
}

func TestManager_RefreshAuthDisabledBeforeExecutionSkipsExecutor(t *testing.T) {
	testCases := []struct {
		name        string
		disableAuth func(*Auth)
	}{
		{
			name: "disabled flag",
			disableAuth: func(auth *Auth) {
				auth.Disabled = true
			},
		},
		{
			name: "disabled status",
			disableAuth: func(auth *Auth) {
				auth.Status = StatusDisabled
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			executor := &unauthorizedRefreshExecutor{
				id:            "codex",
				refreshTokens: map[string]string{"disabled-before-refresh": "new-access-token"},
			}
			manager.RegisterExecutor(executor)

			auth := &Auth{
				ID:       "disabled-before-refresh",
				Provider: "codex",
				Status:   StatusActive,
				Metadata: map[string]any{
					"access_token":  "old-access-token",
					"refresh_token": "refresh-token",
				},
			}
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}
			storedAuth, ok := manager.GetByID(auth.ID)
			if !ok || storedAuth == nil {
				t.Fatal("registered auth is missing")
			}
			testCase.disableAuth(storedAuth)
			if _, errUpdate := manager.Update(context.Background(), storedAuth); errUpdate != nil {
				t.Fatalf("disable auth: %v", errUpdate)
			}

			refreshedAuth, errRefresh := manager.refreshAuthForRequest(context.Background(), auth.ID, "")
			if !errors.Is(errRefresh, errAuthRefreshDisabled) {
				t.Fatalf("refresh error = %v, want %v", errRefresh, errAuthRefreshDisabled)
			}
			if refreshedAuth != nil {
				t.Fatalf("refreshed auth = %v, want nil", refreshedAuth)
			}
			if refreshCalls := executor.RefreshCalls(); refreshCalls != 0 {
				t.Fatalf("executor refresh calls = %d, want 0", refreshCalls)
			}
		})
	}
}

func TestManager_RefreshAuthDoesNotOverwriteConcurrentDisable(t *testing.T) {
	testCases := []struct {
		name        string
		refreshFail bool
	}{
		{name: "successful refresh", refreshFail: false},
		{name: "unauthorized refresh failure", refreshFail: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			refreshStarted := make(chan struct{}, 1)
			allowRefresh := make(chan struct{})
			manager := NewManager(nil, nil, nil)
			executor := &unauthorizedRefreshExecutor{
				id:             "codex",
				refreshFail:    testCase.refreshFail,
				refreshTokens:  map[string]string{"concurrent-disable": "new-access-token"},
				refreshStarted: refreshStarted,
				allowRefresh:   allowRefresh,
			}
			manager.RegisterExecutor(executor)

			auth := &Auth{
				ID:       "concurrent-disable",
				Provider: "codex",
				Status:   StatusActive,
				Metadata: map[string]any{
					"access_token":  "old-access-token",
					"refresh_token": "refresh-token",
				},
			}
			if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			type refreshResult struct {
				auth *Auth
				err  error
			}
			resultChannel := make(chan refreshResult, 1)
			go func() {
				refreshedAuth, errRefresh := manager.refreshAuthForRequest(ctx, auth.ID, "")
				resultChannel <- refreshResult{auth: refreshedAuth, err: errRefresh}
			}()

			select {
			case <-refreshStarted:
			case <-ctx.Done():
				t.Fatalf("refresh did not start: %v", ctx.Err())
			}

			storedAuth, ok := manager.GetByID(auth.ID)
			if !ok || storedAuth == nil {
				t.Fatal("registered auth is missing while refresh is blocked")
			}
			storedAuth.Disabled = true
			storedAuth.Status = StatusDisabled
			storedAuth.StatusMessage = "disabled during refresh"
			if _, errUpdate := manager.Update(ctx, storedAuth); errUpdate != nil {
				t.Fatalf("disable auth during refresh: %v", errUpdate)
			}
			close(allowRefresh)

			var result refreshResult
			select {
			case result = <-resultChannel:
			case <-ctx.Done():
				t.Fatalf("refresh did not finish: %v", ctx.Err())
			}
			if !errors.Is(result.err, errAuthRefreshDisabled) {
				t.Fatalf("refresh error = %v, want %v", result.err, errAuthRefreshDisabled)
			}
			if result.auth != nil {
				t.Fatalf("refreshed auth = %v, want nil", result.auth)
			}
			if refreshCalls := executor.RefreshCalls(); refreshCalls != 1 {
				t.Fatalf("executor refresh calls = %d, want 1", refreshCalls)
			}

			finalAuth, ok := manager.GetByID(auth.ID)
			if !ok || finalAuth == nil {
				t.Fatal("auth is missing after concurrent disable")
			}
			if !finalAuth.Disabled || finalAuth.Status != StatusDisabled {
				t.Fatalf("auth disabled state = (%t, %q), want (true, %q)", finalAuth.Disabled, finalAuth.Status, StatusDisabled)
			}
			if finalAuth.StatusMessage != "disabled during refresh" {
				t.Fatalf("StatusMessage = %q, want concurrent disable message", finalAuth.StatusMessage)
			}
			if finalAuth.LastError != nil {
				t.Fatalf("LastError = %v, want nil after discarded refresh result", finalAuth.LastError)
			}
			if !finalAuth.LastRefreshedAt.IsZero() {
				t.Fatalf("LastRefreshedAt = %s, want zero after discarded refresh result", finalAuth.LastRefreshedAt)
			}
			if accessToken := authAccessToken(finalAuth); accessToken != "old-access-token" {
				t.Fatalf("access token = %q, want old-access-token", accessToken)
			}
		})
	}
}

func TestManager_Execute_UnauthorizedRefreshesCurrentAuthBeforeFallback(t *testing.T) {
	m, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, false)

	resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error = %v, want success on refreshed primary", errExecute)
	}
	if got := string(resp.Payload); got != primary.ID+":fresh-access-token" {
		t.Fatalf("payload = %q, want refreshed primary response", got)
	}

	if got := executor.RefreshCalls(); got != 1 {
		t.Fatalf("Refresh calls = %d, want 1", got)
	}
	if got := executor.ExecuteCalls(); len(got) != 2 || got[0] != primary.ID || got[1] != primary.ID {
		t.Fatalf("Execute calls = %v, want [primary, primary]", got)
	}
	for _, id := range executor.ExecuteCalls() {
		if id == backup.ID {
			t.Fatalf("backup auth should not be used when refresh recovers primary")
		}
	}

	updated, ok := m.GetByID(primary.ID)
	if !ok || updated == nil {
		t.Fatalf("primary auth missing after refresh")
	}
	if got := authAccessToken(updated); got != "fresh-access-token" {
		t.Fatalf("primary access_token = %q, want fresh-access-token", got)
	}
	if state := updated.ModelStates[model]; state != nil && state.Unavailable {
		t.Fatalf("primary model should not remain suspended after successful refresh retry")
	}
}

func TestManager_ExecuteStream_UnauthorizedRefreshesCurrentAuthBeforeFallback(t *testing.T) {
	m, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, false)

	stream, errStream := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errStream != nil {
		t.Fatalf("ExecuteStream error = %v, want success on refreshed primary", errStream)
	}
	if stream == nil || stream.Chunks == nil {
		t.Fatalf("expected stream result")
	}
	chunk, ok := <-stream.Chunks
	if !ok {
		t.Fatalf("expected stream chunk")
	}
	if chunk.Err != nil {
		t.Fatalf("stream chunk error = %v", chunk.Err)
	}
	if got := string(chunk.Payload); got != primary.ID+":fresh-access-token" {
		t.Fatalf("stream payload = %q, want refreshed primary response", got)
	}

	if got := executor.RefreshCalls(); got != 1 {
		t.Fatalf("Refresh calls = %d, want 1", got)
	}
	if got := executor.StreamCalls(); len(got) != 2 || got[0] != primary.ID || got[1] != primary.ID {
		t.Fatalf("Stream calls = %v, want [primary, primary]", got)
	}
	for _, id := range executor.StreamCalls() {
		if id == backup.ID {
			t.Fatalf("backup auth should not be used when refresh recovers primary")
		}
	}
}

func TestManager_Execute_UnauthorizedRefreshFailureFallsBackToNextAuth(t *testing.T) {
	m, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, true)

	resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error = %v, want success via backup", errExecute)
	}
	if got := string(resp.Payload); got != backup.ID+":backup-access-token" {
		t.Fatalf("payload = %q, want backup response", got)
	}

	if got := executor.RefreshCalls(); got != 1 {
		t.Fatalf("Refresh calls = %d, want 1", got)
	}
	if got := executor.ExecuteCalls(); len(got) != 2 || got[0] != primary.ID || got[1] != backup.ID {
		t.Fatalf("Execute calls = %v, want [primary, backup]", got)
	}

	updated, ok := m.GetByID(primary.ID)
	if !ok || updated == nil {
		t.Fatalf("primary auth missing after failed refresh")
	}
	state := updated.ModelStates[model]
	if state == nil || !state.Unavailable {
		t.Fatalf("expected primary model to be suspended after refresh failure")
	}
	if state.StatusMessage != "unauthorized" && (state.LastError == nil || state.LastError.StatusCode() != http.StatusUnauthorized) {
		t.Fatalf("expected unauthorized suspension, got state=%+v", state)
	}
}

func TestManager_Execute_UnauthorizedWithoutRefreshTokenDoesNotCallRefresh(t *testing.T) {
	model := "gpt-5.5"
	primary := &Auth{
		ID:       "aa-primary-api-key",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "stale-access-token",
		},
	}
	backup := &Auth{
		ID:       "bb-backup-api-key",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "backup-access-token",
		},
	}
	executor := &unauthorizedRefreshExecutor{
		id: "codex",
		tokenInvalid: map[string]struct{}{
			"stale-access-token": {},
		},
	}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(primary.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(backup.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(primary.ID)
		reg.UnregisterClient(backup.ID)
	})
	if _, errRegister := m.Register(context.Background(), primary); errRegister != nil {
		t.Fatalf("register primary: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), backup); errRegister != nil {
		t.Fatalf("register backup: %v", errRegister)
	}

	resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error = %v, want success via backup", errExecute)
	}
	if got := string(resp.Payload); got != backup.ID+":backup-access-token" {
		t.Fatalf("payload = %q, want backup response", got)
	}
	if got := executor.RefreshCalls(); got != 0 {
		t.Fatalf("Refresh calls = %d, want 0 when no refresh_token is present", got)
	}
	if got := executor.ExecuteCalls(); len(got) != 2 || got[0] != primary.ID || got[1] != backup.ID {
		t.Fatalf("Execute calls = %v, want [primary, backup]", got)
	}
}

func TestManager_Execute_UnauthorizedRefreshThenRetryStillFailsFallsBackOnce(t *testing.T) {
	m, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, false)
	// Refresh "succeeds" but hands back another invalidated token.
	executor.refreshTokens[primary.ID] = "still-invalid-token"
	executor.mu.Lock()
	executor.tokenInvalid["still-invalid-token"] = struct{}{}
	executor.mu.Unlock()

	resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error = %v, want success via backup", errExecute)
	}
	if got := string(resp.Payload); got != backup.ID+":backup-access-token" {
		t.Fatalf("payload = %q, want backup response", got)
	}
	if got := executor.RefreshCalls(); got != 1 {
		t.Fatalf("Refresh calls = %d, want 1 (no refresh loop)", got)
	}
	if got := executor.ExecuteCalls(); len(got) != 3 || got[0] != primary.ID || got[1] != primary.ID || got[2] != backup.ID {
		t.Fatalf("Execute calls = %v, want [primary, primary, backup]", got)
	}
}
