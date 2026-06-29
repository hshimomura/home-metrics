# Release And Deployment

GitHub is the canonical source repository. CI validates the Go code and API
contract; the Container workflow publishes the runtime image. Production on
`nms4` is managed by `ioslab-docs/servicecore`.

## Compatibility Lines

`v2.0.0` is a deliberate database compatibility boundary. It starts from the
squashed `0001_initial_schema.sql`; migrations from the v1 development history
are not part of the active tree and arbitrary v1 databases are not accepted as
an automatic upgrade source.

| Release line | Database contract | Status |
| --- | --- | --- |
| `v2.0.0+` | `0001_initial_schema.sql`, followed by immutable `0002+` migrations | Active |
| `v1.0.0` through `v1.1.1` | Legacy migration history and matching v1 schema | Historical |

Git tags and GitHub Releases from v1 remain available for audit and source
reproducibility. Their release notes must warn that the artifacts are not valid
for a v2 database.

Application and database rollback are one operation. Never point a v1 image at
the v2 database. To restore v1, restore a matching v1 logical backup and its
matching image together. If no matching backup exists, remain on v2.

## Published Image

```text
ghcr.io/hshimomura/home-metrics
```

The Container workflow publishes:

- `main` for the latest main-branch build;
- `sha-<short-sha>` for a traceable commit build;
- `vX.Y.Z` for a release tag.

Deploy by immutable digest, never by a moving tag:

```text
ghcr.io/hshimomura/home-metrics@sha256:<digest>
```

## CI Contract

The CI workflow runs:

```sh
make test
make build
npx --yes @redocly/cli@1.34.5 lint docs/openapi.yaml
```

`make build` compiles every command listed in `tools/build.sh`. Sensor contract
tests also verify that the canonical metric registry is represented in the
schema, initial baseline migration, OpenAPI contract, and web UI.

## Database Migrations

`db/schema.sql` and `db/migrations/0001_initial_schema.sql` are the same current
baseline. The migration history before this baseline is intentionally not
supported. Follow these rules:

1. Never edit `0001` after it is deployed.
2. Add `0002` and later for every future existing-volume schema change.
3. Update `db/schema.sql` to the same final state.
4. Keep migration SQL compatible with `hm-db-migrate` checksum validation.
5. Make API, workers, and collectors depend on successful migration completion.

`0001_initial_schema.sql` contains the normalized sensor and energy schema,
metric-specific weighted rollup counts, collector status, and sensor threshold
rules/state/events. It does not contain the retired APNs, webhook, user
registration, notification delivery, health-alert, raw payload, or maintenance
tables.

The one production database that predates this baseline is adopted explicitly:

1. confirm its live schema already matches the pre-alert current schema;
2. clear only `schema_migrations` directly in PostgreSQL;
3. deploy the new image and let `hm-db-migrate` apply idempotent `0001`;
4. verify that version `1` is the only migration row and application data row
   counts are preserved.

This is a controlled one-time operation, not a general upgrade path. Databases
from arbitrary older repository revisions must be rebuilt or migrated manually.
Future upgrades begin with `0002`.

After adopting the v2 baseline, create a PostgreSQL custom-format logical
backup and prove it with `pg_restore --list` plus a temporary full restore. Keep
the archive outside the repository with mode `0600`, record its SHA-256, and do
not store database data or credentials in GitHub Releases.

The first maintenance run initializes an immutable rollup accuracy cutoff and
rebuilds only complete retained buckets at or after it. Do not reset that cutoff
during a normal release.

## Main-Branch Deployment

1. Push or merge the home-metrics change to `main`.
2. Confirm both `CI` and `Container` complete successfully.
3. Read the digest from the Container workflow summary.
4. In `ioslab-docs`, update the servicecore home-metrics digest.
5. Run the local servicecore Docker check and deployment contract tests.
6. Commit and push the servicecore change.
7. Confirm `Servicecore / Docker / check` and
   `Servicecore / Docker / deploy` complete for nms4.
8. Verify the running containers and data path on nms4.

The servicecore helper resolves and updates the current `main` digest:

```sh
./servicecore/scripts/update-home-metrics-digest.sh main
./servicecore/scripts/check-docker-host.sh nms4
./servicecore/scripts/test-docker-deploy-contract.sh
```

## Tagged Release

1. Complete and verify the main-branch deployment.
2. Create an annotated `vX.Y.Z` tag from the verified commit.
3. Push the tag and confirm the Container workflow publishes it.
4. Create a GitHub Release describing API, schema, collector, and operational
   changes.
5. Resolve the release-tag digest and update servicecore if production should
   pin the release image rather than the main image.

For the `v2.0.0` boundary release, also verify:

- GitHub Release notes identify `v1.1.1` and earlier as historical-only;
- the release notes state that v1 and v2 databases are not cross-compatible;
- a tested v2 baseline database backup exists;
- servicecore pins the `v2.0.0` tag image by digest after its Container workflow
  succeeds.

Do not put an unverified digest in release notes. The tag-triggered build can
produce a different manifest digest from an earlier main build.

## nms4 Verification

From `/srv/docker/home-metrics`, verify the expected profiles and image:

```sh
docker compose \
  --profile cisco-iot \
  --profile nature-remo \
  --profile apcupsd \
  --profile echonet \
  ps

docker inspect -f '{{.Name}} {{.Config.Image}} {{.State.Status}}' \
  home-metrics-hm-api-server-1 \
  home-metrics-hm-sensor-alert-worker-1 \
  home-metrics-hm-db-maint-1 \
  home-metrics-hm-cisco-iot-orchestrator-collector-1
```

Verify schema and collector status:

```sh
docker compose exec -T db psql -U home_metrics -d ble_sensors -P pager=off \
  -c 'select version,name,applied_at from schema_migrations order by version desc limit 5;'

docker compose exec -T db psql -U home_metrics -d ble_sensors -P pager=off \
  -c 'select collector_name,target_type,target_key,last_success_at,last_data_at,last_error,consecutive_failures from collector_status order by collector_name,target_type,target_key;'
```

Use the configured token without printing it:

```sh
curl -H "Authorization: Bearer $API_TOKEN" \
  https://metrics.ioslab.jp/api/health/details

curl -H "Authorization: Bearer $API_TOKEN" \
  'https://metrics.ioslab.jp/api/sensor-alerts?status=firing'
```

The final verification should establish:

- every running application container uses the pinned digest;
- migration completed successfully;
- API health is `ok` and no expected collector is stale;
- the sensor alert worker is running and alert rule/state/event APIs respond;
- recent telemetry exists for enabled devices;
- logs contain no `conn busy`, ownership conflict, panic, or fatal error;
- Cisco Spaces remains stopped and excluded when intentionally disabled.

## Rollback

Within the v2 line, application rollback means restoring a previous known-good
v2 servicecore image digest when that image supports the current schema. A
database migration is not automatically reversible. Before releasing a
destructive migration, document its data impact and a specific restore
procedure. Across the v1/v2 boundary, restore the matching database and image
together; image-only rollback is forbidden.
