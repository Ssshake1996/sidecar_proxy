package main

import (
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type WebSocketProxy struct {
	cfg      Config
	resolver *IdentityResolver
	queue    *RecordQueue
	dialer   websocket.Dialer
	upgrader websocket.Upgrader
}

func NewWebSocketProxy(cfg Config, resolver *IdentityResolver, queue *RecordQueue) *WebSocketProxy {
	return &WebSocketProxy{
		cfg:      cfg,
		resolver: resolver,
		queue:    queue,
		dialer: websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
		},
		upgrader: websocket.Upgrader{
			ReadBufferSize:  32 * 1024,
			WriteBufferSize: 32 * 1024,
			CheckOrigin: func(_ *http.Request) bool {
				// API clients are not browser origins. Authentication is handled
				// by sub2api and the sidecar does not weaken that check.
				return true
			},
		},
	}
}

func (p *WebSocketProxy) ServeHTTP(w http.ResponseWriter, r *http.Request, requestID, clientRequestID string) {
	upstreamHeaders := cloneWebSocketHeaders(r.Header)
	upstreamHeaders.Set("X-Request-ID", requestID)
	upstreamHeaders.Set("X-Client-Request-ID", clientRequestID)
	upstreamURL := upstreamWebSocketURL(p.cfg.UpstreamURL, r)
	upstream, response, err := p.dialer.DialContext(r.Context(), upstreamURL, upstreamHeaders)
	if err != nil {
		if response != nil && response.Body != nil {
			defer response.Body.Close()
			for key, values := range response.Header {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(response.StatusCode)
			_, _ = io.Copy(w, response.Body)
			return
		}
		log.Printf("prompt-audit websocket upstream failed path=%s err=%v", r.URL.Path, err)
		writeJSONError(w, http.StatusBadGateway, "upstream websocket unavailable")
		return
	}
	defer upstream.Close()
	if protocol := upstream.Subprotocol(); protocol != "" {
		w.Header().Set("Sec-WebSocket-Protocol", protocol)
	}
	client, err := p.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()

	identity := p.resolver.Lookup(extractAPIKey(r.Header))
	errCh := make(chan error, 2)
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	go func() {
		errCh <- p.copyClientFrames(client, upstream, r.URL.Path, requestID, clientRequestID, identity)
	}()
	go func() {
		errCh <- copyServerFrames(upstream, client)
	}()
	<-errCh
	closeBoth()
}

func (p *WebSocketProxy) copyClientFrames(client, upstream *websocket.Conn, path, requestID, clientRequestID string, identity Identity) error {
	for {
		messageType, payload, err := client.ReadMessage()
		if err != nil {
			return err
		}
		if messageType == websocket.TextMessage {
			extraction := ExtractWebSocketMessage(path, payload, p.cfg.MaxPromptBytes)
			if extraction.Model == "" {
				extraction.Model = modelFromPath(path)
			}
			if extraction.Status != "unsupported" || len(extraction.Messages) > 0 {
				if extraction.Status != "unsupported" || p.cfg.CaptureEmptyPrompts {
					record := CaptureRecord{
						ID:              newRecordID(),
						ReceivedAt:      time.Now().UTC(),
						RequestID:       requestID,
						ClientRequestID: clientRequestID,
						Identity:        identity,
						Endpoint:        path,
						Protocol:        extraction.Protocol,
						Model:           extraction.Model,
						Messages:        extraction.Messages,
						PromptText:      extraction.PromptText,
						PromptSHA256:    extraction.PromptSHA256,
						MessageCount:    len(extraction.Messages),
						ParseStatus:     extraction.Status,
						Truncated:       extraction.Truncated,
						ExcludedRoles:   extraction.ExcludedRoles,
					}
					p.queue.Enqueue(record)
				}
			}
		}
		if err := upstream.WriteMessage(messageType, payload); err != nil {
			return err
		}
	}
}

func copyServerFrames(upstream, client *websocket.Conn) error {
	for {
		messageType, payload, err := upstream.ReadMessage()
		if err != nil {
			return err
		}
		if err := client.WriteMessage(messageType, payload); err != nil {
			return err
		}
	}
}

func cloneWebSocketHeaders(source http.Header) http.Header {
	result := make(http.Header)
	for key, values := range source {
		lower := strings.ToLower(key)
		switch lower {
		case "connection", "upgrade", "sec-websocket-key", "sec-websocket-accept", "sec-websocket-version", "sec-websocket-extensions":
			continue
		default:
			for _, value := range values {
				result.Add(key, value)
			}
		}
	}
	return result
}

func newRecordID() string {
	return uuid.NewString()
}
