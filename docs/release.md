# Release and Deployment

`home-metrics` is the canonical server-side source repository.

The production deployment is managed by `ioslab-docs/servicecore`; this repository publishes
the server image and API contract artifacts that `servicecore` and `RoomPlus` consume.

## Repository Policy

- GitHub is the canonical repository for server development.
- The repository is public.
- Container images are published to public GHCR.
- `servicecore` should deploy by image digest, not by a moving tag.

## Container Image

Image name:

```text
ghcr.io/hshimomura/home-metrics
```

Tags:

- `main`: latest image built from the main branch
- `sha-<short-sha>`: traceable development image
- `vX.Y.Z`: release image

During active development, `servicecore` deploys the digest resolved from the `main` tag.
After the service settles, production deploys should move to digests resolved from `vX.Y.Z`
release tags.

Deployment should always use a digest, not the moving tag itself.

Example:

```text
ghcr.io/hshimomura/home-metrics@sha256:<digest>
```

After the first publish, verify that the GHCR package visibility is public in GitHub package
settings. The deployment model assumes public pull access from `nms4`.

## Release Flow

Active development flow:

1. Merge server-side changes to `main`.
2. Confirm CI passes.
3. Confirm the Container workflow publishes `ghcr.io/hshimomura/home-metrics:main`.
4. Confirm whether the change needs a DB migration.
5. If migration is required, make it explicit in the PR or release note and confirm it
   manually before the `servicecore` deploy.
6. Update `ioslab-docs/servicecore` to use the digest resolved from `main`.
7. Let `servicecore-docker-check` and `servicecore-docker-nms4-deploy` apply the change.

Stable release flow:

1. Merge server-side changes to `main`.
2. Confirm CI passes.
3. Create a release tag such as `v1.2.3`.
4. Confirm the Container workflow publishes `ghcr.io/hshimomura/home-metrics:v1.2.3`.
5. Confirm whether the change needs a DB migration.
6. If migration is required, make it explicit in the release note and confirm it manually
   before the `servicecore` deploy.
7. Update `ioslab-docs/servicecore` to use the digest resolved from the release tag.
8. Let `servicecore-docker-check` and `servicecore-docker-nms4-deploy` apply the change.

## API Contract

`RoomPlus` must follow the API contract published by `home-metrics`.

The API contract format is not finalized yet. The target direction is:

- maintain `docs/openapi.yaml` in this repository
- lint `docs/openapi.yaml` in CI
- publish the schema as a release artifact
- let `RoomPlus` validate its handwritten API client against that schema
- keep breaking API changes behind a coordinated server/client release

Generated Swift client code is not used for now. Revisit generation after the API surface
grows enough that the handwritten client becomes costly to maintain.

## Current Operational Scope

The current server scope is sensor readings, energy readings, and collector
status. Alarm rules, APNs push notifications, admin webhook delivery, and
maintenance mode were removed. Migration `0008_drop_alarm_features.sql` drops
the old alarm/APNs/webhook tables and device maintenance columns, so deployments
that have not yet applied it should treat it as a deliberate schema cleanup.

Cisco Spaces raw event storage has also been retired. Migration
`0011_drop_cisco_spaces_raw_events.sql` drops the existing
`cisco_spaces_raw_events` table, which deletes any raw firehose payloads kept for
debug/replay. After this migration, Cisco Spaces collection stores only
normalized `sensor_minute` readings and `collector_status`; raw event export and
replay are no longer part of the operational model.

Xiaomi Flower Care / MiFlora style plant sensors are supported through Cisco
Sensor Connect advertisement telemetry. Migration
`0012_add_plant_sensor_metrics.sql` adds `soil_moisture_percent` and
`conductivity_us_cm` to `sensor_minute` and the rollup tables so plant readings
can use the same latest/series API and web UI as the existing environmental
metrics.
Flower Care battery/firmware GATT polling is deliberately not part of this
release; plant battery values remain unavailable unless a future low-frequency
GATT polling feature is added.
