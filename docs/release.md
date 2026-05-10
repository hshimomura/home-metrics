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

Deployment should use the digest emitted by the Container workflow.

Example:

```text
ghcr.io/hshimomura/home-metrics@sha256:<digest>
```

After the first publish, verify that the GHCR package visibility is public in GitHub package
settings. The deployment model assumes public pull access from `nms4`.

## Release Flow

1. Merge server-side changes to `main`.
2. Confirm CI passes.
3. Create a release tag such as `v1.2.3`.
4. Confirm the Container workflow publishes `ghcr.io/hshimomura/home-metrics:v1.2.3`.
5. Copy the published image digest.
6. Update `ioslab-docs/servicecore` to use that digest.
7. Let `servicecore-docker-check` and `servicecore-docker-nms4-deploy` apply the change.

## API Contract

`RoomPlus` must follow the API contract published by `home-metrics`.

The API contract format is not finalized yet. The target direction is:

- maintain `docs/openapi.yaml` in this repository
- lint `docs/openapi.yaml` in CI
- publish the schema as a release artifact
- let `RoomPlus` validate or generate client code from that schema
- keep breaking API changes behind a coordinated server/client release
