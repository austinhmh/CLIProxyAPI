package helps

import (
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestHasSemanticResponseContent(t *testing.T) {
	testCases := []struct {
		name     string
		format   sdktranslator.Format
		payload  string
		expected bool
	}{
		{name: "openai role only", format: sdktranslator.FormatOpenAI, payload: `data: {"choices":[{"delta":{"role":"assistant"}}]}`, expected: false},
		{name: "openai whitespace text", format: sdktranslator.FormatOpenAI, payload: `data: {"choices":[{"delta":{"content":"  \n"}}]}`, expected: false},
		{name: "openai text", format: sdktranslator.FormatOpenAI, payload: `data: {"choices":[{"delta":{"content":"hello"}}]}`, expected: true},
		{name: "openai reasoning", format: sdktranslator.FormatOpenAI, payload: `data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}`, expected: true},
		{name: "openai tool id only", format: sdktranslator.FormatOpenAI, payload: `data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","function":{"name":"","arguments":""}}]}}]}`, expected: false},
		{name: "openai tool name", format: sdktranslator.FormatOpenAI, payload: `data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"lookup","arguments":""}}]}}]}`, expected: true},
		{name: "responses created", format: sdktranslator.FormatCodex, payload: `{"type":"response.created","response":{"id":"resp_1"}}`, expected: false},
		{name: "responses empty content part", format: sdktranslator.FormatCodex, payload: `{"type":"response.content_part.added","part":{"type":"output_text","text":""}}`, expected: false},
		{name: "responses text delta", format: sdktranslator.FormatCodex, payload: `{"type":"response.output_text.delta","delta":"hello"}`, expected: true},
		{name: "responses text done", format: sdktranslator.FormatCodex, payload: `{"type":"response.output_text.done","text":"hello"}`, expected: true},
		{name: "responses reasoning delta", format: sdktranslator.FormatOpenAIResponse, payload: `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`, expected: true},
		{name: "responses reasoning part", format: sdktranslator.FormatOpenAIResponse, payload: `{"type":"response.reasoning_summary_part.added","part":{"type":"summary_text","text":"thinking"}}`, expected: true},
		{name: "responses reasoning output", format: sdktranslator.FormatOpenAIResponse, payload: `{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]}]}`, expected: true},
		{name: "responses unknown object delta", format: sdktranslator.FormatOpenAIResponse, payload: `{"type":"response.metadata.delta","delta":{"metadata":"value"}}`, expected: false},
		{name: "responses arguments delta", format: sdktranslator.FormatOpenAIResponse, payload: `{"type":"response.function_call_arguments.delta","delta":"{"}`, expected: true},
		{name: "responses object arguments delta", format: sdktranslator.FormatOpenAIResponse, payload: `{"type":"response.function_call_arguments.delta","delta":{"metadata":"value"}}`, expected: false},
		{name: "responses partial image", format: sdktranslator.FormatOpenAIResponse, payload: `{"type":"response.image_generation_call.partial_image","partial_image_b64":"aGVsbG8="}`, expected: true},
		{name: "responses completed metadata only", format: sdktranslator.FormatOpenAIResponse, payload: `{"type":"response.completed","response":{"output":[],"usage":{"total_tokens":1}}}`, expected: false},
		{name: "responses completed fallback content", format: sdktranslator.FormatOpenAIResponse, payload: `{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"final"}]}]}}`, expected: true},
		{name: "claude message start", format: sdktranslator.FormatClaude, payload: `data: {"type":"message_start","message":{"content":[]}}`, expected: false},
		{name: "claude ping", format: sdktranslator.FormatClaude, payload: `data: {"type":"ping"}`, expected: false},
		{name: "claude text delta", format: sdktranslator.FormatClaude, payload: `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`, expected: true},
		{name: "claude thinking delta", format: sdktranslator.FormatClaude, payload: `data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thinking"}}`, expected: true},
		{name: "claude tool name", format: sdktranslator.FormatClaude, payload: `data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"lookup","input":{}}}`, expected: true},
		{name: "gemini usage only", format: sdktranslator.FormatGemini, payload: `{"usageMetadata":{"totalTokenCount":1}}`, expected: false},
		{name: "gemini whitespace text", format: sdktranslator.FormatGemini, payload: `{"candidates":[{"content":{"parts":[{"text":" \n"}]}}]}`, expected: false},
		{name: "gemini text", format: sdktranslator.FormatGemini, payload: `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`, expected: true},
		{name: "gemini function call", format: sdktranslator.FormatGemini, payload: `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]}}]}`, expected: true},
		{name: "gemini formatted empty arguments", format: sdktranslator.FormatGemini, payload: "{\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"\",\"args\":{ }}}]}}]}", expected: false},
		{name: "gemini inline media", format: sdktranslator.FormatAntigravity, payload: `{"response":{"candidates":[{"content":{"parts":[{"inlineData":{"data":"aGVsbG8="}}]}}]}}`, expected: true},
		{name: "interactions control", format: sdktranslator.FormatInteractions, payload: `data: {"event_type":"interaction.created","interaction":{"id":"i_1"}}`, expected: false},
		{name: "interactions text delta", format: sdktranslator.FormatInteractions, payload: `data: {"event_type":"step.delta","delta":{"type":"text","text":"hello"}}`, expected: true},
		{name: "interactions reasoning delta", format: sdktranslator.FormatInteractions, payload: `data: {"event_type":"step.delta","delta":{"type":"thought_summary","content":{"text":"thinking"}}}`, expected: true},
		{name: "interactions tool start", format: sdktranslator.FormatInteractions, payload: `data: {"event_type":"step.start","step":{"type":"function_call","name":"lookup","arguments":{}}}`, expected: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			actual := HasSemanticResponseContent(testCase.format, []byte(testCase.payload))
			if actual != testCase.expected {
				t.Fatalf("HasSemanticResponseContent() = %t, want %t", actual, testCase.expected)
			}
		})
	}
}
