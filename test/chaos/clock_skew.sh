#!/usr/bin/env bash
# Chaos: submit a payment with a transaction_date far in the future
# (beyond the configured clock-skew grace). Service must reject with 400.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

api_ready || { echo "api not ready"; exit 1; }

CUSTOMER_ID="CHAOS_SKEW_$(date +%s)"
docker exec -i -e PGPASSWORD=postgres "$PG_CONTAINER" psql -h localhost -U postgres -d paybook -v ON_ERROR_STOP=1 <<SQL >/dev/null
INSERT INTO customers (id) VALUES ('$CUSTOMER_ID');
INSERT INTO deployments (customer_id, value_kobo, term_weeks, current_balance_kobo, started_at)
VALUES ('$CUSTOMER_ID', 100000000, 50, 100000000, now());
SQL

# Future date: 10 days ahead.
future_date=$(date -u -v+10d +"%Y-%m-%d %H:%M:%S" 2>/dev/null || date -u -d "+10 days" +"%Y-%m-%d %H:%M:%S")

body=$(jq -n -c --arg c "$CUSTOMER_ID" --arg r "$(random_ref)" --arg d "$future_date" \
    '{customer_id:$c, payment_status:"COMPLETE", transaction_amount:"1000", transaction_date:$d, transaction_reference:$r}')

status=$(sign_and_post "$body")
echo "[chaos] future-dated payment status: $status (expected 400)"
[[ "$status" == "400" ]] || { echo "expected 400, got $status"; exit 1; }

# Ensure nothing persisted.
cnt=$(docker exec -i -e PGPASSWORD=postgres "$PG_CONTAINER" psql -h localhost -U postgres -d paybook -tAc \
    "SELECT COUNT(*) FROM payments WHERE customer_id = '$CUSTOMER_ID'")
echo "[chaos] payments persisted for skewed request: $cnt (expected 0)"
[[ "$cnt" == "0" ]] || { echo "validation error left a payment row"; exit 1; }

echo "[chaos] PASS: clock_skew"
