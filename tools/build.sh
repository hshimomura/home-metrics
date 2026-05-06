#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

build_dir="${BUILD_DIR:-build}"
go_cache="${GOCACHE:-$PWD/.cache/go-build}"
go_mod_cache="${GOMODCACHE:-$PWD/.cache/go-mod}"

bins=(
  hm-ble-collector
  hm-db-check
  hm-alert-worker
  hm-api-server
  hm-db-maint
  hm-nature-remo-collector
  hm-echonet-collector
  hm-apcupsd-collector
  hm-energy-influx-import
)

mkdir -p "$build_dir" "$go_cache" "$go_mod_cache"

for bin in "${bins[@]}"; do
  echo "building $build_dir/$bin"
  GOCACHE="$go_cache" GOMODCACHE="$go_mod_cache" go build -o "$build_dir/$bin" "./cmd/$bin"
done
