package main

import (
	"strings"
	"testing"
)

func TestExtractOpenAIChatKeepsOnlyUserMessagesInOrder(t *testing.T) {
	body := []byte(`{
		"model":"gpt-6",
		"messages":[
			{"role":"system","content":"hidden system"},
			{"role":"user","content":"first"},
			{"role":"assistant","content":"hidden answer"},
			{"role":"developer","content":"hidden developer"},
			{"role":"user","content":[{"type":"text","text":"second"},{"type":"image_url","image_url":{"url":"secret"}}]},
			{"role":"tool","content":"hidden tool"}
		]
	}`)
	got := ExtractRequest("/v1/chat/completions", "application/json", body, 1024)
	if got.Protocol != "openai-chat" || got.Model != "gpt-6" {
		t.Fatalf("unexpected protocol/model: %#v", got)
	}
	if got.PromptText != "first\n\nsecond" {
		t.Fatalf("prompt order/content mismatch: %q", got.PromptText)
	}
	if len(got.Messages) != 2 || got.Messages[0].Text != "first" || got.Messages[1].Text != "second" {
		t.Fatalf("messages mismatch: %#v", got.Messages)
	}
	for _, role := range []string{"system", "assistant", "developer", "tool"} {
		if got.ExcludedRoles[role] != 1 {
			t.Fatalf("excluded role %q missing: %#v", role, got.ExcludedRoles)
		}
	}
}

func TestExtractResponsesExcludesInstructions(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"instructions":"do not persist this",
		"input":[
			{"role":"developer","content":"also hidden"},
			{"role":"user","content":[{"type":"input_text","text":"keep me"}]}
		]
	}`)
	got := ExtractRequest("/v1/responses", "application/json", body, 1024)
	if got.PromptText != "keep me" || got.ExcludedRoles["system"] != 1 || got.ExcludedRoles["developer"] != 1 {
		t.Fatalf("responses extraction mismatch: %#v", got)
	}
}

func TestExtractAnthropicAndGeminiSystemFields(t *testing.T) {
	anthropic := ExtractRequest("/v1/messages", "application/json", []byte(`{
		"system":"private",
		"messages":[{"role":"user","content":[{"type":"text","text":"anthropic user"}]}]
	}`), 1024)
	if anthropic.PromptText != "anthropic user" || anthropic.ExcludedRoles["system"] != 1 {
		t.Fatalf("anthropic extraction mismatch: %#v", anthropic)
	}
	gemini := ExtractRequest("/v1beta/models/gemini:generateContent", "application/json", []byte(`{
		"systemInstruction":{"parts":[{"text":"private"}]},
		"contents":[{"role":"model","parts":[{"text":"hidden"}]},{"role":"user","parts":[{"text":"gemini user"}]}]
	}`), 1024)
	if gemini.PromptText != "gemini user" || gemini.ExcludedRoles["system"] != 1 || gemini.ExcludedRoles["assistant"] != 1 {
		t.Fatalf("gemini extraction mismatch: %#v", gemini)
	}
}

func TestExtractWebSocketResponseCreate(t *testing.T) {
	got := ExtractWebSocketMessage("/v1/responses", []byte(`{
		"type":"response.create",
		"instructions":"hidden",
		"input":"websocket user"
	}`), 1024)
	if got.Protocol != "openai-responses-ws" || got.PromptText != "websocket user" {
		t.Fatalf("websocket extraction mismatch: %#v", got)
	}
}

func TestTruncateUTF8AndHash(t *testing.T) {
	got := ExtractRequest("/v1/chat/completions", "application/json", []byte(`{"messages":[{"role":"user","content":"你好世界"}]} `), 7)
	if !got.Truncated || got.PromptText != "你好" {
		t.Fatalf("unexpected UTF-8 truncation: %q truncated=%v", got.PromptText, got.Truncated)
	}
	if got.PromptSHA256 == "" || strings.TrimSpace(got.PromptSHA256) == "" {
		t.Fatal("prompt hash was not populated")
	}
}

func TestUnknownProtocolDoesNotPersistRawJSON(t *testing.T) {
	got := ExtractRequest("/v1/unknown", "application/json", []byte(`{"context":"system secret","metadata":{"value":"private"}}`), 1024)
	if got.Status != "unsupported" || got.PromptText != "" || len(got.Messages) != 0 {
		t.Fatalf("unknown protocol leaked content: %#v", got)
	}
}
