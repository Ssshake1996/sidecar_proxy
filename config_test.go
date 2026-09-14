package main

import "testing"

func setMinimalConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"PROMPT_AUDIT_HTTP_ADDR", "PROMPT_AUDIT_HTTPS_ADDR", "PROMPT_AUDIT_LISTEN_ADDR",
		"PROMPT_AUDIT_TLS_CERT_FILE", "PROMPT_AUDIT_TLS_KEY_FILE", "PROMPT_AUDIT_ADMIN_PASSWORD",
		"PROMPT_AUDIT_IDENTITY_DATABASE_URL", "PROMPT_AUDIT_IDENTITY_HMAC_SECRET",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("PROMPT_AUDIT_DATABASE_URL", "postgres://audit@localhost/audit?sslmode=disable")
	t.Setenv("PROMPT_AUDIT_UPSTREAM_URL", "http://127.0.0.1:8080")
}

func TestLoadConfigDefaultsToHTTPAndKeepsHTTPSOptional(t *testing.T) {
	setMinimalConfigEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPListenAddr != ":8090" || cfg.HTTPSListenAddr != "" || cfg.AdminUsername != "admin" {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
}

func TestLoadConfigHTTPSRequiresCertificatePair(t *testing.T) {
	setMinimalConfigEnv(t)
	t.Setenv("PROMPT_AUDIT_HTTPS_ADDR", ":8443")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected HTTPS certificate validation error")
	}

	t.Setenv("PROMPT_AUDIT_TLS_CERT_FILE", "/cert/fullchain.pem")
	t.Setenv("PROMPT_AUDIT_TLS_KEY_FILE", "/cert/privkey.pem")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPSListenAddr != ":8443" || cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		t.Fatalf("HTTPS config was not loaded: %#v", cfg)
	}
}
