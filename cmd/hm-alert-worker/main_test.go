package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestMatches(t *testing.T) {
	tests := []struct {
		name      string
		operator  string
		value     float64
		threshold float64
		want      bool
	}{
		{name: "greater true", operator: ">", value: 35.1, threshold: 35, want: true},
		{name: "greater equal false", operator: ">", value: 35, threshold: 35, want: false},
		{name: "greater or equal true", operator: ">=", value: 35, threshold: 35, want: true},
		{name: "less true", operator: "<", value: 19.9, threshold: 20, want: true},
		{name: "less equal true", operator: "<=", value: 20, threshold: 20, want: true},
		{name: "unsupported false", operator: "=", value: 20, threshold: 20, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matches(tt.operator, tt.value, tt.threshold)
			if got != tt.want {
				t.Fatalf("matches(%q, %f, %f) = %t, want %t", tt.operator, tt.value, tt.threshold, got, tt.want)
			}
		})
	}
}

func TestNewNotifierFromEnvDryRun(t *testing.T) {
	t.Setenv("APNS_KEY_ID", "")
	t.Setenv("APNS_TEAM_ID", "")
	t.Setenv("APNS_BUNDLE_ID", "")
	t.Setenv("APNS_KEY_FILE", "")

	notifier, err := newNotifierFromEnv(true, nil)
	if err != nil {
		t.Fatalf("newNotifierFromEnv(true) error = %v", err)
	}
	if notifier.Mode() != "dry-run" {
		t.Fatalf("Mode() = %q, want dry-run", notifier.Mode())
	}
}

func TestNewNotifierFromEnvRequiresAPNSConfig(t *testing.T) {
	t.Setenv("APNS_KEY_ID", "")
	t.Setenv("APNS_TEAM_ID", "")
	t.Setenv("APNS_BUNDLE_ID", "")
	t.Setenv("APNS_KEY_FILE", "")

	_, err := newNotifierFromEnv(false, nil)
	if err == nil {
		t.Fatal("newNotifierFromEnv(false) error = nil, want error")
	}
}

func TestAPNSJWT(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	notifier := &apnsNotifier{
		keyID:      "KEY123",
		teamID:     "TEAM123",
		privateKey: privateKey,
	}

	token, err := notifier.jwt(time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt parts = %d, want 3", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if len(signature) != 64 {
		t.Fatalf("signature len = %d, want 64", len(signature))
	}

	cached, err := notifier.jwt(time.Unix(1_700_000_100, 0))
	if err != nil {
		t.Fatalf("cached jwt: %v", err)
	}
	if cached != token {
		t.Fatal("jwt was not cached inside 50 minutes")
	}
}
