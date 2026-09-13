package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web/admin.html
var adminHTML []byte

func (p *ProxyServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := contextWithTimeout(r, time.Second)
	defer cancel()
	status := http.StatusOK
	var dropped uint64
	if p.queue != nil {
		dropped = p.queue.Dropped()
	}
	result := map[string]any{
		"status":        "ok",
		"queue_dropped": dropped,
	}
	if err := p.store.Ping(ctx); err != nil {
		status = http.StatusServiceUnavailable
		result["status"] = "degraded"
		result["database"] = "unavailable"
	}
	writeJSON(w, status, result)
}

func (p *ProxyServer) handleAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "/admin/ui" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(adminHTML)
		return
	}
	if p.cfg.AdminToken == "" {
		writeJSONError(w, http.StatusNotFound, "admin API is disabled")
		return
	}
	if !p.authorizeAdmin(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="prompt-audit"`)
		writeJSONError(w, http.StatusUnauthorized, "admin authentication required")
		return
	}

	if path == "/admin" {
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "prompt-audit-sidecar",
			"endpoints": []string{
				"GET /admin/ui",
				"GET /admin/records",
				"GET /admin/records/{id}",
				"PATCH /admin/records/{id}",
			},
		})
		return
	}
	if path == "/admin/records" && r.Method == http.MethodGet {
		p.listRecords(w, r)
		return
	}
	prefix := "/admin/records/"
	if strings.HasPrefix(path, prefix) {
		id := strings.TrimPrefix(path, prefix)
		if id == "" || strings.Contains(id, "/") {
			writeJSONError(w, http.StatusNotFound, "record not found")
			return
		}
		switch r.Method {
		case http.MethodGet:
			p.getRecord(w, r, id)
		case http.MethodPatch:
			p.updateRecord(w, r, id)
		default:
			w.Header().Set("Allow", "GET, PATCH")
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	writeJSONError(w, http.StatusNotFound, "not found")
}

func (p *ProxyServer) authorizeAdmin(r *http.Request) bool {
	token := strings.TrimSpace(r.Header.Get("X-Prompt-Audit-Admin-Token"))
	if token == "" {
		authorization := strings.TrimSpace(r.Header.Get("Authorization"))
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			token = strings.TrimSpace(parts[1])
		}
	}
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(p.cfg.AdminToken)) == 1
}

func (p *ProxyServer) listRecords(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := RecordFilter{
		RequestID: strings.TrimSpace(query.Get("request_id")),
		Query:     strings.TrimSpace(query.Get("q")),
		Limit:     parsePositiveInt(query.Get("limit"), 50),
		Offset:    parseNonNegativeInt(query.Get("offset"), 0),
	}
	if value := strings.TrimSpace(query.Get("user_id")); value != "" {
		userID, err := strconv.ParseInt(value, 10, 64)
		if err != nil || userID <= 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid user_id")
			return
		}
		filter.UserID = &userID
	}
	var err error
	if filter.From, err = parseTimeQuery(query.Get("from")); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid from timestamp")
		return
	}
	if filter.To, err = parseTimeQuery(query.Get("to")); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid to timestamp")
		return
	}
	result, err := p.store.List(r.Context(), filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to list records")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (p *ProxyServer) getRecord(w http.ResponseWriter, r *http.Request, id string) {
	result, err := p.store.Get(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "record not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (p *ProxyServer) updateRecord(w http.ResponseWriter, r *http.Request, id string) {
	var update ReviewUpdate
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&update); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid review payload")
		return
	}
	if update.Reviewer == "" {
		update.Reviewer = strings.TrimSpace(r.Header.Get("X-Prompt-Audit-Reviewer"))
	}
	result, err := p.store.UpdateReview(r.Context(), id, update)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "record not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func parsePositiveInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func parseNonNegativeInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func parseTimeQuery(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, err
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}
