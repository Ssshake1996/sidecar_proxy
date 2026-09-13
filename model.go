package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

type Identity struct {
	UserID         *int64 `json:"user_id,omitempty"`
	Username       string `json:"username,omitempty"`
	Email          string `json:"email,omitempty"`
	APIKeyID       *int64 `json:"api_key_id,omitempty"`
	APIKeyName     string `json:"api_key_name,omitempty"`
	IdentitySource string `json:"identity_source"`
}

func (i Identity) Resolved() bool {
	return i.UserID != nil && *i.UserID > 0
}

type UserMessage struct {
	Index  int    `json:"index"`
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
}

type Extraction struct {
	Protocol      string
	Model         string
	Messages      []UserMessage
	ExcludedRoles map[string]int
	Status        string
	Truncated     bool
	PromptText    string
	PromptSHA256  string
}

type CaptureRecord struct {
	ID              string
	ReceivedAt      time.Time
	RequestID       string
	ClientRequestID string
	Identity        Identity
	Endpoint        string
	Protocol        string
	Model           string
	Messages        []UserMessage
	PromptText      string
	PromptSHA256    string
	MessageCount    int
	ParseStatus     string
	Truncated       bool
	ExcludedRoles   map[string]int
}

func (e Extraction) finalize(maxPromptBytes int) Extraction {
	if e.ExcludedRoles == nil {
		e.ExcludedRoles = map[string]int{}
	}
	parts := make([]string, 0, len(e.Messages))
	for _, message := range e.Messages {
		if strings.TrimSpace(message.Text) != "" {
			// Keep the client's text unchanged; only use TrimSpace to decide
			// whether an all-whitespace segment is worth storing.
			parts = append(parts, message.Text)
		}
	}
	fullPrompt := strings.Join(parts, "\n\n")
	sum := sha256.Sum256([]byte(fullPrompt))
	e.PromptSHA256 = hex.EncodeToString(sum[:])
	if maxPromptBytes > 0 && len([]byte(fullPrompt)) > maxPromptBytes {
		e.PromptText = truncateUTF8(fullPrompt, maxPromptBytes)
		e.Truncated = true
	} else {
		e.PromptText = fullPrompt
	}
	if len(e.Messages) == 0 && e.Status == "" {
		e.Status = "empty"
	}
	if len(e.Messages) > 0 && e.Status == "" {
		e.Status = "parsed"
	}
	return e
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 || len([]byte(value)) <= maxBytes {
		return value
	}
	var size int
	var end int
	for index, r := range value {
		runeSize := len(string(r))
		if size+runeSize > maxBytes {
			break
		}
		size += runeSize
		end = index + runeSize
	}
	return value[:end]
}
