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
	client      *http.Client
	keyID       string
	teamID      string
	bundleID    string
	environment string
	privateKey  *ecdsa.PrivateKey
	host        string
	token       string
	tokenIAT    time.Time
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
	environment := strings.TrimSpace(os.Getenv("APNS_ENVIRONMENT"))
	keyFile := strings.TrimSpace(os.Getenv("APNS_KEY_FILE"))
	if environment == "" {
		environment = "sandbox"
	}
	if keyID == "" || teamID == "" || bundleID == "" || keyFile == "" {
		return nil, errors.New("APNS_KEY_ID, APNS_TEAM_ID, APNS_BUNDLE_ID, and APNS_KEY_FILE are required")
	}
	if environment != "sandbox" && environment != "production" {
		return nil, errors.New("APNS_ENVIRONMENT must be sandbox or production")
	}
	privateKey, err := loadAPNSPrivateKey(keyFile)
	if err != nil {
		return nil, err
	}
	host := apnsSandboxHost
	if environment == "production" {
		host = apnsProductionHost
	}
	return &apnsTestSender{
		client:      client,
		keyID:       keyID,
		teamID:      teamID,
		bundleID:    bundleID,
		environment: environment,
		privateKey:  privateKey,
		host:        host,
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
	body, err := json.Marshal(map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{
				"title": "RoomPulse Test",
				"body":  testNotificationBody(event),
			},
			"sound": "default",
		},
		"type":                          "test_notification",
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
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.host+"/3/device/"+target.Token, bytes.NewReader(body))
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
		if err := s.disableTarget(ctx, db, target.ID); err != nil {
			return fmt.Errorf("APNs status=%d reason=%s; disable token: %w", response.StatusCode, apnsErr.Reason, err)
		}
	}
	if apnsErr.Reason != "" {
		return fmt.Errorf("APNs status=%d reason=%s", response.StatusCode, apnsErr.Reason)
	}
	return fmt.Errorf("APNs status=%d body=%s", response.StatusCode, strings.TrimSpace(string(responseBody)))
}

func (s *apnsTestSender) disableTarget(ctx context.Context, db *pgxpool.Pool, id int64) error {
	_, err := db.Exec(ctx, `
		UPDATE ios_devices
		SET enabled = false, updated_at = now()
		WHERE id = $1
	`, id)
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

func testNotificationBody(event testNotificationEvent) string {
	value := "n/a"
	if event.Value != nil {
		value = fmt.Sprintf("%.2f", *event.Value)
	}
	threshold := "n/a"
	if event.Threshold != nil {
		threshold = fmt.Sprintf("%.2f", *event.Threshold)
	}
	return fmt.Sprintf("%s %s value=%s threshold=%s at %s", event.MAC, event.Metric, value, threshold, event.TriggeredAt.Format(time.RFC3339))
}

func formatOptionalTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.Format(time.RFC3339Nano)
	return &formatted
}
