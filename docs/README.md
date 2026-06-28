# Documentation

The documentation is organized by responsibility. Runtime behavior in the Go
code, `db/schema.sql`, `compose.yaml`, and `docs/openapi.yaml` is authoritative.

| Document | Purpose |
| --- | --- |
| [Architecture](architecture.md) | Current ingestion, ownership, storage, rollup, and health design. |
| [API](api.md) | Implemented REST behavior and authentication. |
| [OpenAPI](openapi.yaml) | Machine-readable API contract checked by CI. |
| [Client contract](client-contract.md) | Stable device metadata and sensor metric semantics for clients. |
| [Release and deployment](release.md) | Image publication, migrations, servicecore deployment, and verification. |
| [Xiaomi Flower Care](xiaomi-flower-care-cisco-sensor-connect.md) | Implemented FE95 decoding and optional GATT operation. |

Historical implementation plans and completed downstream UI plans are not kept
as active documentation. Git history remains the source for those decisions.
Grafana dashboards and RoomPlus implementation details belong to their own
deployment or application repositories.
