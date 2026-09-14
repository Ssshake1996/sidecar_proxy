package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed web/admin.html
var adminHTML []byte

//go:embed web/login.html
var loginHTML []byte

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
	if p.store == nil || p.store.Ping(ctx) != nil {
		status = http.StatusServiceUnavailable
		result["status"] = "degraded"
		result["database"] = "unavailable"
	}
	writeJSON(w, status, result)
}

func (p *ProxyServer) handleAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch path {
	case "/admin/login":
		switch r.Method {
		case http.MethodGet:
			p.serveLogin(w, r)
		case http.MethodPost:
			p.loginAdmin(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	case "/admin/logout":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		p.logoutAdmin(w, r)
		return
	case "/admin/ui":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !p.requireAdminPage(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(adminHTML)
		return
	case "/admin/session":
		username, ok, err := p.authenticateAdmin(r)
		if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "admin authentication unavailable")
			return
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="prompt-audit"`)
			writeJSONError(w, http.StatusUnauthorized, "admin login required")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": username})
		return
	case "/admin":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if _, ok, err := p.authenticateAdmin(r); err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "admin authentication unavailable")
			return
		} else if !ok {
			redirectToLogin(w, r, "/admin/ui")
			return
		}
		http.Redirect(w, r, "/admin/ui", http.StatusFound)
		return
	}

	if !p.requireAdminAPI(w, r) {
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

func (p *ProxyServer) serveLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok, err := p.authenticateAdmin(r); err == nil && ok {
		http.Redirect(w, r, "/admin/ui", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(loginHTML)
}

func (p *ProxyServer) loginAdmin(w http.ResponseWriter, r *http.Request) {
	if p.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "admin authentication unavailable")
		return
	}
	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credentials); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid login payload")
		return
	}
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	ok, err := p.store.AuthenticateAdmin(ctx, credentials.Username, credentials.Password)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "admin authentication unavailable")
		return
	}
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	token, expiresAt, err := p.store.CreateAdminSession(ctx, credentials.Username, p.cfg.AdminSessionTTL)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "could not create admin session")
		return
	}
	setAdminSessionCookie(w, r, token, expiresAt)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"username":      strings.TrimSpace(credentials.Username),
		"expires_at":    expiresAt,
	})
}

func (p *ProxyServer) logoutAdmin(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(adminSessionCookieName); err == nil && p.store != nil {
		ctx, cancel := contextWithTimeout(r, 2*time.Second)
		_ = p.store.DeleteAdminSession(ctx, cookie.Value)
		cancel()
	}
	clearAdminSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
}

func (p *ProxyServer) requireAdminPage(w http.ResponseWriter, r *http.Request) bool {
	_, ok, err := p.authenticateAdmin(r)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "admin authentication unavailable")
		return false
	}
	if !ok {
		redirectToLogin(w, r, r.URL.RequestURI())
		return false
	}
	return true
}

func (p *ProxyServer) requireAdminAPI(w http.ResponseWriter, r *http.Request) bool {
	_, ok, err := p.authenticateAdmin(r)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "admin authentication unavailable")
		return false
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="prompt-audit"`)
		writeJSONError(w, http.StatusUnauthorized, "admin login required")
		return false
	}
	return true
}

func (p *ProxyServer) authenticateAdmin(r *http.Request) (string, bool, error) {
	if p.cfg.AdminToken != "" {
		if token := bearerToken(r.Header); token != "" &&
			subtle.ConstantTimeCompare([]byte(token), []byte(p.cfg.AdminToken)) == 1 {
			return "token", true, nil
		}
	}
	if p.store == nil {
		return "", false, nil
	}
	cookie, err := r.Cookie(adminSessionCookieName)
	if err != nil {
		return "", false, nil
	}
	ctx, cancel := contextWithTimeout(r, 2*time.Second)
	defer cancel()
	return p.store.ValidateAdminSession(ctx, cookie.Value)
}

func bearerToken(headers http.Header) string {
	authorization := strings.TrimSpace(headers.Get("Authorization"))
	parts := strings.SplitN(authorization, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return strings.TrimSpace(headers.Get("X-Prompt-Audit-Admin-Token"))
}

func setAdminSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    token,
		Path:     "/admin",
		Expires:  expiresAt,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func clearAdminSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func isSecureRequest(r *http.Request) bool {
	if r != nil && r.TLS != nil {
		return true
	}
	if r != nil && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return true
	}
	return false
}

func redirectToLogin(w http.ResponseWriter, r *http.Request, next string) {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/admin/ui"
	}
	location := "/admin/login?next=" + url.QueryEscape(next)
	http.Redirect(w, r, location, http.StatusFound)
}

func (p *ProxyServer) listRecords(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := RecordFilter{
		RequestID:    strings.TrimSpace(query.Get("request_id")),
		Query:        strings.TrimSpace(query.Get("q")),
		ReviewStatus: strings.TrimSpace(query.Get("review_status")),
		Limit:        parsePositiveInt(query.Get("limit"), 25),
		Offset:       parseNonNegativeInt(query.Get("offset"), 0),
	}
	if filter.Limit > 200 {
		filter.Limit = 200
	}
	if len([]byte(filter.Query)) > 256 {
		writeJSONError(w, http.StatusBadRequest, "q is too long")
		return
	}
	if filter.ReviewStatus != "" {
		filter.ReviewStatus = strings.ToLower(filter.ReviewStatus)
		switch filter.ReviewStatus {
		case "pending", "approved", "flagged", "ignored":
		default:
			writeJSONError(w, http.StatusBadRequest, "invalid review_status")
			return
		}
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
	w.Header().Set("Cache-Control", "no-store")
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
