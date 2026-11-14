package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestMotionEventBounds(t *testing.T) {
	now := time.Now()
	for _, q := range []string{"limit=0", "limit=5001", "limit=no", "since=invalid"} {
		if _, _, err := motionEventParams(httptest.NewRequest("GET", "/api/?"+q, nil), now); err == nil {
			t.Fatalf("accepted %s", q)
		}
	}
	since, limit, err := motionEventParams(httptest.NewRequest("GET", "/api/", nil), now)
	if err != nil || limit != 1000 || !since.Equal(now.Add(-24*time.Hour)) {
		t.Fatal("invalid defaults")
	}
}
