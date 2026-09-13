package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"strings"
)

var excludedRoleNames = map[string]struct{}{
	"system":    {},
	"developer": {},
	"assistant": {},
	"tool":      {},
}

var promptFieldNames = []string{"prompt", "query", "input", "text", "description"}

// ExtractRequest parses only known user-input locations. It intentionally has
// no raw-body fallback: an unknown protocol must never cause a system prompt
// to be persisted accidentally.
func ExtractRequest(path, contentType string, body []byte, maxPromptBytes int) Extraction {
	extraction := Extraction{
		Protocol:      protocolForPath(path),
		ExcludedRoles: map[string]int{},
	}
	if len(body) == 0 {
		extraction.Status = "empty"
		return extraction.finalize(maxPromptBytes)
	}

	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch {
	case mediaType == "application/x-www-form-urlencoded":
		extractFormValues(&extraction, body)
	case mediaType == "multipart/form-data":
		if err := extractMultipart(&extraction, contentType, body); err != nil {
			extraction.Status = "unsupported"
		}
	default:
		var root any
		if err := json.Unmarshal(body, &root); err != nil {
			extraction.Status = "unsupported"
			return extraction.finalize(maxPromptBytes)
		}
		extraction.Model = modelFromJSON(root)
		extractJSONByProtocol(&extraction, root)
	}
	return extraction.finalize(maxPromptBytes)
}

// ExtractWebSocketMessage applies the same allow-list to one client text
// frame. Server frames are never passed to this function.
func ExtractWebSocketMessage(path string, payload []byte, maxPromptBytes int) Extraction {
	extraction := Extraction{
		Protocol:      protocolForPath(path),
		ExcludedRoles: map[string]int{},
	}
	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		extraction.Status = "unsupported"
		return extraction.finalize(maxPromptBytes)
	}
	extraction.Model = modelFromJSON(root)
	if object, ok := root.(map[string]any); ok {
		eventType, _ := object["type"].(string)
		switch strings.ToLower(strings.TrimSpace(eventType)) {
		case "response.create":
			extraction.Protocol = "openai-responses-ws"
			extractResponses(&extraction, object)
		case "conversation.item.create":
			extraction.Protocol = "openai-responses-ws"
			if item, ok := object["item"].(map[string]any); ok {
				extractRoleMessage(&extraction, item, "item")
			} else {
				extraction.Status = "unsupported"
			}
		default:
			extractJSONByProtocol(&extraction, root)
		}
	} else {
		extraction.Status = "unsupported"
	}
	return extraction.finalize(maxPromptBytes)
}

func extractJSONByProtocol(extraction *Extraction, root any) {
	object, ok := root.(map[string]any)
	if !ok {
		extraction.Status = "unsupported"
		return
	}
	protocol := extraction.Protocol
	switch {
	case protocol == "openai-chat":
		extractOpenAIChat(extraction, object)
	case protocol == "openai-responses" || protocol == "openai-responses-ws":
		extractResponses(extraction, object)
	case protocol == "anthropic-messages":
		extractAnthropic(extraction, object)
	case protocol == "gemini":
		extractGemini(extraction, object)
	default:
		extractGeneric(extraction, object)
	}
}

func extractOpenAIChat(extraction *Extraction, object map[string]any) {
	if value, ok := object["system"]; ok && hasValue(value) {
		incrementExcluded(extraction, "system")
	}
	if value, ok := object["developer"]; ok && hasValue(value) {
		incrementExcluded(extraction, "developer")
	}
	messages, ok := object["messages"].([]any)
	if !ok {
		extractGeneric(extraction, object)
		return
	}
	for index, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		extractRoleMessage(extraction, message, fmt.Sprintf("messages[%d]", index))
	}
	if len(messages) == 0 {
		extraction.Status = "empty"
	}
}

func extractResponses(extraction *Extraction, object map[string]any) {
	if value, ok := object["instructions"]; ok && hasValue(value) {
		incrementExcluded(extraction, "system")
	}
	input, exists := object["input"]
	if !exists {
		if len(extraction.Messages) == 0 {
			extraction.Status = "empty"
		}
		return
	}
	switch value := input.(type) {
	case string:
		addUserMessage(extraction, value, "input")
	case []any:
		for index, raw := range value {
			if text, ok := raw.(string); ok {
				addUserMessage(extraction, text, fmt.Sprintf("input[%d]", index))
				continue
			}
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := item["role"].(string)
			if strings.TrimSpace(role) == "" {
				if itemType, _ := item["type"].(string); itemType == "input_text" {
					if text, _ := item["text"].(string); text != "" {
						addUserMessage(extraction, text, fmt.Sprintf("input[%d].text", index))
					}
					continue
				}
			}
			extractRoleMessage(extraction, item, fmt.Sprintf("input[%d]", index))
		}
	case map[string]any:
		extractRoleMessage(extraction, value, "input")
	default:
		extraction.Status = "partial"
	}
}

func extractAnthropic(extraction *Extraction, object map[string]any) {
	if value, ok := object["system"]; ok && hasValue(value) {
		incrementExcluded(extraction, "system")
	}
	messages, ok := object["messages"].([]any)
	if !ok {
		extraction.Status = "empty"
		return
	}
	for index, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		extractRoleMessage(extraction, message, fmt.Sprintf("messages[%d]", index))
	}
}

func extractGemini(extraction *Extraction, object map[string]any) {
	for _, key := range []string{"systemInstruction", "system_instruction"} {
		if value, ok := object[key]; ok && hasValue(value) {
			incrementExcluded(extraction, "system")
		}
	}
	contents, ok := object["contents"].([]any)
	if !ok {
		extractGeneric(extraction, object)
		return
	}
	for index, raw := range contents {
		content, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := content["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "model":
			incrementExcluded(extraction, "assistant")
			continue
		case "system", "developer":
			incrementExcluded(extraction, strings.ToLower(strings.TrimSpace(role)))
			continue
		}
		// Gemini omits role for some single-turn requests; an absent role is
		// treated as user content because systemInstruction was handled above.
		extractParts(extraction, content["parts"], fmt.Sprintf("contents[%d].parts", index))
	}
}

func extractGeneric(extraction *Extraction, object map[string]any) {
	if messages, ok := object["messages"].([]any); ok {
		for index, raw := range messages {
			message, ok := raw.(map[string]any)
			if ok {
				extractRoleMessage(extraction, message, fmt.Sprintf("messages[%d]", index))
			}
		}
		if len(extraction.Messages) > 0 || len(messages) > 0 {
			return
		}
	}
	for _, key := range promptFieldNames {
		value, exists := object[key]
		if !exists {
			continue
		}
		switch typed := value.(type) {
		case string:
			addUserMessage(extraction, typed, key)
		case []any:
			for index, item := range typed {
				if text, ok := item.(string); ok {
					addUserMessage(extraction, text, fmt.Sprintf("%s[%d]", key, index))
				}
			}
		}
	}
	if len(extraction.Messages) == 0 {
		extraction.Status = "unsupported"
	} else {
		extraction.Status = "partial"
	}
}

func extractRoleMessage(extraction *Extraction, object map[string]any, source string) {
	role, _ := object["role"].(string)
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		role = "user"
	}
	if _, excluded := excludedRoleNames[role]; excluded {
		incrementExcluded(extraction, role)
		return
	}
	if role != "user" {
		return
	}
	if content, ok := object["content"]; ok {
		extractContent(extraction, content, source+".content")
		return
	}
	if parts, ok := object["parts"]; ok {
		extractParts(extraction, parts, source+".parts")
		return
	}
	if text, ok := object["text"].(string); ok {
		addUserMessage(extraction, text, source+".text")
	}
}

func extractContent(extraction *Extraction, value any, source string) {
	switch typed := value.(type) {
	case string:
		addUserMessage(extraction, typed, source)
	case []any:
		for index, item := range typed {
			partSource := fmt.Sprintf("%s[%d]", source, index)
			switch part := item.(type) {
			case string:
				addUserMessage(extraction, part, partSource)
			case map[string]any:
				partType, _ := part["type"].(string)
				if text, ok := part["text"].(string); ok &&
					(strings.EqualFold(partType, "") || strings.EqualFold(partType, "text") || strings.EqualFold(partType, "input_text")) {
					addUserMessage(extraction, text, partSource+".text")
				} else if nested, ok := part["content"]; ok {
					extractContent(extraction, nested, partSource+".content")
				}
			}
		}
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			addUserMessage(extraction, text, source+".text")
		} else if nested, ok := typed["content"]; ok {
			extractContent(extraction, nested, source+".content")
		}
	}
}

func extractParts(extraction *Extraction, value any, source string) {
	extractContent(extraction, value, source)
}

func addUserMessage(extraction *Extraction, text, source string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	extraction.Messages = append(extraction.Messages, UserMessage{
		Index:  len(extraction.Messages),
		Text:   text,
		Source: source,
	})
}

func incrementExcluded(extraction *Extraction, role string) {
	if extraction.ExcludedRoles == nil {
		extraction.ExcludedRoles = map[string]int{}
	}
	extraction.ExcludedRoles[role]++
}

func hasValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

func modelFromJSON(root any) string {
	object, ok := root.(map[string]any)
	if !ok {
		return ""
	}
	model, _ := object["model"].(string)
	return strings.TrimSpace(model)
}

func protocolForPath(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.Contains(lower, "/chat/completions"):
		return "openai-chat"
	case strings.Contains(lower, "/responses"):
		return "openai-responses"
	case strings.Contains(lower, "/messages"):
		return "anthropic-messages"
	case strings.Contains(lower, "generatecontent") || strings.Contains(lower, "/v1beta"):
		return "gemini"
	case strings.Contains(lower, "/images") || strings.Contains(lower, "/videos") || strings.Contains(lower, "/media"):
		return "media"
	default:
		return "generic"
	}
}

func modelFromPath(path string) string {
	lower := strings.ToLower(path)
	marker := "/models/"
	index := strings.Index(lower, marker)
	if index < 0 {
		return ""
	}
	model := path[index+len(marker):]
	if separator := strings.IndexByte(model, ':'); separator >= 0 {
		model = model[:separator]
	}
	if separator := strings.IndexByte(model, '/'); separator >= 0 {
		model = model[:separator]
	}
	return strings.TrimSpace(model)
}

func extractFormValues(extraction *Extraction, body []byte) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		extraction.Status = "unsupported"
		return
	}
	for _, key := range promptFieldNames {
		for _, value := range values[key] {
			addUserMessage(extraction, value, key)
		}
	}
	if len(extraction.Messages) == 0 {
		extraction.Status = "unsupported"
	} else {
		extraction.Status = "partial"
	}
}

func extractMultipart(extraction *Extraction, contentType string, body []byte) error {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return err
	}
	boundary := params["boundary"]
	if boundary == "" {
		return fmt.Errorf("multipart boundary is missing")
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.ToLower(strings.TrimSpace(part.FormName()))
		if !isPromptField(name) || part.FileName() != "" {
			continue
		}
		value, err := io.ReadAll(io.LimitReader(part, 1<<20))
		if err != nil {
			return err
		}
		addUserMessage(extraction, string(value), name)
	}
	if len(extraction.Messages) == 0 {
		extraction.Status = "unsupported"
	} else {
		extraction.Status = "partial"
	}
	return nil
}

func isPromptField(value string) bool {
	for _, key := range promptFieldNames {
		if value == key {
			return true
		}
	}
	return false
}
