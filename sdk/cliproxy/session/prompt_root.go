package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

// OpenAIResponsesPromptRootFingerprint identifies the complete first-turn prompt root,
// not a client session. Later conversation turns are excluded from the root.
func OpenAIResponsesPromptRootFingerprint(payload []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var body map[string]any
	if decoder.Decode(&body) != nil || body == nil {
		return ""
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ""
	}
	prefix, valid := responsesPromptInputPrefix(body["input"])
	if !valid {
		return ""
	}
	root := map[string]any{
		"version": "cpa-openai-responses-prompt-root-v1",
		"input":   canonicalResponsesPromptInput(prefix),
	}
	for _, field := range []string{"instructions", "tools", "reasoning", "text", "tool_choice", "parallel_tool_calls"} {
		if value, exists := body[field]; exists {
			root[field] = value
		}
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return "cpa-openai-responses-root-v1:" + hex.EncodeToString(digest[:])
}

func responsesPromptInputPrefix(input any) (any, bool) {
	if text, valid := input.(string); valid {
		return text, strings.TrimSpace(text) != ""
	}
	items, valid := input.([]any)
	if !valid {
		return nil, false
	}
	cursorPreamble := false
	for index, rawItem := range items {
		item, valid := rawItem.(map[string]any)
		if !valid {
			return nil, false
		}
		itemType, _ := item["type"].(string)
		if itemType != "" && itemType != "message" {
			return nil, false
		}
		role, _ := item["role"].(string)
		switch role {
		case "system", "developer":
			continue
		case "user":
			content := item["content"]
			if content == nil {
				content = item["text"]
			}
			if !hasResponsesPromptContent(content) {
				continue
			}
			text, _ := responsesPromptText(content)
			trimmed := strings.TrimSpace(text)
			if !cursorPreamble && strings.HasPrefix(trimmed, "<user_info>") {
				cursorPreamble = true
				continue
			}
			if cursorPreamble {
				if hasNonEmptyResponsesUserQuery(trimmed) {
					return items[:index+1], true
				}
				return nil, false
			}
			return items[:index+1], true
		default:
			// A history boundary before a root is not a new first user turn.
			return nil, false
		}
	}
	return nil, false
}

func hasNonEmptyResponsesUserQuery(text string) bool {
	const openingTag = "<user_query>"
	const closingTag = "</user_query>"
	openingStart := strings.Index(text, openingTag)
	if openingStart < 0 {
		return false
	}
	contentStart := openingStart + len(openingTag)
	closingOffset := strings.Index(text[contentStart:], closingTag)
	if closingOffset < 0 {
		return false
	}
	return strings.TrimSpace(text[contentStart:contentStart+closingOffset]) != ""
}

func responsesPromptText(content any) (string, bool) {
	switch value := content.(type) {
	case string:
		return value, true
	case []any:
		var text strings.Builder
		textOnly := true
		for _, rawPart := range value {
			part, valid := rawPart.(map[string]any)
			if !valid {
				return "", false
			}
			partType, _ := part["type"].(string)
			if partType != "input_text" && partType != "text" {
				textOnly = false
				continue
			}
			partText, valid := part["text"].(string)
			if !valid {
				return "", false
			}
			text.WriteString(partText)
			text.WriteByte('\n')
		}
		return text.String(), textOnly
	default:
		return "", false
	}
}

func hasResponsesPromptContent(content any) bool {
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		for _, part := range value {
			if hasResponsesPromptContent(part) {
				return true
			}
		}
	case map[string]any:
		for field, child := range value {
			switch field {
			case "type", "role", "id", "status", "annotations":
				continue
			}
			if !ignoredResponsesPromptField(field) && hasResponsesPromptContent(child) {
				return true
			}
		}
	case json.Number, bool:
		return true
	}
	return false
}

func ignoredResponsesPromptField(field string) bool {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "prompt_cache_key", "promptcachekey", "conversation", "conversation_id", "conversationid",
		"session", "session_id", "sessionid", "thread", "thread_id", "threadid",
		"previous_response_id", "previousresponseid", "service_tier", "servicetier",
		"stream", "max_output_tokens", "maxoutputtokens":
		return true
	default:
		return false
	}
}

func canonicalResponsesPromptInput(input any) any {
	switch value := input.(type) {
	case map[string]any:
		canonical := make(map[string]any, len(value))
		for field, child := range value {
			if !ignoredResponsesPromptField(field) {
				canonical[field] = canonicalResponsesPromptInput(child)
			}
		}
		return canonical
	case []any:
		canonical := make([]any, len(value))
		for index, child := range value {
			canonical[index] = canonicalResponsesPromptInput(child)
		}
		return canonical
	default:
		return value
	}
}
