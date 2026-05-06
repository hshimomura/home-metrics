# Apple Push Notifications

This document describes how Home Metrics integrates with Apple Push Notification
service (APNs) without committing account-specific or device-specific data to a
public repository.

## Security And Privacy

Do not commit these values:

- APNs `.p8` private key files.
- `APNS_KEY_ID`, `APNS_TEAM_ID`, and production bundle identifiers when they
  identify a private Apple Developer account or unreleased app.
- APNs device tokens.
- Real user names, host names, private IP addresses, MAC addresses, or payloads
  copied from a personal deployment.

Use placeholders in public examples:

```text
APNS_KEY_ID=XXXXXXXXXX
APNS_TEAM_ID=YYYYYYYYYY
APNS_BUNDLE_ID=org.example.home-metrics
```

Store deployment values in `/etc/home-metrics/home-metrics.env` and the private
key in a path such as `/etc/home-metrics/AuthKey_XXXXXXXXXX.p8`.

Recommended permissions:

```bash
sudo install -d -m 0750 -o root -g home_metrics /etc/home-metrics
sudo install -m 0640 -o root -g home_metrics AuthKey_XXXXXXXXXX.p8 /etc/home-metrics/
sudo install -m 0640 -o root -g home_metrics home-metrics.env /etc/home-metrics/
```

## Server Configuration

Set the APNs provider credentials in `/etc/home-metrics/home-metrics.env`:

```bash
APNS_KEY_FILE=/etc/home-metrics/AuthKey_XXXXXXXXXX.p8
APNS_KEY_ID=XXXXXXXXXX
APNS_TEAM_ID=YYYYYYYYYY
APNS_BUNDLE_ID=org.example.home-metrics
```

`APNS_ENVIRONMENT` is intentionally not used by the backend. The APNs endpoint
is selected per registered device:

- `ios_devices.apns_environment = sandbox` sends to
  `https://api.sandbox.push.apple.com`.
- `ios_devices.apns_environment = production` sends to
  `https://api.push.apple.com`.

One APNs Auth Key can send to both sandbox and production as long as the bundle
ID and team match.

After changing APNs settings, restart the API server and alert worker:

```bash
sudo systemctl restart hm-api-server.service hm-alert-worker.service
```

## Client Registration

The iOS app registers its device token with:

```http
POST /api/ios/devices
Authorization: Bearer <API_TOKEN>
Content-Type: application/json
```

```json
{
  "apns_device_token": "<hex device token>",
  "app_bundle_id": "org.example.home-metrics",
  "apns_environment": "sandbox",
  "device_name": "iPhone",
  "enabled": true
}
```

The client is responsible for choosing the environment:

- Xcode-installed Debug builds register `sandbox`.
- TestFlight and App Store builds register `production`.
- Release builds installed outside TestFlight should use the value from the
  app entitlement or provisioning profile.

The backend stores sandbox and production rows separately because APNs treats
their tokens as different environments.

## Sending

`hm-alert-worker` evaluates `alert_rules`, applies cooldown state, records
`notification_events`, and sends APNs notifications when
`ALERT_WORKER_DRY_RUN=false`.

Only enabled iOS devices whose `app_bundle_id` matches `APNS_BUNDLE_ID` are
eligible. If APNs returns `BadDeviceToken`, `Unregistered`, or `410 Gone`, the
backend disables that device and stores `disabled_reason` / `disabled_at`.

## Test Notifications

The Web UI can send a test notification to a registered iOS device through:

```http
POST /api/ios/devices/{id}/test-notification
```

By default, the API server uses the latest `notification_events` row for the
default user as the test payload source. To make tests deterministic in a local
deployment, set:

```bash
APNS_TEST_NOTIFICATION_EVENT_CREATED_AT=2026-01-02T03:04:05Z
```

Do not commit real event timestamps from a private deployment.

APNs HTTP `200` means Apple accepted the notification. It does not guarantee
that a banner will be shown. On iOS, check notification permission, Focus mode,
app notification settings, and foreground presentation handling.

## Troubleshooting

`BadDeviceToken` usually means the token was sent to the wrong environment or
the app was reinstalled and issued a new token.

`BadEnvironmentKeyInToken` means a sandbox token was sent to production, or a
production token was sent to sandbox.

`DeviceTokenNotForTopic` means `APNS_BUNDLE_ID` does not match the app bundle
identifier used when the token was issued.

`Unregistered` or `410 Gone` means APNs considers the token invalid. The backend
will disable the row; the client should re-register when it gets a fresh token.

## Rotation

When rotating an APNs Auth Key:

1. Create a new APNs Auth Key in Apple Developer.
2. Place the new `.p8` file outside the repository.
3. Update `APNS_KEY_FILE` and `APNS_KEY_ID`.
4. Restart `hm-api-server.service` and `hm-alert-worker.service`.
5. Verify sandbox and production test sends.
6. Revoke the old key after the new one is confirmed.
