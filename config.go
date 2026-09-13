package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config contains only sidecar settings. It deliberately does not reuse the
// sub2api application config so the proxy remains independently deployable.
type Config struct {
	ListenAddr           string
	UpstreamURL          *url.URL
	AuditDatabaseURL     string
	IdentityDatabaseURL  string
	IdentityHMACSecret   string
	IdentityRefresh      time.Duration
	QueueSize            int
	MaxCaptureBytes      int64
	MaxPromptBytes       int
	AdminToken           string
	AllowUnknownIdentity bool
	CaptureEmptyPrompts  bool
	CapturePrefixes      []string
	AuditRetentionDays   int
	EnableWebSocket      bool
	ShutdownTimeout      time.Duration
}

func LoadConfig() (Config, error) {
	c := Config{
		ListenAddr:           envString("PROMPT_AUDIT_LISTEN_ADDR", ":8090"),
		AuditDatabaseURL:     strings.TrimSpace(os.Getenv("PROMPT_AUDIT_DATABASE_URL")),
		IdentityDatabaseURL:  strings.TrimSpace(os.Getenv("PROMPT_AUDIT_IDENTITY_DATABASE_URL")),
		IdentityHMACSecret:   strings.TrimSpace(os.Getenv("PROMPT_AUDIT_IDENTITY_HMAC_SECRET")),
		IdentityRefresh:      envDuration("PROMPT_AUDIT_IDENTITY_REFRESH", 30*time.Second),
		QueueSize:            envInt("PROMPT_AUDIT_QUEUE_SIZE", 2048),
		MaxCaptureBytes:      envInt64("PROMPT_AUDIT_MAX_CAPTURE_BYTES", 1<<20),
		MaxPromptBytes:       envInt("PROMPT_AUDIT_MAX_PROMPT_BYTES", 1<<20),
		AdminToken:           strings.TrimSpace(os.Getenv("PROMPT_AUDIT_ADMIN_TOKEN")),
		AllowUnknownIdentity: envBool("PROMPT_AUDIT_CAPTURE_UNKNOWN_IDENTITY", false),
		CaptureEmptyPrompts:  envBool("PROMPT_AUDIT_CAPTURE_EMPTY_PROMPTS", false),
		CapturePrefixes:      splitCSV(envString("PROMPT_AUDIT_CAPTURE_PREFIXES", "/v1,/v1beta")),
		AuditRetentionDays:   envInt("PROMPT_AUDIT_RETENTION_DAYS", 0),
		EnableWebSocket:      envBool("PROMPT_AUDIT_WEBSOCKET", true),
		ShutdownTimeout:      envDuration("PROMPT_AUDIT_SHUTDOWN_TIMEOUT", 10*time.Second),
	}

	if c.AuditDatabaseURL == "" {
		return Config{}, fmt.Errorf("PROMPT_AUDIT_DATABASE_URL is required")
	}
	if c.QueueSize < 1 {
		return Config{}, fmt.Errorf("PROMPT_AUDIT_QUEUE_SIZE must be positive")
	}
	if c.MaxCaptureBytes < 1024 {
		return Config{}, fmt.Errorf("PROMPT_AUDIT_MAX_CAPTURE_BYTES must be at least 1024")
	}
	if c.MaxPromptBytes < 256 {
		return Config{}, fmt.Errorf("PROMPT_AUDIT_MAX_PROMPT_BYTES must be at least 256")
	}
	if c.AuditRetentionDays < 0 {
		return Config{}, fmt.Errorf("PROMPT_AUDIT_RETENTION_DAYS cannot be negative")
	}
	if c.IdentityDatabaseURL != "" && c.IdentityHMACSecret == "" {
		return Config{}, fmt.Errorf("PROMPT_AUDIT_IDENTITY_HMAC_SECRET is required when identity database is configured")
	}

	upstreamRaw := strings.TrimSpace(os.Getenv("PROMPT_AUDIT_UPSTREAM_URL"))
	if upstreamRaw == "" {
		return Config{}, fmt.Errorf("PROMPT_AUDIT_UPSTREAM_URL is required")
	}
	upstream, err := url.Parse(upstreamRaw)
	if err != nil || upstream.Host == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") {
		return Config{}, fmt.Errorf("PROMPT_AUDIT_UPSTREAM_URL must be an absolute http(s) URL")
	}
	c.UpstreamURL = upstream
	if len(c.CapturePrefixes) == 0 {
		return Config{}, fmt.Errorf("PROMPT_AUDIT_CAPTURE_PREFIXES cannot be empty")
	}
	return c, nil
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || value == 0 {
		return fallback
	}
	return value
}

func envInt64(key string, fallback int64) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64)
	if err != nil || value == 0 {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
