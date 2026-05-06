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

	"github.com/jackc/pgx/v5"
)

const (
	apnsSandboxHost    = "https://api.sandbox.push.apple.com"
	apnsProductionHost = "https://api.push.apple.com"
)

type dryRunNotifier struct{}

func (dryRunNotifier) Mode() string {
	return "dry-run"
}

func (dryRunNotifier) Notify(context.Context, *pgx.Conn, alertRule, latestValue, time.Time) (notificationResult, error) {
	return notificationResult{Status: "dry_run"}, nil
}

type apnsNotifier struct {
	client     *http.Client
	keyID      string
	teamID     string
	bundleID   string
	privateKey *ecdsa.PrivateKey
	token      string
	tokenIAT   time.Time
}

type iosTarget struct {
	ID          int64
	Token       string
	Environment string
}

type apnsErrorResponse struct {
	Reason    string `json:"reason"`
	Timestamp int64  `json:"timestamp"`
}

func newNotifierFromEnv(dryRun bool, client *http.Client) (notifier, error) {
	if dryRun {
		return dryRunNotifier{}, nil
	}

	keyID := strings.TrimSpace(os.Getenv("APNS_KEY_ID"))
	teamID := strings.TrimSpace(os.Getenv("APNS_TEAM_ID"))
	bundleID := strings.TrimSpace(os.Getenv("APNS_BUNDLE_ID"))
	keyFile := strings.TrimSpace(os.Getenv("APNS_KEY_FILE"))
	if keyID == "" || teamID == "" || bundleID == "" || keyFile == "" {
		return nil, errors.New("APNS_KEY_ID, APNS_TEAM_ID, APNS_BUNDLE_ID, and APNS_KEY_FILE are required when ALERT_WORKER_DRY_RUN=false")
	}

	privateKey, err := loadAPNSPrivateKey(keyFile)
	if err != nil {
		return nil, err
	}
	return &apnsNotifier{
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

func (n *apnsNotifier) Mode() string {
	return "apns-sandbox-production"
}

func (n *apnsNotifier) Notify(ctx context.Context, db *pgx.Conn, rule alertRule, value latestValue, now time.Time) (notificationResult, error) {
	targets, err := n.loadTargets(ctx, db, rule.UserID)
	if err != nil {
		return notificationResult{}, err
	}
	if len(targets) == 0 {
		message := "no enabled iOS devices for APNs bundle"
		return notificationResult{Status: "skipped", ErrorMessage: &message}, nil
	}

	var failures []string
	sentCount := 0
	for _, target := range targets {
		if err := n.send(ctx, db, target, rule, value, now); err != nil {
			failures = append(failures, fmt.Sprintf("ios_device_id=%d: %v", target.ID, err))
			continue
		}
		sentCount++
	}
	if sentCount == 0 {
		message := strings.Join(failures, "; ")
		return notificationResult{Status: "failed", ErrorMessage: &message}, nil
	}
	sentAt := now
	if len(failures) > 0 {
		message := strings.Join(failures, "; ")
		return notificationResult{Status: "sent", SentAt: &sentAt, ErrorMessage: &message}, nil
	}
	return notificationResult{Status: "sent", SentAt: &sentAt}, nil
}

func (n *apnsNotifier) loadTargets(ctx context.Context, db *pgx.Conn, userID int64) ([]iosTarget, error) {
	rows, err := db.Query(ctx, `
		SELECT id, apns_device_token, apns_environment
		FROM ios_devices
		WHERE user_id = $1
			AND enabled
			AND app_bundle_id = $2
		ORDER BY id
	`, userID, n.bundleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	targets := []iosTarget{}
	for rows.Next() {
		var target iosTarget
		if err := rows.Scan(&target.ID, &target.Token, &target.Environment); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (n *apnsNotifier) send(ctx context.Context, db *pgx.Conn, target iosTarget, rule alertRule, value latestValue, now time.Time) error {
	token, err := n.jwt(now)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{
				"title": "BLE sensor alert",
				"body":  alertBody(rule, value),
			},
			"sound": "default",
		},
		"rule_id":   rule.ID,
		"mac":       rule.MAC,
		"metric":    rule.Metric,
		"value":     value.Value,
		"threshold": rule.Threshold,
	})
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		apnsHost(target.Environment)+"/3/device/"+target.Token,
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("apns-topic", n.bundleID)
	request.Header.Set("apns-push-type", "alert")
	request.Header.Set("apns-priority", "10")

	response, err := n.client.Do(request)
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
		if err := n.disableTarget(ctx, db, target.ID, apnsDisableReason(response.StatusCode, apnsErr.Reason)); err != nil {
			return fmt.Errorf("APNs status=%d reason=%s; disable token: %w", response.StatusCode, apnsErr.Reason, err)
		}
	}
	if apnsErr.Reason != "" {
		return fmt.Errorf("APNs status=%d reason=%s", response.StatusCode, apnsErr.Reason)
	}
	return fmt.Errorf("APNs status=%d body=%s", response.StatusCode, strings.TrimSpace(string(responseBody)))
}

func (n *apnsNotifier) disableTarget(ctx context.Context, db *pgx.Conn, id int64, reason string) error {
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

func (n *apnsNotifier) jwt(now time.Time) (string, error) {
	if n.token != "" && now.Sub(n.tokenIAT) < 50*time.Minute {
		return n.token, nil
	}
	header := map[string]string{
		"alg": "ES256",
		"kid": n.keyID,
	}
	claims := map[string]any{
		"iss": n.teamID,
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
	r, s, err := ecdsa.Sign(rand.Reader, n.privateKey, digest[:])
	if err != nil {
		return "", err
	}
	signature := appendFixedWidth(r, s, 32)
	n.token = signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
	n.tokenIAT = now
	return n.token, nil
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

func alertBody(rule alertRule, value latestValue) string {
	return fmt.Sprintf("%s %s %.2f: current %.2f at %s", rule.Metric, rule.Operator, rule.Threshold, value.Value, value.TS.Format(time.RFC3339))
}
