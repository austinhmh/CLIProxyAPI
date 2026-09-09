package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

// OpenAIResponsesPromptRootFingerprint identifies a content prefix, not a client session.
// It never searches later conversation turns for a user_query marker.
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
			text, textOnly := responsesPromptText(content)
			trimmed := strings.TrimSpace(text)
			if !cursorPreamble && textOnly && strings.HasPrefix(trimmed, "<user_info>") {
				if !responsesCursorBlocks(trimmed, false) {
					return nil, false
				}
				cursorPreamble = true
				continue
			}
			if cursorPreamble {
				if !textOnly {
					return nil, false
				}
				if responsesCursorBlocks(trimmed, true) {
					return items[:index+1], true
				}
				if responsesCursorBlocks(trimmed, false) {
					continue
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

// responsesCursorBlocks accepts complete outer wrappers only. Unknown wrappers
// remain cold rather than guessing where a Cursor preamble ends.
func responsesCursorBlocks(text string, requireQuery bool) bool {
	seenBlock, seenQuery := false, false
	for text = strings.TrimSpace(text); text != ""; text = strings.TrimSpace(text) {
		if !strings.HasPrefix(text, "<") {
			return false
		}
		openingEnd := strings.IndexByte(text, '>')
		if openingEnd < 0 {
			return false
		}
		name := text[1:openingEnd]
		switch name {
		case "user_query":
			if !requireQuery || seenQuery {
				return false
			}
			seenQuery = true
		case "system_reminder":
		case "user_info", "agent_transcripts", "rules", "agent_skills", "mcp_instructions", "attached_files":
			if requireQuery {
				return false
			}
		default:
			return false
		}
		closing := "</" + name + ">"
		closingStart := strings.Index(text[openingEnd+1:], closing)
		if closingStart < 0 {
			return false
		}
		contentEnd := openingEnd + 1 + closingStart
		if name == "user_query" && strings.TrimSpace(text[openingEnd+1:contentEnd]) == "" {
			return false
		}
		text = text[contentEnd+len(closing):]
		seenBlock = true
	}
	return seenBlock && (!requireQuery || seenQuery)
}

func responsesPromptText(content any) (string, bool) {
	switch value := content.(type) {
	case string:
		return value, true
	case []any:
		var text strings.Builder
		for _, rawPart := range value {
			part, valid := rawPart.(map[string]any)
			if !valid || (part["type"] != "input_text" && part["type"] != "text") {
				return "", false
			}
			partText, valid := part["text"].(string)
			if !valid {
				return "", false
			}
			text.WriteString(partText)
			text.WriteByte('\n')
		}
		return text.String(), true
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
