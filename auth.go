package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	adminSessionCookieName = "prompt_audit_session"
	adminPasswordCost      = 12
	defaultAdminSessionTTL = 12 * time.Hour
)

// generateAdminPassword creates a URL-safe password that can be copied from
// container logs without shell quoting or URL encoding.
func generateAdminPassword() (string, error) {
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate admin password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}

func generateSessionToken() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate admin session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}

func hashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func hashAdminPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("admin password is empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), adminPasswordCost)
	if err != nil {
		return "", fmt.Errorf("hash admin password: %w", err)
	}
	return string(hash), nil
}

// EnsureAdmin atomically creates the first configured administrator. Existing
// credentials are never replaced during restart or deployment.
func (s *Store) EnsureAdmin(ctx context.Context, username, password string) (bool, error) {
	hash, err := hashAdminPassword(password)
	if err != nil {
		return false, err
	}
	return s.EnsureAdminHash(ctx, username, hash)
}

func (s *Store) EnsureAdminHash(ctx context.Context, username, passwordHash string) (bool, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return false, err
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return false, fmt.Errorf("admin username is empty")
	}
	if passwordHash == "" {
		return false, fmt.Errorf("admin password hash is empty")
	}
	var inserted string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO manual_prompt_audit_admins (username, password_hash)
		VALUES ($1, $2)
		ON CONFLICT (username) DO NOTHING
		RETURNING username`, username, passwordHash).Scan(&inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return inserted != "", nil
}

func (s *Store) AuthenticateAdmin(ctx context.Context, username, password string) (bool, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return false, err
	}
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return false, nil
	}
	var passwordHash string
	err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM manual_prompt_audit_admins WHERE username = $1`, username,
	).Scan(&passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		return false, nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE manual_prompt_audit_admins SET last_login_at = NOW() WHERE username = $1`, username,
	); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) CreateAdminSession(ctx context.Context, username string, ttl time.Duration) (string, time.Time, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return "", time.Time{}, err
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return "", time.Time{}, fmt.Errorf("admin username is empty")
	}
	if ttl <= 0 {
		ttl = defaultAdminSessionTTL
	}
	for attempt := 0; attempt < 3; attempt++ {
		token, err := generateSessionToken()
		if err != nil {
			return "", time.Time{}, err
		}
		expiresAt := time.Now().UTC().Add(ttl)
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO manual_prompt_audit_sessions (token_hash, username, expires_at)
			VALUES ($1, $2, $3)`, hashSessionToken(token), username, expiresAt)
		if err == nil {
			return token, expiresAt, nil
		}
		if attempt == 2 {
			return "", time.Time{}, err
		}
	}
	return "", time.Time{}, fmt.Errorf("create admin session failed")
}

func (s *Store) ValidateAdminSession(ctx context.Context, token string) (string, bool, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return "", false, err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false, nil
	}
	var username string
	err := s.db.QueryRowContext(ctx, `
		SELECT username FROM manual_prompt_audit_sessions
		WHERE token_hash = $1 AND expires_at > NOW()`, hashSessionToken(token)).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	_, _ = s.db.ExecContext(ctx,
		`UPDATE manual_prompt_audit_sessions
		 SET last_seen_at = NOW()
		 WHERE token_hash = $1 AND last_seen_at < NOW() - INTERVAL '5 minutes'`, hashSessionToken(token),
	)
	return username, true, nil
}

func (s *Store) DeleteAdminSession(ctx context.Context, token string) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM manual_prompt_audit_sessions WHERE token_hash = $1`, hashSessionToken(token),
	)
	return err
}

func (s *Store) DeleteExpiredAdminSessions(ctx context.Context) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM manual_prompt_audit_sessions WHERE expires_at <= NOW()`,
	)
	return err
}
