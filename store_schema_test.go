package main

import (
	"strings"
	"testing"
)

func TestAuditSchemaDoesNotPersistResponsesOrRawBodies(t *testing.T) {
	schema, err := migrationFS.ReadFile("migrations/001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(schema))
	for _, forbidden := range []string{"response_body", "response_headers", "raw_request_body", "system_prompt", "authorization"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("schema contains forbidden field %q", forbidden)
		}
	}
	if !strings.Contains(lower, "manual_prompt_audit_records") {
		t.Fatal("audit table is missing")
	}
}
