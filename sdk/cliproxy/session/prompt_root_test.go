package session

import (
	"encoding/json"
	"testing"
)

func TestOpenAIResponsesPromptRootAppendStability(t *testing.T) {
	tests := []struct {
		name     string
		initial  string
		appended string
	}{
		{"ordinary", `{"input":[{"role":"user","content":"hello"}]}`, `{"input":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"<user_query>later</user_query>"}]}`},
		{"cursor", `{"input":[{"role":"user","content":"<user_info>state</user_info><rules>rules</rules>"},{"role":"user","content":"<system_reminder>agent</system_reminder><user_query>hello</user_query>"}]}`, `{"input":[{"role":"user","content":"<user_info>state</user_info><rules>rules</rules>"},{"role":"user","content":"<system_reminder>agent</system_reminder><user_query>hello</user_query>"},{"role":"assistant","content":"hi"},{"role":"user","content":"<user_info>new state</user_info><user_query>later</user_query>"}]}`},
		{"empty user", `{"input":[{"role":"user","content":" "},{"role":"user","content":"hello"}]}`, `{"input":[{"role":"user","content":" "},{"role":"user","content":"hello"},{"role":"user","content":"later"}]}`},
		{"multimodal", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/image"}]}]}`, `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/image"}]},{"role":"user","content":"later"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := OpenAIResponsesPromptRootFingerprint([]byte(test.initial))
			if root == "" || root != OpenAIResponsesPromptRootFingerprint([]byte(test.appended)) {
				t.Fatal("appending history changed a valid root")
			}
		})
	}
}

func TestOpenAIResponsesPromptRootInvalidBoundary(t *testing.T) {
	for _, payload := range []string{
		`null`, `[]`, `{}`, `{"input":""}`, `{"input":"hi"} {}`,
		`{"input":[{"role":"assistant","content":"history"},{"role":"user","content":"hello"}]}`,
		`{"input":[{"type":"item_reference","id":"previous"},{"role":"user","content":"hello"}]}`,
		`{"input":[{"role":"user","content":"<user_info>state</user_info>extra"},{"role":"user","content":"<user_query>hello</user_query>"}]}`,
		`{"input":[{"role":"user","content":"<user_info>state"}]}`,
		`{"input":[{"role":"user","content":"<user_info>state</user_info><unknown>other</unknown>"}]}`,
		`{"input":[{"role":"user","content":"<user_info>state</user_info>"}]}`,
		`{"input":[{"role":"user","content":"<user_info>state</user_info>"},{"role":"assistant","content":"history"},{"role":"user","content":"<user_query>later</user_query>"}]}`,
		`{"input":[{"role":"user","content":"<user_info>state</user_info>"},{"role":"user","content":"<user_query> </user_query>"}]}`,
	} {
		if root := OpenAIResponsesPromptRootFingerprint([]byte(payload)); root != "" {
			t.Errorf("invalid root boundary accepted: %s", payload)
		}
	}
}

func TestOpenAIResponsesPromptRootFields(t *testing.T) {
	base := map[string]any{"input": "hello"}
	fingerprint := func(body map[string]any) string {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return OpenAIResponsesPromptRootFingerprint(encoded)
	}
	root := fingerprint(base)
	if root == "" {
		t.Fatal("string input has no root")
	}
	for _, field := range []string{"prompt_cache_key", "conversation", "session_id", "thread_id", "previous_response_id", "service_tier", "stream", "max_output_tokens", "model"} {
		base[field] = "changed"
		if fingerprint(base) != root {
			t.Errorf("identity/transport field %s changed root", field)
		}
		delete(base, field)
	}
	for _, field := range []string{"instructions", "tools", "reasoning", "text", "tool_choice", "parallel_tool_calls"} {
		base[field] = "changed"
		if fingerprint(base) == root {
			t.Errorf("prompt field %s did not change root", field)
		}
		delete(base, field)
	}
	first := []byte(`{"tools":[{"type":"function","name":"test"}],"input":[{"role":"user","content":"hello","session_id":"one"}]}`)
	second := []byte(`{"input":[{"session_id":"two","content":"hello","role":"user"}],"tools":[{"name":"test","type":"function"}]}`)
	if OpenAIResponsesPromptRootFingerprint(first) != OpenAIResponsesPromptRootFingerprint(second) {
		t.Fatal("key order or nested identity changed root")
	}
}

func TestOpenAIResponsesPromptRootCursorFirstTurnChanges(t *testing.T) {
	fingerprint := func(state, query string) string {
		body, err := json.Marshal(map[string]any{"input": []any{
			map[string]any{"role": "user", "content": "<user_info>" + state + "</user_info>"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<user_query>" + query + "</user_query>"}}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return OpenAIResponsesPromptRootFingerprint(body)
	}
	root := fingerprint("state", "query")
	if root == "" || root == fingerprint("other", "query") || root == fingerprint("state", "other") {
		t.Fatal("first-turn state/query must participate in root")
	}
}
