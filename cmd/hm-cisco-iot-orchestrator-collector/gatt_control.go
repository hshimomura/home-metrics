package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func withGATTControlSession(cfg config, fn func() error) error {
	if cfg.ControlMu == nil {
		return fn()
	}
	cfg.ControlMu.Lock()
	defer cfg.ControlMu.Unlock()
	return fn()
}

func controlConnect(ctx context.Context, cfg config, deviceID string, services []string) error {
	bodyServices := make([]map[string]string, 0, len(services))
	for _, service := range services {
		service = strings.TrimSpace(service)
		if service == "" {
			continue
		}
		bodyServices = append(bodyServices, map[string]string{"serviceID": service})
	}
	if len(bodyServices) == 0 {
		return errors.New("at least one GATT service is required")
	}
	_, err := controlPost(ctx, cfg, "/control/connectivity/connect", map[string]any{
		"technology": "ble",
		"id":         strings.TrimSpace(deviceID),
		"controlApp": cfg.ControlAppID,
		"ble": map[string]any{
			"services": bodyServices,
		},
	})
	return err
}

func controlDisconnect(ctx context.Context, cfg config, deviceID string) error {
	_, err := controlPost(ctx, cfg, "/control/connectivity/disconnect", map[string]any{
		"technology": "ble",
		"id":         strings.TrimSpace(deviceID),
		"controlApp": cfg.ControlAppID,
	})
	return err
}

func controlRead(ctx context.Context, cfg config, deviceID string, serviceID string, characteristicID string) ([]byte, error) {
	body, err := controlPost(ctx, cfg, "/control/data/read", map[string]any{
		"technology": "ble",
		"id":         strings.TrimSpace(deviceID),
		"controlApp": cfg.ControlAppID,
		"ble": map[string]any{
			"serviceID":        serviceID,
			"characteristicID": characteristicID,
		},
	})
	if err != nil {
		return nil, err
	}
	var response struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	return decodeHexValue(response.Value)
}

func controlWrite(ctx context.Context, cfg config, deviceID string, serviceID string, characteristicID string, payload []byte) error {
	_, err := controlPost(ctx, cfg, "/control/data/write", map[string]any{
		"technology": "ble",
		"id":         strings.TrimSpace(deviceID),
		"controlApp": cfg.ControlAppID,
		"ble": map[string]any{
			"serviceID":        serviceID,
			"characteristicID": characteristicID,
		},
		"value": strings.ToLower(hex.EncodeToString(payload)),
	})
	return err
}

func controlPost(ctx context.Context, cfg config, path string, body map[string]any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.APIURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", cfg.ControlAPIKey)
	resp, err := doHTTPRequest(cfg, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	limited, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s status=%s body=%s", path, resp.Status, string(limited))
	}
	var result struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(limited, &result); err == nil && strings.EqualFold(result.Status, "FAILURE") {
		if result.Reason == "" {
			result.Reason = string(limited)
		}
		return nil, fmt.Errorf("%s failed: %s", path, result.Reason)
	}
	return limited, nil
}

func decodeHexValue(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, " ", "")
	value = strings.ReplaceAll(value, ":", "")
	value = strings.TrimPrefix(value, "0x")
	if value == "" {
		return nil, errors.New("empty hex value")
	}
	data, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode hex value %q: %w", value, err)
	}
	return data, nil
}

func httpClient(cfg config) *http.Client {
	if !cfg.TLSSkipVerify {
		return http.DefaultClient
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Lab IoT Orchestrator uses an IP-address HTTPS endpoint.
		},
		Timeout: 60 * time.Second,
	}
}
