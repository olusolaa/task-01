#!/usr/bin/env bash
# End-to-end demo. Hits a running service and asserts each expected
# outcome. Exits non-zero on the first surprise.
#
# Requires the service reachable at $BASE_URL and postgres at
# $PG_CONTAINER (default localhost + paybook-pg). Run `make up` first
# (or start the binary against the bare postgres container).

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8085}"
HMAC_SECRET="${HMAC_SECRET:-dev_secret_change_in_production}"

sign() {
    local body="$1"
    printf '%s' "$body" | openssl dgst -sha256 -hmac "$HMAC_SECRET" -hex | awk '{print $NF}'
}

post() {
    local body="$1"
    local sig
    sig=$(sign "$body")
    curl -s -o /tmp/demo_body -w "%{http_code}" \
        -X POST \
        -H "Content-Type: application/json" \
        -H "X-Signature: $sig" \
        --data-raw "$body" \
        "$BASE_URL/payments"
}

expect() {
    local label="$1" got="$2" want="$3"
    if [[ "$got" != "$want" ]]; then
        echo "FAIL: $label - got $got, want $want"
        echo "body: $(cat /tmp/demo_body 2>/dev/null)"
        exit 1
    fi
    printf "  ok  %-50s %s\n" "$label" "$got"
}

ref() { echo "VPAYDEMO$(head -c 12 /dev/urandom | xxd -p)"; }
now() { date -u +"%Y-%m-%d %H:%M:%S"; }

echo "[demo] base=$BASE_URL"

# Wait for readiness.
for i in $(seq 1 30); do
    if curl -fs "$BASE_URL/readyz" >/dev/null 2>&1; then
        echo "  api ready"
        break
    fi
    sleep 1
    [[ "$i" == "30" ]] && { echo "api not ready at $BASE_URL"; exit 1; }
done

# ---------------------------------------------------------------------------
# Scenario 1: happy path apply
# ---------------------------------------------------------------------------
ref1=$(ref)
body1=$(jq -n -c --arg r "$ref1" --arg d "$(now)" \
    '{customer_id:"GIG00001", payment_status:"COMPLETE", transaction_amount:"10000", transaction_date:$d, transaction_reference:$r}')
status=$(post "$body1")
expect "happy path apply"                       "$status" "201"
first_body=$(cat /tmp/demo_body)

# ---------------------------------------------------------------------------
# Scenario 2: replay returns byte-identical response with header
# ---------------------------------------------------------------------------
status=$(post "$body1")
expect "replay status"                          "$status" "201"
second_body=$(cat /tmp/demo_body)
if [[ "$first_body" != "$second_body" ]]; then
    echo "FAIL: replay body differs"
    echo "  first:  $first_body"
    echo "  second: $second_body"
    exit 1
fi
printf "  ok  %-50s byte-identical\n" "replay body"

# Header check
sig=$(sign "$body1")
hdr=$(curl -s -o /dev/null -D - -X POST \
    -H "Content-Type: application/json" -H "X-Signature: $sig" \
    --data-raw "$body1" "$BASE_URL/payments" | grep -i "Idempotent-Replayed" | tr -d '\r')
if ! echo "$hdr" | grep -q "true"; then
    echo "FAIL: missing Idempotent-Replayed: true header"; exit 1
fi
printf "  ok  %-50s present\n" "Idempotent-Replayed header"

# ---------------------------------------------------------------------------
# Scenario 3: overpayment -> 409
# ---------------------------------------------------------------------------
body=$(jq -n -c --arg r "$(ref)" --arg d "$(now)" \
    '{customer_id:"GIG00001", payment_status:"COMPLETE", transaction_amount:"999999999999", transaction_date:$d, transaction_reference:$r}')
status=$(post "$body")
expect "overpayment"                            "$status" "409"

# ---------------------------------------------------------------------------
# Scenario 4: non-COMPLETE status recorded but not applied -> 202
# ---------------------------------------------------------------------------
body=$(jq -n -c --arg r "$(ref)" --arg d "$(now)" \
    '{customer_id:"GIG00001", payment_status:"PENDING", transaction_amount:"1000", transaction_date:$d, transaction_reference:$r}')
status=$(post "$body")
expect "non-COMPLETE recorded"                  "$status" "202"

# ---------------------------------------------------------------------------
# Scenario 5: unknown customer -> 404
# ---------------------------------------------------------------------------
body=$(jq -n -c --arg r "$(ref)" --arg d "$(now)" \
    '{customer_id:"GIG_DOES_NOT_EXIST", payment_status:"COMPLETE", transaction_amount:"1000", transaction_date:$d, transaction_reference:$r}')
status=$(post "$body")
expect "unknown customer"                       "$status" "404"

# ---------------------------------------------------------------------------
# Scenario 6: invalid amount -> 400
# ---------------------------------------------------------------------------
body=$(jq -n -c --arg r "$(ref)" --arg d "$(now)" \
    '{customer_id:"GIG00001", payment_status:"COMPLETE", transaction_amount:"10.555", transaction_date:$d, transaction_reference:$r}')
status=$(post "$body")
expect "invalid amount (sub-kobo 10.555)"       "$status" "400"

# ---------------------------------------------------------------------------
# Scenario 7: bad HMAC -> 401
# ---------------------------------------------------------------------------
ref_bad=$(ref)
body=$(jq -n -c --arg r "$ref_bad" --arg d "$(now)" \
    '{customer_id:"GIG00001", payment_status:"COMPLETE", transaction_amount:"1000", transaction_date:$d, transaction_reference:$r}')
status=$(curl -s -o /tmp/demo_body -w "%{http_code}" \
    -X POST \
    -H "Content-Type: application/json" \
    -H "X-Signature: deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef" \
    --data-raw "$body" "$BASE_URL/payments")
expect "bad HMAC"                               "$status" "401"

# ---------------------------------------------------------------------------
# Scenario 8: balance endpoint
# ---------------------------------------------------------------------------
status=$(curl -s -o /tmp/demo_body -w "%{http_code}" "$BASE_URL/customers/GIG00001/balance")
expect "balance 200"                            "$status" "200"
drift=$(jq -r '.deployments[0].drift_naira' /tmp/demo_body)
if [[ "$drift" != "0.00" ]]; then
    echo "FAIL: drift=$drift on GIG00001 after happy-path apply"
    exit 1
fi
printf "  ok  %-50s %s\n" "balance drift" "$drift"

echo
echo "[demo] ALL SCENARIOS PASSED"
