package adminwebhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewDisabledWithoutURL(t *testing.T) {
	client, err := New("", "", 0, nil)
	if err != nil {
		t.Fatalf("New disabled error = %v", err)
	}
	if client != nil {
		t.Fatal("New disabled client != nil")
	}
}

func TestNewRequiresTokenWhenURLSet(t *testing.T) {
	_, err := New("http://example.test/webhook", "", 0, nil)
	if err == nil {
		t.Fatal("New with URL and empty token error = nil")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error should not contain secret material: %v", err)
	}
}

func TestSendPostsJSONWithBearerToken(t *testing.T) {
	var got Payload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Fatalf("Authorization = %q, want bearer token", auth)
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", contentType)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client, err := New(server.URL, "test-token", time.Second, server.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Send(context.Background(), Payload{
		EventID:  "hm-test",
		Status:   "info",
		Severity: "info",
		Title:    "test",
		Source:   "home-metrics",
		Summary:  "webhook test",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.EventID != "hm-test" || got.Source != "home-metrics" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestSendReturnsHTTPErrorWithoutTokenLeak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer server.Close()

	client, err := New(server.URL, "very-secret-token", time.Second, server.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := client.Send(context.Background(), Payload{
		EventID:  "hm-test",
		Status:   "info",
		Severity: "info",
		Title:    "test",
		Source:   "home-metrics",
		Summary:  "webhook test",
	})
	if err == nil {
		t.Fatal("Send error = nil, want error")
	}
	if result.StatusCode != http.StatusForbidden {
		t.Fatalf("StatusCode = %d, want %d", result.StatusCode, http.StatusForbidden)
	}
	if strings.Contains(err.Error(), "very-secret-token") || strings.Contains(result.ResponseBody, "very-secret-token") {
		t.Fatal("secret token leaked")
	}
}
