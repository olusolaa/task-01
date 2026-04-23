#!/usr/bin/env bash
# Chaos: partition the api container from postgres using docker network
# disconnect. Service must return 5xx during the partition and recover
# cleanly when the network is restored, with no half-applied balances.
#
# Requires the full docker-compose stack (both api and postgres in
# containers on the same docker network).

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

NETWORK="${NETWORK:-task-01_default}"

api_ready || { echo "api not ready"; exit 1; }

if ! docker network inspect "$NETWORK" >/dev/null 2>&1; then
    echo "network $NETWORK not found; set NETWORK=<compose_network>"
    exit 1
fi

CUSTOMER_ID="CHAOS_NET_$(date +%s)"
docker exec -e PGPASSWORD=postgres "$PG_CONTAINER" psql -h localhost -U postgres -d paybook -v ON_ERROR_STOP=1 <<SQL >/dev/null
INSERT INTO customers (id) VALUES ('$CUSTOMER_ID');
INSERT INTO deployments (customer_id, value_kobo, term_weeks, current_balance_kobo, started_at)
VALUES ('$CUSTOMER_ID', 100000000, 50, 100000000, now());
SQL

# Partition api from postgres.
echo "[chaos] disconnecting $API_CONTAINER from $NETWORK"
docker network disconnect "$NETWORK" "$API_CONTAINER" >/dev/null

sleep 2
FAIL_COUNT=0
for i in $(seq 1 5); do
    body=$(jq -n -c --arg c "$CUSTOMER_ID" --arg r "$(random_ref)" --arg d "$(now_utc)" \
        '{customer_id:$c, payment_status:"COMPLETE", transaction_amount:"1000", transaction_date:$d, transaction_reference:$r}')
    status=$(sign_and_post "$body" || echo 000)
    [[ "$status" != "201" ]] && FAIL_COUNT=$((FAIL_COUNT + 1))
done
echo "[chaos] during partition: $FAIL_COUNT/5 requests failed"

# Reconnect.
echo "[chaos] reconnecting $API_CONTAINER"
docker network connect "$NETWORK" "$API_CONTAINER" >/dev/null
sleep 3

# Recovery probe.
for i in $(seq 1 10); do
    body=$(jq -n -c --arg c "$CUSTOMER_ID" --arg r "$(random_ref)" --arg d "$(now_utc)" \
        '{customer_id:$c, payment_status:"COMPLETE", transaction_amount:"2222", transaction_date:$d, transaction_reference:$r}')
    status=$(sign_and_post "$body")
    if [[ "$status" == "201" ]]; then
        echo "[chaos] recovered on attempt $i"
        break
    fi
    sleep 1
    [[ "$i" == "10" ]] && { echo "did not recover"; exit 1; }
done

# Integrity check.
FINAL=$(docker exec -e PGPASSWORD=postgres "$PG_CONTAINER" psql -h localhost -U postgres -d paybook -tAc \
    "SELECT d.current_balance_kobo,
            d.value_kobo - COALESCE(SUM(p.amount_kobo) FILTER (WHERE p.result = 'APPLIED'), 0) AS computed
     FROM deployments d
     LEFT JOIN payments p ON p.deployment_id = d.id
     WHERE d.customer_id = '$CUSTOMER_ID'
     GROUP BY d.id")
STORED=$(echo "$FINAL" | cut -d'|' -f1)
COMPUTED=$(echo "$FINAL" | cut -d'|' -f2)
echo "[chaos] final: stored=$STORED computed=$COMPUTED"
[[ "$STORED" == "$COMPUTED" ]] || { echo "DRIFT detected"; exit 1; }

echo "[chaos] PASS: network_partition"
