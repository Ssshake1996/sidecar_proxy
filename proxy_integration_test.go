package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProxyForwardsOriginalBodyAndQueuesUserOnlyCopy(t *testing.T) {
	var forwardedBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		forwardedBody = string(body)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("upstream response must not be captured"))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	secret := "test-secret"
	userID := int64(42)
	resolver := &IdentityResolver{
		secret:  []byte(secret),
		entries: map[string]Identity{},
	}
	resolver.entries[hmacFingerprint([]byte(secret), "sk-test")] = Identity{
		UserID:         &userID,
		Email:          "user@example.com",
		IdentitySource: "api_key_map",
	}
	queue := NewRecordQueue(4)
	cfg := Config{
		UpstreamURL:     target,
		CapturePrefixes: []string{"/v1"},
		MaxCaptureBytes: 4096,
		MaxPromptBytes:  4096,
		EnableWebSocket: false,
	}
	proxy, err := NewProxyServer(cfg, resolver, nil, queue)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()

	body := `{"model":"gpt-6","messages":[{"role":"system","content":"internal"},{"role":"user","content":"keep this"}]}`
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected upstream status: %d", resp.StatusCode)
	}
	if forwardedBody != body {
		t.Fatalf("proxy changed request body: got %q want %q", forwardedBody, body)
	}
	select {
	case record := <-queue.items:
		if record.PromptText != "keep this" || record.Identity.UserID == nil || *record.Identity.UserID != 42 {
			t.Fatalf("unexpected queued record: %#v", record)
		}
		if record.ExcludedRoles["system"] != 1 {
			t.Fatalf("system role was not excluded: %#v", record.ExcludedRoles)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not enqueue audit record")
	}
}
