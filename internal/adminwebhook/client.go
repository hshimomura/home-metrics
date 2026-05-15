package adminwebhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxResponsePreviewBytes = 2048

type Payload struct {
	EventID          string            `json:"event_id"`
	EventType        string            `json:"event_type,omitempty"`
	Status           string            `json:"status"`
	Severity         string            `json:"severity"`
	AlertKey         string            `json:"alert_key,omitempty"`
	Title            string            `json:"title"`
	Source           string            `json:"source"`
	Summary          string            `json:"summary"`
	Labels           map[string]string `json:"labels,omitempty"`
	Impact           map[string]any    `json:"impact,omitempty"`
	Timestamps       map[string]string `json:"timestamps,omitempty"`
	SuggestedActions []string          `json:"suggested_actions,omitempty"`
	URL              string            `json:"url,omitempty"`
}

type Result struct {
	StatusCode   int
	ResponseBody string
}

type Client struct {
	URL        string
	Token      string
	HTTPClient *http.Client
}

func New(url string, token string, timeout time.Duration, httpClient *http.Client) (*Client, error) {
	url = strings.TrimSpace(url)
	token = strings.TrimSpace(token)
	if url == "" {
		return nil, nil
	}
	if token == "" {
		return nil, errors.New("WEBHOOK_RELAY_TOKEN is required when WEBHOOK_RELAY_URL is set")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	} else if httpClient.Timeout == 0 {
		httpClient.Timeout = timeout
	}
	return &Client{URL: url, Token: token, HTTPClient: httpClient}, nil
}

func (c *Client) Send(ctx context.Context, payload Payload) (Result, error) {
	if c == nil {
		return Result{}, errors.New("admin webhook is not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	res, err := c.HTTPClient.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer res.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(res.Body, maxResponsePreviewBytes))
	result := Result{StatusCode: res.StatusCode, ResponseBody: strings.TrimSpace(string(responseBody))}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return result, fmt.Errorf("admin webhook status=%d", res.StatusCode)
	}
	return result, nil
}
