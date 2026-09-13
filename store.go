package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

//go:embed migrations/001_init.sql
var migrationFS embed.FS

type Store struct {
	db            *sql.DB
	schema        []byte
	schemaMu      sync.Mutex
	schemaReady   bool
	nextSchemaTry time.Time
	lastSchemaErr error
}

func OpenStore(ctx context.Context, databaseURL string) (*Store, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	schema, err := migrationFS.ReadFile("migrations/001_init.sql")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &Store{db: db, schema: schema}
	initCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := store.ensureSchema(initCtx); err != nil {
		// The proxy is deliberately fail-open. A database outage at startup
		// must not make the traffic path unavailable; writes retry lazily.
		log.Printf("prompt-audit store unavailable at startup: %v", err)
	}
	cancel()
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store is not initialized")
	}
	return s.ensureSchema(ctx)
}

func (s *Store) Insert(ctx context.Context, record CaptureRecord) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	messages, err := json.Marshal(record.Messages)
	if err != nil {
		return err
	}
	excluded, err := json.Marshal(record.ExcludedRoles)
	if err != nil {
		return err
	}
	const query = `
		INSERT INTO manual_prompt_audit_records (
			id, received_at, request_id, client_request_id,
			user_id, username_snapshot, email_snapshot, api_key_id, api_key_name_snapshot,
			identity_source, endpoint, protocol, requested_model, user_messages_json,
			prompt_text, prompt_sha256, message_count, parse_status, truncated,
			excluded_role_counts
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14,
			$15, $16, $17, $18, $19,
			$20
		)`
	_, err = s.db.ExecContext(ctx, query,
		record.ID,
		record.ReceivedAt,
		record.RequestID,
		record.ClientRequestID,
		nullInt64(record.Identity.UserID),
		record.Identity.Username,
		record.Identity.Email,
		nullInt64(record.Identity.APIKeyID),
		record.Identity.APIKeyName,
		record.Identity.IdentitySource,
		record.Endpoint,
		record.Protocol,
		record.Model,
		messages,
		record.PromptText,
		record.PromptSHA256,
		record.MessageCount,
		record.ParseStatus,
		record.Truncated,
		excluded,
	)
	return err
}

func (s *Store) DeleteExpired(ctx context.Context, retentionDays int) error {
	if retentionDays <= 0 {
		return nil
	}
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM manual_prompt_audit_records WHERE received_at < NOW() - ($1 * INTERVAL '1 day')`,
		retentionDays,
	)
	return err
}

type RecordFilter struct {
	UserID    *int64
	RequestID string
	Query     string
	From      *time.Time
	To        *time.Time
	Limit     int
	Offset    int
}

type RecordSummary struct {
	ID                 string         `json:"id"`
	ReceivedAt         time.Time      `json:"received_at"`
	RequestID          string         `json:"request_id"`
	ClientRequestID    string         `json:"client_request_id,omitempty"`
	UserID             *int64         `json:"user_id,omitempty"`
	Username           string         `json:"username,omitempty"`
	Email              string         `json:"email,omitempty"`
	APIKeyID           *int64         `json:"api_key_id,omitempty"`
	APIKeyName         string         `json:"api_key_name,omitempty"`
	IdentitySource     string         `json:"identity_source"`
	Endpoint           string         `json:"endpoint"`
	Protocol           string         `json:"protocol"`
	Model              string         `json:"model"`
	Messages           []UserMessage  `json:"user_messages"`
	PromptText         string         `json:"prompt_text"`
	PromptSHA256       string         `json:"prompt_sha256"`
	MessageCount       int            `json:"message_count"`
	ParseStatus        string         `json:"parse_status"`
	Truncated          bool           `json:"truncated"`
	ExcludedRoleCounts map[string]int `json:"excluded_role_counts"`
	ReviewStatus       string         `json:"review_status"`
	Reviewer           string         `json:"reviewer,omitempty"`
	ReviewNote         string         `json:"review_note,omitempty"`
	ReviewedAt         *time.Time     `json:"reviewed_at,omitempty"`
}

type ListResult struct {
	Items  []RecordSummary `json:"items"`
	Total  int             `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

func (s *Store) List(ctx context.Context, filter RecordFilter) (ListResult, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return ListResult{}, err
	}
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	where := []string{"1=1"}
	args := make([]any, 0, 8)
	add := func(condition string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(condition, len(args)))
	}
	if filter.UserID != nil {
		add("user_id = $%d", *filter.UserID)
	}
	if filter.RequestID != "" {
		add("request_id = $%d", filter.RequestID)
	}
	if filter.Query != "" {
		placeholder := len(args) + 1
		args = append(args, "%"+filter.Query+"%")
		where = append(where, fmt.Sprintf(
			"(prompt_text ILIKE $%d OR email_snapshot ILIKE $%d OR username_snapshot ILIKE $%d)",
			placeholder, placeholder, placeholder,
		))
	}
	if filter.From != nil {
		add("received_at >= $%d", *filter.From)
	}
	if filter.To != nil {
		add("received_at < $%d", *filter.To)
	}
	whereSQL := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM manual_prompt_audit_records WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return ListResult{}, err
	}
	query := `SELECT id, received_at, request_id, client_request_id,
		user_id, username_snapshot, email_snapshot, api_key_id, api_key_name_snapshot,
		identity_source, endpoint, protocol, requested_model, user_messages_json, prompt_text, prompt_sha256,
		message_count, parse_status, truncated, excluded_role_counts,
		review_status, reviewer, review_note, reviewed_at
		FROM manual_prompt_audit_records WHERE ` + whereSQL +
		` ORDER BY received_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args)+1) +
		` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, filter.Limit, filter.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ListResult{}, err
	}
	defer rows.Close()
	items := make([]RecordSummary, 0, filter.Limit)
	for rows.Next() {
		item, err := scanRecordSummary(rows)
		if err != nil {
			return ListResult{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, err
	}
	return ListResult{Items: items, Total: total, Limit: filter.Limit, Offset: filter.Offset}, nil
}

func (s *Store) Get(ctx context.Context, id string) (RecordSummary, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return RecordSummary{}, err
	}
	parsed, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return RecordSummary{}, fmt.Errorf("invalid record id")
	}
	const query = `SELECT id, received_at, request_id, client_request_id,
		user_id, username_snapshot, email_snapshot, api_key_id, api_key_name_snapshot,
		identity_source, endpoint, protocol, requested_model, user_messages_json, prompt_text, prompt_sha256,
		message_count, parse_status, truncated, excluded_role_counts,
		review_status, reviewer, review_note, reviewed_at
		FROM manual_prompt_audit_records WHERE id = $1`
	row := s.db.QueryRowContext(ctx, query, parsed)
	return scanRecordSummary(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecordSummary(row rowScanner) (RecordSummary, error) {
	var item RecordSummary
	var id uuid.UUID
	var userID, keyID sql.NullInt64
	var username, email, keyName, identitySource string
	var messages, excluded []byte
	if err := row.Scan(
		&id, &item.ReceivedAt, &item.RequestID, &item.ClientRequestID,
		&userID, &username, &email, &keyID, &keyName,
		&identitySource, &item.Endpoint, &item.Protocol, &item.Model, &messages, &item.PromptText, &item.PromptSHA256,
		&item.MessageCount, &item.ParseStatus, &item.Truncated, &excluded,
		&item.ReviewStatus, &item.Reviewer, &item.ReviewNote, &item.ReviewedAt,
	); err != nil {
		return RecordSummary{}, err
	}
	item.ID = id.String()
	item.UserID = nullableInt64(userID)
	item.APIKeyID = nullableInt64(keyID)
	item.Username = username
	item.Email = email
	item.APIKeyName = keyName
	item.IdentitySource = identitySource
	if len(messages) > 0 {
		if err := json.Unmarshal(messages, &item.Messages); err != nil {
			return RecordSummary{}, err
		}
	}
	if len(excluded) > 0 {
		if err := json.Unmarshal(excluded, &item.ExcludedRoleCounts); err != nil {
			return RecordSummary{}, err
		}
	}
	if item.ExcludedRoleCounts == nil {
		item.ExcludedRoleCounts = map[string]int{}
	}
	return item, nil
}

type ReviewUpdate struct {
	Status   string `json:"review_status"`
	Reviewer string `json:"reviewer"`
	Note     string `json:"review_note"`
}

func (s *Store) UpdateReview(ctx context.Context, id string, update ReviewUpdate) (RecordSummary, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return RecordSummary{}, err
	}
	parsed, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return RecordSummary{}, fmt.Errorf("invalid record id")
	}
	status := strings.ToLower(strings.TrimSpace(update.Status))
	switch status {
	case "pending", "approved", "flagged", "ignored":
	default:
		return RecordSummary{}, fmt.Errorf("review_status must be pending, approved, flagged, or ignored")
	}
	reviewer := strings.TrimSpace(update.Reviewer)
	var reviewedAt any
	if status == "pending" {
		reviewedAt = nil
	} else {
		reviewedAt = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `UPDATE manual_prompt_audit_records
		SET review_status = $1, reviewer = $2, review_note = $3, reviewed_at = $4
		WHERE id = $5`, status, reviewer, update.Note, reviewedAt, parsed)
	if err != nil {
		return RecordSummary{}, err
	}
	return s.Get(ctx, parsed.String())
}

func (s *Store) ensureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store is not initialized")
	}
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if s.schemaReady {
		return nil
	}
	if !s.nextSchemaTry.IsZero() && time.Now().Before(s.nextSchemaTry) && s.lastSchemaErr != nil {
		return s.lastSchemaErr
	}
	if err := s.db.PingContext(ctx); err != nil {
		s.lastSchemaErr = err
		s.nextSchemaTry = time.Now().Add(5 * time.Second)
		return err
	}
	if _, err := s.db.ExecContext(ctx, string(s.schema)); err != nil {
		s.lastSchemaErr = fmt.Errorf("initialize prompt audit schema: %w", err)
		s.nextSchemaTry = time.Now().Add(5 * time.Second)
		return s.lastSchemaErr
	}
	s.schemaReady = true
	s.lastSchemaErr = nil
	s.nextSchemaTry = time.Time{}
	return nil
}

func nullInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}
