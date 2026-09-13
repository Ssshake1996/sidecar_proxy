package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketProxyCapturesClientFrameAndForwardsServerFrame(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","output":"must not be stored"}`))
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	secret := "ws-secret"
	userID := int64(7)
	resolver := &IdentityResolver{secret: []byte(secret), entries: map[string]Identity{}}
	resolver.entries[hmacFingerprint([]byte(secret), "sk-ws")] = Identity{
		UserID:         &userID,
		IdentitySource: "api_key_map",
	}
	queue := NewRecordQueue(4)
	proxy, err := NewProxyServer(Config{
		UpstreamURL:     upstreamURL,
		CapturePrefixes: []string{"/v1"},
		MaxPromptBytes:  4096,
		EnableWebSocket: true,
	}, resolver, nil, queue)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	wsURL := "ws" + server.URL[len("http"):] + "/v1/responses"
	clientHeaders := http.Header{"Authorization": []string{"Bearer sk-ws"}}
	client, _, err := websocket.DefaultDialer.Dial(wsURL, clientHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","input":"keep websocket user"}`)); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.TextMessage || string(payload) != `{"type":"response.completed","output":"must not be stored"}` {
		t.Fatalf("unexpected server frame: type=%d payload=%s", messageType, payload)
	}
	select {
	case record := <-queue.items:
		if record.PromptText != "keep websocket user" || record.Identity.UserID == nil || *record.Identity.UserID != 7 {
			t.Fatalf("unexpected websocket record: %#v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("websocket frame was not queued")
	}
}
