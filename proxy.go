package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ProxyServer struct {
	cfg       Config
	resolver  *IdentityResolver
	store     *Store
	queue     *RecordQueue
	reverse   *httputil.ReverseProxy
	websocket *WebSocketProxy
}

func NewProxyServer(cfg Config, resolver *IdentityResolver, store *Store, queue *RecordQueue) (*ProxyServer, error) {
	if cfg.UpstreamURL == nil || cfg.UpstreamURL.Host == "" {
		return nil, fmt.Errorf("upstream URL is not configured")
	}
	if queue == nil {
		return nil, fmt.Errorf("record queue is not configured")
	}
	target := *cfg.UpstreamURL
	reverse := httputil.NewSingleHostReverseProxy(&target)
	reverse.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("prompt-audit upstream request failed method=%s path=%s err=%v", r.Method, r.URL.Path, err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	server := &ProxyServer{
		cfg:      cfg,
		resolver: resolver,
		store:    store,
		queue:    queue,
		reverse:  reverse,
	}
	if cfg.EnableWebSocket {
		server.websocket = NewWebSocketProxy(cfg, resolver, queue)
	}
	return server, nil
}

func (p *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" || r.URL.Path == "/health" {
		p.handleHealth(w, r)
		return
	}
	if r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/") {
		p.handleAdmin(w, r)
		return
	}

	requestID := correlationID(r.Header.Get("X-Request-ID"))
	clientRequestID := correlationID(r.Header.Get("X-Client-Request-ID"))
	r.Header.Set("X-Request-ID", requestID)
	r.Header.Set("X-Client-Request-ID", clientRequestID)

	if p.websocket != nil && isWebSocketRequest(r) {
		p.websocket.ServeHTTP(w, r, requestID, clientRequestID)
		return
	}

	if p.shouldCapture(r) {
		receivedAt := time.Now().UTC()
		body, captured, err := readBodyForCapture(r, p.cfg.MaxCaptureBytes)
		if err != nil {
			log.Printf("prompt-audit request body capture skipped path=%s err=%v", r.URL.Path, err)
		} else if captured {
			p.captureHTTP(r, requestID, clientRequestID, receivedAt, body)
		}
	}
	p.reverse.ServeHTTP(w, r)
}

func (p *ProxyServer) shouldCapture(r *http.Request) bool {
	if r == nil || (r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch) {
		return false
	}
	for _, prefix := range p.cfg.CapturePrefixes {
		prefix = strings.TrimRight(prefix, "/")
		if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
			return true
		}
	}
	return false
}

func (p *ProxyServer) captureHTTP(r *http.Request, requestID, clientRequestID string, receivedAt time.Time, body []byte) {
	contentType := r.Header.Get("Content-Type")
	extraction := ExtractRequest(r.URL.Path, contentType, body, p.cfg.MaxPromptBytes)
	if extraction.Model == "" {
		extraction.Model = modelFromPath(r.URL.Path)
	}
	if extraction.Status == "unsupported" && len(extraction.Messages) == 0 {
		return
	}
	identity := p.resolver.Lookup(extractAPIKey(r.Header))
	record := CaptureRecord{
		ID:              uuid.NewString(),
		ReceivedAt:      receivedAt,
		RequestID:       requestID,
		ClientRequestID: clientRequestID,
		Identity:        identity,
		Endpoint:        r.URL.Path,
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

func readBodyForCapture(r *http.Request, maxBytes int64) ([]byte, bool, error) {
	if r == nil || r.Body == nil || maxBytes <= 0 {
		return nil, false, nil
	}
	contentEncoding := strings.TrimSpace(r.Header.Get("Content-Encoding"))
	if contentEncoding != "" && !strings.EqualFold(contentEncoding, "identity") {
		return nil, false, nil
	}
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if contentType != "" &&
		!strings.Contains(contentType, "json") &&
		!strings.Contains(contentType, "form-urlencoded") &&
		!strings.Contains(contentType, "multipart/form-data") {
		return nil, false, nil
	}
	original := r.Body
	prefix, err := io.ReadAll(io.LimitReader(original, maxBytes+1))
	if err != nil {
		r.Body = &preservedBody{Reader: io.MultiReader(bytes.NewReader(prefix), original), closer: original}
		return nil, false, err
	}
	if int64(len(prefix)) > maxBytes {
		r.Body = &preservedBody{Reader: io.MultiReader(bytes.NewReader(prefix), original), closer: original}
		return nil, false, nil
	}
	_ = original.Close()
	r.Body = io.NopCloser(bytes.NewReader(prefix))
	return prefix, true, nil
}

type preservedBody struct {
	io.Reader
	closer io.Closer
}

func (b *preservedBody) Close() error {
	if b.closer == nil {
		return nil
	}
	return b.closer.Close()
}

func correlationID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return uuid.NewString()
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("._:-", r) {
			continue
		}
		return uuid.NewString()
	}
	return value
}

func isWebSocketRequest(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func upstreamWebSocketURL(base *url.URL, request *http.Request) string {
	target := *base
	if target.Scheme == "http" {
		target.Scheme = "ws"
	} else if target.Scheme == "https" {
		target.Scheme = "wss"
	}
	basePath := strings.TrimRight(target.Path, "/")
	requestPath := request.URL.Path
	if !strings.HasPrefix(requestPath, "/") {
		requestPath = "/" + requestPath
	}
	target.Path = basePath + requestPath
	target.RawPath = ""
	target.RawQuery = request.URL.RawQuery
	return target.String()
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, message)
}
