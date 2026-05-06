package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	apnsSandboxHost    = "https://api.sandbox.push.apple.com"
	apnsProductionHost = "https://api.push.apple.com"
)

type apnsTestSender struct {
	client     *http.Client
	keyID      string
	teamID     string
	bundleID   string
	privateKey *ecdsa.PrivateKey
	token      string
	tokenIAT   time.Time
}

type apnsTestTarget struct {
	ID              int64
	Token           string
	AppBundleID     string
	APNSEnvironment string
	DeviceName      *string
	Enabled         bool
}

type apnsErrorResponse struct {
	Reason string `json:"reason"`
}

func newAPNSTestSenderFromEnv(client *http.Client) (*apnsTestSender, error) {
	keyID := strings.TrimSpace(os.Getenv("APNS_KEY_ID"))
	teamID := strings.TrimSpace(os.Getenv("APNS_TEAM_ID"))
	bundleID := strings.TrimSpace(os.Getenv("APNS_BUNDLE_ID"))
	keyFile := strings.TrimSpace(os.Getenv("APNS_KEY_FILE"))
	if keyID == "" || teamID == "" || bundleID == "" || keyFile == "" {
		return nil, errors.New("APNS_KEY_ID, APNS_TEAM_ID, APNS_BUNDLE_ID, and APNS_KEY_FILE are required")
	}
	privateKey, err := loadAPNSPrivateKey(keyFile)
	if err != nil {
		return nil, err
	}
	return &apnsTestSender{
		client:     client,
		keyID:      keyID,
		teamID:     teamID,
		bundleID:   bundleID,
		privateKey: privateKey,
	}, nil
}

func loadAPNSPrivateKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read APNs key file: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("APNs key file does not contain a PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse APNs private key: %w", err)
	}
	privateKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("APNs private key must be ECDSA")
	}
	return privateKey, nil
}

func (s *apnsTestSender) loadTarget(ctx context.Context, db *pgxpool.Pool, userID int64, id int64) (apnsTestTarget, error) {
	var target apnsTestTarget
	var deviceName pgtype.Text
	err := db.QueryRow(ctx, `
		SELECT
			id,
			apns_device_token,
			app_bundle_id,
			apns_environment,
			device_name,
			enabled
		FROM ios_devices
		WHERE id = $1 AND user_id = $2
	`, id, userID).Scan(
		&target.ID,
		&target.Token,
		&target.AppBundleID,
		&target.APNSEnvironment,
		&deviceName,
		&target.Enabled,
	)
	if deviceName.Valid {
		target.DeviceName = &deviceName.String
	}
	return target, err
}

func (s *apnsTestSender) send(ctx context.Context, db *pgxpool.Pool, target apnsTestTarget, event testNotificationEvent, now time.Time) error {
	token, err := s.jwt(now)
	if err != nil {
		return err
	}
	deviceLabel := loadDeviceLabel(ctx, db, event.MAC)
	title, alertBody := alertContent(deviceLabel, event.Metric, event.Value)
	body, err := json.Marshal(map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{
				"title": title,
				"body":  alertBody,
			},
			"sound":     "default",
			"thread-id": "sensor:" + event.MAC,
		},
		"type":                          "test_notification",
		"roompulse":                     sensorRoutePayload(event.MAC, event.Metric),
		"ios_device_id":                 target.ID,
		"notification_event_id":         event.ID,
		"notification_event_created_at": event.CreatedAt.Format(time.RFC3339Nano),
		"alert_rule_id":                 event.AlertRuleID,
		"mac":                           event.MAC,
		"metric":                        event.Metric,
		"value":                         event.Value,
		"threshold":                     event.Threshold,
		"triggered_at":                  event.TriggeredAt.Format(time.RFC3339Nano),
		"original_status":               event.Status,
		"original_sent_at":              formatOptionalTime(event.SentAt),
		"original_error_message":        event.ErrorMessage,
		"sent_at":                       now.Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, apnsHost(target.APNSEnvironment)+"/3/device/"+target.Token, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("apns-topic", s.bundleID)
	request.Header.Set("apns-push-type", "alert")
	request.Header.Set("apns-priority", "10")

	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}

	apnsErr := apnsErrorResponse{}
	_ = json.Unmarshal(responseBody, &apnsErr)
	if response.StatusCode == http.StatusGone || apnsErr.Reason == "BadDeviceToken" || apnsErr.Reason == "Unregistered" {
		if err := s.disableTarget(ctx, db, target.ID, apnsDisableReason(response.StatusCode, apnsErr.Reason)); err != nil {
			return fmt.Errorf("APNs status=%d reason=%s; disable token: %w", response.StatusCode, apnsErr.Reason, err)
		}
	}
	if apnsErr.Reason != "" {
		return fmt.Errorf("APNs status=%d reason=%s", response.StatusCode, apnsErr.Reason)
	}
	return fmt.Errorf("APNs status=%d body=%s", response.StatusCode, strings.TrimSpace(string(responseBody)))
}

func (s *apnsTestSender) disableTarget(ctx context.Context, db *pgxpool.Pool, id int64, reason string) error {
	_, err := db.Exec(ctx, `
		UPDATE ios_devices
		SET enabled = false,
			disabled_reason = $2,
			disabled_at = now(),
			updated_at = now()
		WHERE id = $1
	`, id, reason)
	return err
}

func (s *apnsTestSender) jwt(now time.Time) (string, error) {
	if s.token != "" && now.Sub(s.tokenIAT) < 50*time.Minute {
		return s.token, nil
	}
	header := map[string]string{
		"alg": "ES256",
		"kid": s.keyID,
	}
	claims := map[string]any{
		"iss": s.teamID,
		"iat": now.Unix(),
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	r, signS, err := ecdsa.Sign(rand.Reader, s.privateKey, digest[:])
	if err != nil {
		return "", err
	}
	signature := appendFixedWidth(r, signS, 32)
	s.token = signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
	s.tokenIAT = now
	return s.token, nil
}

func appendFixedWidth(r *big.Int, s *big.Int, width int) []byte {
	signature := make([]byte, width*2)
	r.FillBytes(signature[:width])
	s.FillBytes(signature[width:])
	return signature
}

func apnsHost(environment string) string {
	if environment == "production" {
		return apnsProductionHost
	}
	return apnsSandboxHost
}

func apnsDisableReason(statusCode int, reason string) string {
	if reason != "" {
		return reason
	}
	return fmt.Sprintf("APNs status=%d", statusCode)
}

func loadDeviceLabel(ctx context.Context, db *pgxpool.Pool, mac string) string {
	var label string
	if err := db.QueryRow(ctx, `
		SELECT label
		FROM devices
		WHERE mac = $1
	`, mac).Scan(&label); err == nil && strings.TrimSpace(label) != "" {
		return label
	}
	return mac
}

func sensorRoutePayload(mac string, metric string) map[string]any {
	return map[string]any{
		"route":     "sensor_detail",
		"device_id": mac,
		"mac":       mac,
		"metric":    metric,
	}
}

func alertContent(deviceLabel string, metric string, value *float64) (string, string) {
	name, unit, digits := alertMetricPresentation(metric)
	title := fmt.Sprintf("%s %s alert", deviceLabel, name)
	if value == nil {
		return title, fmt.Sprintf("Current %s is unavailable.", name)
	}
	return title, fmt.Sprintf("Current %s is %s%s.", name, formatAlertValue(*value, digits), unit)
}

func alertMetricPresentation(metric string) (name string, unit string, digits int) {
	switch metric {
	case "temperature_c":
		return "temperature", " °C", 1
	case "humidity_percent":
		return "humidity", " %", 0
	case "battery_percent":
		return "battery", " %", 0
	case "lux":
		return "illuminance", " lux", 0
	case "pressure_hpa":
		return "pressure", " hPa", 0
	case "co2_ppm":
		return "CO2", " ppm", 0
	case "etvoc":
		return "eTVOC", "", 0
	case "rssi_dbm":
		return "signal", " dBm", 0
	default:
		return metric, "", 1
	}
}

func formatAlertValue(value float64, digits int) string {
	return fmt.Sprintf("%.*f", digits, value)
}

func formatOptionalTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.Format(time.RFC3339Nano)
	return &formatted
}
