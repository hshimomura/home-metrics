# syntax=docker/dockerfile:1

FROM golang:1.26.4-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN ./tools/build.sh

FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system home_metrics \
    && useradd --system --gid home_metrics --home-dir /var/lib/home-metrics --create-home home_metrics

WORKDIR /app

COPY --from=builder /src/build/ /usr/local/bin/
COPY --from=builder /src/web/ ./web/
COPY --from=builder /src/db/migrations/ ./db/migrations/
COPY --from=builder /src/examples/sensors.json.example /etc/home-metrics/sensors.json
COPY docker/scripts/run-db-maint-loop /usr/local/bin/run-db-maint-loop

USER home_metrics:home_metrics

CMD ["hm-api-server"]
