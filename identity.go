package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

// IdentityResolver keeps a local HMAC-keyed directory. The raw API key is
// read only while refreshing the directory and is never persisted or logged.
type IdentityResolver struct {
	db      *sql.DB
	secret  []byte
	refresh time.Duration

	mu      sync.RWMutex
	entries map[string]Identity
}

func NewIdentityResolver(ctx context.Context, cfg Config) (*IdentityResolver, error) {
	refreshInterval := cfg.IdentityRefresh
	if refreshInterval <= 0 {
		refreshInterval = 30 * time.Second
	}
	resolver := &IdentityResolver{
		secret:  []byte(cfg.IdentityHMACSecret),
		refresh: refreshInterval,
		entries: make(map[string]Identity),
	}
	if strings.TrimSpace(cfg.IdentityDatabaseURL) == "" {
		return resolver, nil
	}
	db, err := sql.Open("postgres", cfg.IdentityDatabaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	resolver.db = db
	refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := resolver.Refresh(refreshCtx); err != nil {
		// Identity lookup must never prevent the gateway from starting. The
		// periodic refresh will recover when the database becomes available.
		log.Printf("prompt-audit identity refresh skipped: %v", err)
	}
	return resolver, nil
}

func (r *IdentityResolver) Start(ctx context.Context) {
	if r == nil || r.db == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(r.refresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if err := r.Refresh(refreshCtx); err != nil {
					log.Printf("prompt-audit identity refresh failed: %v", err)
				}
				cancel()
			}
		}
	}()
}

func (r *IdentityResolver) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

func (r *IdentityResolver) Refresh(ctx context.Context) error {
	if r == nil || r.db == nil {
		return nil
	}
	query := `
		SELECT k.key, k.id, k.user_id, COALESCE(k.name, ''),
		       COALESCE(u.username, ''), COALESCE(u.email, '')
		FROM api_keys AS k
		LEFT JOIN users AS u ON u.id = k.user_id
		WHERE k.deleted_at IS NULL`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		// username was added after the initial sub2api schema. Keep the
		// resolver compatible with installations that have not migrated it.
		fallback := `
			SELECT k.key, k.id, k.user_id, COALESCE(k.name, ''), '', COALESCE(u.email, '')
			FROM api_keys AS k
			LEFT JOIN users AS u ON u.id = k.user_id
			WHERE k.deleted_at IS NULL`
		rows, err = r.db.QueryContext(ctx, fallback)
	}
	if err != nil {
		return err
	}
	defer rows.Close()

	next := make(map[string]Identity)
	for rows.Next() {
		var key, name, username, email string
		var keyID, userID int64
		if err := rows.Scan(&key, &keyID, &userID, &name, &username, &email); err != nil {
			return err
		}
		keyIDCopy, userIDCopy := keyID, userID
		next[hmacFingerprint(r.secret, key)] = Identity{
			UserID:         &userIDCopy,
			Username:       username,
			Email:          email,
			APIKeyID:       &keyIDCopy,
			APIKeyName:     name,
			IdentitySource: "api_key_map",
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.entries = next
	r.mu.Unlock()
	return nil
}

func (r *IdentityResolver) Lookup(apiKey string) Identity {
	if r == nil || strings.TrimSpace(apiKey) == "" {
		return Identity{IdentitySource: "unknown"}
	}
	fingerprint := hmacFingerprint(r.secret, apiKey)
	r.mu.RLock()
	identity, ok := r.entries[fingerprint]
	r.mu.RUnlock()
	if !ok {
		return Identity{IdentitySource: "unknown"}
	}
	return identity
}

// ResolveRequestID fills identity from a usage log when the request has
// already been billed/logged. It is a fallback for keys that were rotated
// between the request and the next directory refresh.
func (r *IdentityResolver) ResolveRequestID(ctx context.Context, requestID string) (Identity, bool) {
	if r == nil || r.db == nil || strings.TrimSpace(requestID) == "" {
		return Identity{}, false
	}
	const query = `
		SELECT l.user_id, COALESCE(u.username, ''), COALESCE(u.email, ''),
		       l.api_key_id
		FROM usage_logs AS l
		LEFT JOIN users AS u ON u.id = l.user_id
		WHERE l.request_id = $1
		ORDER BY l.created_at DESC
		LIMIT 1`
	var userID, keyID int64
	var username, email string
	if err := r.db.QueryRowContext(ctx, query, requestID).Scan(&userID, &username, &email, &keyID); err != nil {
		return Identity{}, false
	}
	return Identity{
		UserID:         &userID,
		Username:       username,
		Email:          email,
		APIKeyID:       &keyID,
		IdentitySource: "usage_log",
	}, true
}

func hmacFingerprint(secret []byte, value string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func extractAPIKey(headers http.Header) string {
	if authorization := strings.TrimSpace(headers.Get("Authorization")); authorization != "" {
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			if key := strings.TrimSpace(parts[1]); key != "" {
				return key
			}
		}
	}
	for _, header := range []string{"x-api-key", "x-goog-api-key"} {
		if value := strings.TrimSpace(headers.Get(header)); value != "" {
			return value
		}
	}
	return ""
}
