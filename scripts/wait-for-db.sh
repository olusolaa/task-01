#!/usr/bin/env bash
set -euo pipefail

URL="${1:-postgres://postgres:postgres@localhost:5432/paybook?sslmode=disable}"
DEADLINE=$((SECONDS + 30))

while (( SECONDS < DEADLINE )); do
  if psql "$URL" -c 'SELECT 1' >/dev/null 2>&1; then
    echo "postgres ready"
    exit 0
  fi
  sleep 1
done

echo "postgres did not become ready within 30s" >&2
exit 1
