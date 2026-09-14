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
	// ListenAddr is retained as a compatibility alias for HTTPListenAddr.
	ListenAddr           string
	HTTPListenAddr       string
	HTTPSListenAddr      string
	TLSCertFile          string
	TLSKeyFile           string
	UpstreamURL          *url.URL
	AuditDatabaseURL     string
	IdentityDatabaseURL  string
	IdentityHMACSecret   string
	IdentityRefresh      time.Duration
	QueueSize            int
	MaxCaptureBytes      int64
	MaxPromptBytes       int
	AdminToken           string
	AdminUsername        string
	AdminPassword        string
	AdminSessionTTL      time.Duration
	AllowUnknownIdentity bool
	CaptureEmptyPrompts  bool
	CapturePrefixes      []string
	AuditRetentionDays   int
	EnableWebSocket      bool
	ShutdownTimeout      time.Duration
}

func LoadConfig() (Config, error) {
	httpListenAddr := strings.TrimSpace(os.Getenv("PROMPT_AUDIT_HTTP_ADDR"))
	if httpListenAddr == "" {
		httpListenAddr = envString("PROMPT_AUDIT_LISTEN_ADDR", ":8090")
	}
	httpsListenAddr := strings.TrimSpace(os.Getenv("PROMPT_AUDIT_HTTPS_ADDR"))
	tlsCertFile := strings.TrimSpace(os.Getenv("PROMPT_AUDIT_TLS_CERT_FILE"))
	tlsKeyFile := strings.TrimSpace(os.Getenv("PROMPT_AUDIT_TLS_KEY_FILE"))
	adminPassword := os.Getenv("PROMPT_AUDIT_ADMIN_PASSWORD")
	c := Config{
		ListenAddr:           httpListenAddr,
		HTTPListenAddr:       httpListenAddr,
		HTTPSListenAddr:      httpsListenAddr,
		TLSCertFile:          tlsCertFile,
		TLSKeyFile:           tlsKeyFile,
		AuditDatabaseURL:     strings.TrimSpace(os.Getenv("PROMPT_AUDIT_DATABASE_URL")),
		IdentityDatabaseURL:  strings.TrimSpace(os.Getenv("PROMPT_AUDIT_IDENTITY_DATABASE_URL")),
		IdentityHMACSecret:   strings.TrimSpace(os.Getenv("PROMPT_AUDIT_IDENTITY_HMAC_SECRET")),
		IdentityRefresh:      envDuration("PROMPT_AUDIT_IDENTITY_REFRESH", 30*time.Second),
		QueueSize:            envInt("PROMPT_AUDIT_QUEUE_SIZE", 2048),
		MaxCaptureBytes:      envInt64("PROMPT_AUDIT_MAX_CAPTURE_BYTES", 1<<20),
		MaxPromptBytes:       envInt("PROMPT_AUDIT_MAX_PROMPT_BYTES", 1<<20),
		AdminToken:           strings.TrimSpace(os.Getenv("PROMPT_AUDIT_ADMIN_TOKEN")),
		AdminUsername:        envString("PROMPT_AUDIT_ADMIN_USERNAME", "admin"),
		AdminPassword:        adminPassword,
		AdminSessionTTL:      envDuration("PROMPT_AUDIT_ADMIN_SESSION_TTL", 12*time.Hour),
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
	if c.HTTPListenAddr == "" && c.HTTPSListenAddr == "" {
		return Config{}, fmt.Errorf("at least one of PROMPT_AUDIT_HTTP_ADDR or PROMPT_AUDIT_HTTPS_ADDR is required")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return Config{}, fmt.Errorf("PROMPT_AUDIT_TLS_CERT_FILE and PROMPT_AUDIT_TLS_KEY_FILE must be configured together")
	}
	if c.HTTPSListenAddr != "" && (c.TLSCertFile == "" || c.TLSKeyFile == "") {
		return Config{}, fmt.Errorf("TLS certificate and key are required when PROMPT_AUDIT_HTTPS_ADDR is configured")
	}
	if strings.TrimSpace(c.AdminUsername) == "" || len([]byte(c.AdminUsername)) > 64 || strings.IndexFunc(c.AdminUsername, func(r rune) bool {
		return r == '/' || r == '\\' || r == ':' || r == ' ' || r == '\t' || r == '\r' || r == '\n'
	}) >= 0 {
		return Config{}, fmt.Errorf("PROMPT_AUDIT_ADMIN_USERNAME must be 1-64 characters without whitespace or path separators")
	}
	if c.AdminPassword != "" {
		if len([]byte(c.AdminPassword)) < 12 {
			return Config{}, fmt.Errorf("PROMPT_AUDIT_ADMIN_PASSWORD must be at least 12 bytes when configured")
		}
		if strings.ContainsAny(c.AdminPassword, "\r\n") {
			return Config{}, fmt.Errorf("PROMPT_AUDIT_ADMIN_PASSWORD cannot contain newlines")
		}
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
