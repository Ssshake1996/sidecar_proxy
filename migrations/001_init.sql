CREATE TABLE IF NOT EXISTS manual_prompt_audit_records (
    id                    UUID PRIMARY KEY,
    received_at           TIMESTAMPTZ NOT NULL,
    request_id            VARCHAR(128) NOT NULL,
    client_request_id     VARCHAR(128) NOT NULL DEFAULT '',
    user_id               BIGINT,
    username_snapshot     VARCHAR(255) NOT NULL DEFAULT '',
    email_snapshot        VARCHAR(255) NOT NULL DEFAULT '',
    api_key_id            BIGINT,
    api_key_name_snapshot VARCHAR(255) NOT NULL DEFAULT '',
    identity_source       VARCHAR(32) NOT NULL DEFAULT 'unknown',
    endpoint              TEXT NOT NULL DEFAULT '',
    protocol              VARCHAR(64) NOT NULL DEFAULT '',
    requested_model       VARCHAR(255) NOT NULL DEFAULT '',
    user_messages_json    JSONB NOT NULL DEFAULT '[]'::jsonb,
    prompt_text           TEXT NOT NULL DEFAULT '',
    prompt_sha256         CHAR(64) NOT NULL DEFAULT '',
    message_count         INTEGER NOT NULL DEFAULT 0,
    parse_status          VARCHAR(32) NOT NULL DEFAULT 'unknown',
    truncated             BOOLEAN NOT NULL DEFAULT FALSE,
    excluded_role_counts  JSONB NOT NULL DEFAULT '{}'::jsonb,
    review_status         VARCHAR(32) NOT NULL DEFAULT 'pending',
    reviewer              VARCHAR(255) NOT NULL DEFAULT '',
    review_note           TEXT NOT NULL DEFAULT '',
    reviewed_at           TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_manual_prompt_audit_received
    ON manual_prompt_audit_records (received_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_manual_prompt_audit_user_received
    ON manual_prompt_audit_records (user_id, received_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_manual_prompt_audit_request
    ON manual_prompt_audit_records (request_id);
CREATE INDEX IF NOT EXISTS idx_manual_prompt_audit_review
    ON manual_prompt_audit_records (review_status, received_at DESC);

CREATE TABLE IF NOT EXISTS manual_prompt_audit_admins (
    username       VARCHAR(64) PRIMARY KEY,
    password_hash  TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login_at  TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS manual_prompt_audit_sessions (
    token_hash     CHAR(64) PRIMARY KEY,
    username       VARCHAR(64) NOT NULL REFERENCES manual_prompt_audit_admins(username) ON DELETE CASCADE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at     TIMESTAMPTZ NOT NULL,
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_manual_prompt_audit_sessions_expiry
    ON manual_prompt_audit_sessions (expires_at);
