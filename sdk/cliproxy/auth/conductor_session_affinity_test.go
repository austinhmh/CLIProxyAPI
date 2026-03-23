package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type sessionAffinityExecutor struct {
	id string
}

func (e *sessionAffinityExecutor) Identifier() string { return e.id }
func (e *sessionAffinityExecutor) Execute(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (e *sessionAffinityExecutor) ExecuteStream(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {}\n\n")}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}
func (e *sessionAffinityExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }
func (e *sessionAffinityExecutor) CountTokens(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{"count":1}`)}, nil
}
func (e *sessionAffinityExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManager_SessionAffinity_PrefersStickyAuthUntilUnavailable(t *testing.T) {
	manager := NewManager(nil, &BalancedHashSelector{}, nil)
	manager.RegisterExecutor(&sessionAffinityExecutor{id: "claude"})

	authA := &Auth{ID: "auth-a", Provider: "claude"}
	authB := &Auth{ID: "auth-b", Provider: "claude"}
	if _, err := manager.Register(context.Background(), authA); err != nil {
		t.Fatalf("register auth-a: %v", err)
	}
	if _, err := manager.Register(context.Background(), authB); err != nil {
		t.Fatalf("register auth-b: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authA.ID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient(authB.ID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(authA.ID)
		reg.UnregisterClient(authB.ID)
	})

	opts1 := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "sess-1",
			"idempotency_key":                           "req-1",
		},
	}
	var firstSelected string
	opts1.Metadata[cliproxyexecutor.SelectedAuthCallbackMetadataKey] = func(id string) { firstSelected = id }
	req := cliproxyexecutor.Request{Model: "test-model", Payload: []byte(`{"input":"hello"}`)}
	if _, err := manager.Execute(context.Background(), []string{"claude"}, req, opts1); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	if firstSelected == "" {
		t.Fatalf("first selected auth is empty")
	}

	opts2 := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "sess-1",
			"idempotency_key":                           "req-2",
		},
	}
	var secondSelected string
	opts2.Metadata[cliproxyexecutor.SelectedAuthCallbackMetadataKey] = func(id string) { secondSelected = id }
	if _, err := manager.Execute(context.Background(), []string{"claude"}, req, opts2); err != nil {
		t.Fatalf("second execute: %v", err)
	}
	if secondSelected != firstSelected {
		t.Fatalf("sticky auth mismatch, first=%s second=%s", firstSelected, secondSelected)
	}

	// Make the sticky auth unavailable, then expect migration.
	manager.MarkResult(context.Background(), Result{
		AuthID:   firstSelected,
		Provider: "claude",
		Model:    "test-model",
		Success:  false,
		Error:    &Error{HTTPStatus: 429, Message: "quota"},
	})

	opts3 := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "sess-1",
			"idempotency_key":                           "req-3",
		},
	}
	var thirdSelected string
	opts3.Metadata[cliproxyexecutor.SelectedAuthCallbackMetadataKey] = func(id string) { thirdSelected = id }
	if _, err := manager.Execute(context.Background(), []string{"claude"}, req, opts3); err != nil {
		t.Fatalf("third execute: %v", err)
	}
	if thirdSelected == "" {
		t.Fatalf("third selected auth is empty")
	}
	if thirdSelected == firstSelected {
		t.Fatalf("expected sticky migration after unavailable auth, still got %s", thirdSelected)
	}
}

func TestManager_SessionAffinity_CloseExecutionSessionClearsBinding(t *testing.T) {
	manager := NewManager(nil, &BalancedHashSelector{}, nil)
	manager.RegisterExecutor(&sessionAffinityExecutor{id: "claude"})

	authA := &Auth{ID: "auth-a", Provider: "claude"}
	if _, err := manager.Register(context.Background(), authA); err != nil {
		t.Fatalf("register auth-a: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authA.ID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() { reg.UnregisterClient(authA.ID) })

	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "sess-clear",
			"idempotency_key":                           "req-clear",
		},
	}
	req := cliproxyexecutor.Request{Model: "test-model", Payload: []byte(`{"input":"hello"}`)}
	if _, err := manager.Execute(context.Background(), []string{"claude"}, req, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}

	manager.mu.RLock()
	_, existsBefore := manager.sessionAffinity["sess-clear"]
	manager.mu.RUnlock()
	if !existsBefore {
		t.Fatalf("expected session affinity binding to exist before close")
	}

	manager.CloseExecutionSession("sess-clear")

	manager.mu.RLock()
	_, existsAfter := manager.sessionAffinity["sess-clear"]
	manager.mu.RUnlock()
	if existsAfter {
		t.Fatalf("expected session affinity binding to be removed after close")
	}
}

