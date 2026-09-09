package cliproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestServiceOpenAICompatPriorityRuntimeSurvivesReplacement(t *testing.T) {
	bodies := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		bodies <- body
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"resp_test","object":"response","status":"completed","output":[]}`)
	}))
	defer server.Close()
	newConfig := func() *config.Config {
		return &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "compat", PriorityCacheWarmup: true}}}
	}
	service := &Service{cfg: newConfig(), coreManager: coreauth.NewManager(nil, nil, nil)}
	auth := &coreauth.Auth{ID: "auth", Provider: "compat", Attributes: map[string]string{
		"base_url": server.URL + "/v1", "api_key": "first", "compat_name": "compat",
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	register := func() coreauth.ProviderExecutor {
		t.Helper()
		service.executorRegistrationMu.Lock()
		service.registerOpenAICompatProviderExecutor("compat", auth, service.cfg, true, false)
		service.executorRegistrationMu.Unlock()
		registered, exists := service.coreManager.Executor("compat")
		if !exists {
			t.Fatal("compat executor not registered")
		}
		return registered
	}
	execute := func(executor coreauth.ProviderExecutor, priority bool) {
		t.Helper()
		payload := []byte(`{"model":"test-model","input":"hello","service_tier":"priority"}`)
		_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: "test-model", Payload: payload}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(<-bodies, "service_tier").String() == "priority"; got != priority {
			t.Fatalf("priority = %v, want %v", got, priority)
		}
	}
	first := register()
	execute(first, false)
	runtime := service.openAICompatPriorityWarmupRuntime
	service.cfgMu.Lock()
	service.cfg = newConfig()
	service.cfgMu.Unlock()
	second := register()
	if first == second || runtime != service.openAICompatPriorityWarmupRuntime {
		t.Fatal("replacement did not preserve Service runtime")
	}
	execute(second, true)
	auth.Attributes["api_key"] = "second"
	third := register()
	execute(third, false)
	execute(third, true)
}
