#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
for sql in db/migrations/*.sql; do cat "$sql" >> "$tmp"; printf '\n' >> "$tmp"; done
cat >> "$tmp" <<'SQL'
CREATE TABLE IF NOT EXISTS schema_migrations (
    version bigint PRIMARY KEY,
    name text NOT NULL,
    checksum text NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
);
SQL
for sql in db/migrations/*.sql; do
    base=$(basename "$sql" .sql)
    version=$(printf '%s' "${base%%_*}" | sed 's/^0*//')
    name=${base#*_}
    checksum=$(shasum -a 256 "$sql" | cut -d ' ' -f 1)
    printf "INSERT INTO schema_migrations (version, name, checksum) VALUES (%s, '%s', '%s');\n" "$version" "$name" "$checksum" >> "$tmp"
done
cat "$tmp" > db/schema.sql
