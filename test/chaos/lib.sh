#!/usr/bin/env bash
# Shared helpers for chaos scripts. Source this file at the top.

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
HMAC_SECRET="${HMAC_SECRET:-dev_secret_change_in_production}"
PG_CONTAINER="${PG_CONTAINER:-paybook-pg}"
API_CONTAINER="${API_CONTAINER:-paybook-api}"

# Post a signed payment. Echoes the http status to stdout.
sign_and_post() {
    local body="$1"
    local sig
    sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$HMAC_SECRET" -hex | awk '{print $NF}')
    curl -s -o /dev/null -w "%{http_code}" \
        -X POST \
        -H "Content-Type: application/json" \
        -H "X-Signature: $sig" \
        --data-raw "$body" \
        "$BASE_URL/payments" || echo "000"
}

random_ref() {
    local suffix
    suffix=$(head -c 16 /dev/urandom | xxd -p)
    echo "VPAYCHAOS${suffix}"
}

now_utc() {
    date -u +"%Y-%m-%d %H:%M:%S"
}

api_ready() {
    local deadline=$((SECONDS + 30))
    while (( SECONDS < deadline )); do
        if curl -fs "$BASE_URL/readyz" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

api_up_only() {
    # Liveness without readiness: /healthz doesn't touch DB.
    curl -fs "$BASE_URL/healthz" >/dev/null 2>&1
}
