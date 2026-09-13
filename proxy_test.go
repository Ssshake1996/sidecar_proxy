package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestReadBodyForCaptureRestoresSmallBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	r.Header.Set("Content-Type", "application/json")
	body, captured, err := readBodyForCapture(r, 1024)
	if err != nil || !captured || string(body) != `{"messages":[]}` {
		t.Fatalf("capture failed body=%q captured=%v err=%v", body, captured, err)
	}
	restored, err := io.ReadAll(r.Body)
	if err != nil || string(restored) != string(body) {
		t.Fatalf("body was not restored: %q err=%v", restored, err)
	}
}

func TestReadBodyForCaptureRestoresOversizedBodyWithoutCapturing(t *testing.T) {
	original := strings.Repeat("x", 32)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(original))
	r.Header.Set("Content-Type", "application/json")
	body, captured, err := readBodyForCapture(r, 8)
	if err != nil || captured || body != nil {
		t.Fatalf("oversized body should not be captured: body=%q captured=%v err=%v", body, captured, err)
	}
	restored, err := io.ReadAll(r.Body)
	if err != nil || string(restored) != original {
		t.Fatalf("oversized body changed: %q err=%v", restored, err)
	}
}

func TestCorrelationIDRejectsHeaderInjection(t *testing.T) {
	got := correlationID("bad\nheader")
	if got == "bad\nheader" || got == "" || strings.ContainsAny(got, "\r\n") {
		t.Fatalf("unsafe correlation id: %q", got)
	}
}

func TestUpstreamWebSocketURL(t *testing.T) {
	base, _ := url.Parse("https://gateway.internal/base")
	r := httptest.NewRequest(http.MethodGet, "http://proxy/v1/responses?stream=true", nil)
	got := upstreamWebSocketURL(base, r)
	if got != "wss://gateway.internal/base/v1/responses?stream=true" {
		t.Fatalf("unexpected websocket target: %s", got)
	}
}

func TestExtractAPIKeyDoesNotUseQueryParameters(t *testing.T) {
	headers := make(http.Header)
	if got := extractAPIKey(headers); got != "" {
		t.Fatalf("unexpected key from empty headers: %q", got)
	}
	headers.Set("Authorization", "Bearer sk-test")
	if got := extractAPIKey(headers); got != "sk-test" {
		t.Fatalf("authorization key mismatch: %q", got)
	}
}

func TestModelFromPath(t *testing.T) {
	if got := modelFromPath("/v1beta/models/gemini-2.5-pro:generateContent"); got != "gemini-2.5-pro" {
		t.Fatalf("unexpected model from path: %q", got)
	}
}
