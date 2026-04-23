#!/usr/bin/env bash
# Chaos: kill postgres mid-load and verify the service fails cleanly,
# recovers when postgres returns, and has no half-applied balances.
#
# Requires a running stack:
#   - api reachable at $BASE_URL
#   - postgres in docker container $PG_CONTAINER (default: paybook-pg)
#
# Exits non-zero if any expected behavior fails.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

echo "[chaos] preflight: api ready at $BASE_URL"
api_ready || { echo "api not ready; start the stack first"; exit 1; }

# Use a fresh test customer so we can assert balance cleanly.
CUSTOMER_ID="CHAOS_DB_KILL_$(date +%s)"
echo "[chaos] seeding customer=$CUSTOMER_ID"
docker exec -i -e PGPASSWORD=postgres "$PG_CONTAINER" psql -h localhost -U postgres -d paybook -v ON_ERROR_STOP=1 <<SQL
INSERT INTO customers (id) VALUES ('$CUSTOMER_ID');
INSERT INTO deployments (customer_id, value_kobo, term_weeks, current_balance_kobo, started_at)
VALUES ('$CUSTOMER_ID', 100000000, 50, 100000000, now() - '1 day'::interval);
INSERT INTO virtual_accounts (va_number, deployment_id)
SELECT 'VA$CUSTOMER_ID', id FROM deployments WHERE customer_id = '$CUSTOMER_ID';
SQL

# Apply a few payments to establish baseline.
echo "[chaos] applying 5 baseline payments (10,000 kobo each)"
for i in $(seq 1 5); do
    body=$(jq -n -c --arg c "$CUSTOMER_ID" --arg r "$(random_ref)" --arg d "$(now_utc)" \
        '{customer_id:$c, payment_status:"COMPLETE", transaction_amount:"10000", transaction_date:$d, transaction_reference:$r}')
    status=$(sign_and_post "$body")
    [[ "$status" == "201" ]] || { echo "baseline $i failed: $status"; exit 1; }
done

# Read baseline balance.
BASELINE=$(docker exec -i -e PGPASSWORD=postgres "$PG_CONTAINER" psql -h localhost -U postgres -d paybook -tAc \
    "SELECT current_balance_kobo FROM deployments WHERE customer_id = '$CUSTOMER_ID'")
echo "[chaos] baseline balance after 5 payments: $BASELINE (expected 99950000)"
[[ "$BASELINE" == "99950000" ]] || { echo "baseline mismatch"; exit 1; }

# Kill postgres.
echo "[chaos] stopping $PG_CONTAINER"
docker stop "$PG_CONTAINER" >/dev/null

# Hit api while postgres is down. Expect 503 on /readyz and 5xx on /payments.
echo "[chaos] probing api while postgres is down"
FAIL_COUNT=0
for i in $(seq 1 10); do
    body=$(jq -n -c --arg c "$CUSTOMER_ID" --arg r "$(random_ref)" --arg d "$(now_utc)" \
        '{customer_id:$c, payment_status:"COMPLETE", transaction_amount:"5000", transaction_date:$d, transaction_reference:$r}')
    status=$(sign_and_post "$body")
    if [[ "$status" != "201" ]]; then
        FAIL_COUNT=$((FAIL_COUNT + 1))
    fi
done
echo "[chaos] during outage: ${FAIL_COUNT}/10 requests failed (expected 10)"
[[ "$FAIL_COUNT" == "10" ]] || { echo "expected all 10 to fail during db outage"; exit 1; }

# Verify /healthz (liveness) still returns 200 even though DB is gone.
if api_up_only; then
    echo "[chaos] liveness /healthz: OK while DB down (correct)"
else
    echo "[chaos] liveness /healthz failed during outage"
    exit 1
fi

# Bring postgres back.
echo "[chaos] restarting $PG_CONTAINER"
docker start "$PG_CONTAINER" >/dev/null
for i in $(seq 1 30); do
    if docker exec -e PGPASSWORD=postgres "$PG_CONTAINER" pg_isready -U postgres -d paybook >/dev/null 2>&1; then
        echo "[chaos] postgres ready after ${i}s"
        break
    fi
    sleep 1
done

# Give pgx pool a moment to re-establish.
sleep 3

# Verify a fresh payment succeeds post-recovery.
echo "[chaos] applying one payment after recovery"
for i in $(seq 1 10); do
    body=$(jq -n -c --arg c "$CUSTOMER_ID" --arg r "$(random_ref)" --arg d "$(now_utc)" \
        '{customer_id:$c, payment_status:"COMPLETE", transaction_amount:"7777", transaction_date:$d, transaction_reference:$r}')
    status=$(sign_and_post "$body")
    if [[ "$status" == "201" ]]; then
        echo "[chaos] recovery succeeded on attempt $i"
        break
    fi
    sleep 1
    [[ "$i" == "10" ]] && { echo "did not recover after 10 attempts"; exit 1; }
done

# Verify balance integrity: stored == value - SUM(applied).
FINAL=$(docker exec -i -e PGPASSWORD=postgres "$PG_CONTAINER" psql -h localhost -U postgres -d paybook -tAc \
    "SELECT d.current_balance_kobo,
            d.value_kobo - COALESCE(SUM(p.amount_kobo) FILTER (WHERE p.result = 'APPLIED'), 0) AS computed
     FROM deployments d
     LEFT JOIN payments p ON p.deployment_id = d.id
     WHERE d.customer_id = '$CUSTOMER_ID'
     GROUP BY d.id")
STORED=$(echo "$FINAL" | cut -d'|' -f1)
COMPUTED=$(echo "$FINAL" | cut -d'|' -f2)
echo "[chaos] final: stored=$STORED computed=$COMPUTED"
[[ "$STORED" == "$COMPUTED" ]] || { echo "DRIFT detected: stored != computed"; exit 1; }

echo "[chaos] PASS: db_kill"
