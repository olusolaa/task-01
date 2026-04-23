# Runbook

Short, scannable responses for the incidents this service can produce.

## Alert: drift on `/customers/{id}/balance`

**What it means.** The reconciliation endpoint returned a non-zero `drift_kobo`. The stored cache disagrees with the payment ledger. The ledger is authoritative.

**Is it user-visible?** No immediate impact — balances shown to customers use the stored cache, which is internally consistent within a transaction. Drift is an integrity concern, not a request-path failure.

**Triage.**
1. Query the row directly:
   ```sql
   SELECT d.id, d.current_balance_kobo,
          d.value_kobo - COALESCE(SUM(p.amount_kobo) FILTER (WHERE p.result='APPLIED'), 0) AS computed
   FROM deployments d
   LEFT JOIN payments p ON p.deployment_id = d.id
   WHERE d.id = '<deployment_id>'
   GROUP BY d.id;
   ```
2. Inspect recent payments for manual tampering:
   ```sql
   SELECT id, amount_kobo, result, received_at, applied_balance_kobo
   FROM payments WHERE deployment_id = '<deployment_id>'
   ORDER BY received_at DESC LIMIT 20;
   ```
3. If drift is due to a manual SQL `UPDATE` (check `pg_stat_activity` audit logs), the computed value is truth. Restore the stored cache:
   ```sql
   UPDATE deployments SET current_balance_kobo = <computed>
   WHERE id = '<deployment_id>';
   ```

**Root cause investigation.** Drift should never happen through the service. If it did, look for:
- Manual DB intervention without a corresponding ledger entry
- Postgres-level replication lag on the stored column if a replica was written to
- A service bug — if so, file an incident and add a property test that reproduces

## Alert: `/readyz` returning 503

**What it means.** The api cannot reach postgres.

**Triage.**
1. Check postgres container status: `docker ps | grep postgres` or equivalent.
2. Check api logs: `docker logs paybook-api 2>&1 | grep -i "db\|pool\|connect"`.
3. Inspect `pg_stat_activity` from a separate shell for long-running transactions blocking the pool.
4. If postgres is up but unreachable: test DNS / connectivity from the api container. Network partition.
5. If postgres is down: start it; pgx will reconnect on its own within seconds.

**Request-path effect.** Load balancer removes the pod from rotation. Clients receive 503s on `POST /payments` until readiness returns. No corruption: any in-flight request that lost its DB connection rolled back.

## Alert: HMAC rejection rate spike

**What it means.** A spike in `paybook_http_requests_total{path="/payments",status="401"}`. Either the bank rotated a secret without telling us, or someone is probing.

**Triage.**
1. Check request source IPs against the bank's known webhook egress.
2. Check recent deploys: did `HMAC_SECRET` change?
3. Coordinate with the bank's integrations team.

**Remediation.** Support multi-key validation during rotation: accept either the old or new secret for the cutover window (not currently implemented; flagged as out-of-scope in the README).

## Incident: overpayment returning 409

**What it means.** A bank transfer arrived for an amount that exceeds the remaining balance. This is a real business event, not a bug.

**What the service did.** Recorded the payment with `result = REJECTED`, `reject_reason = 'overpayment'`, returned 409 with the outstanding balance in the response. The money stays in the customer's virtual account at the bank.

**Next step.** Ops reconciliation: either refund to the customer's source account or apply to a new deployment if one has been started. That flow is out of this service's scope.

## Incident: deployment fully repaid

**What it means.** A payment hit zero balance. The deployment transitioned to `FULLY_REPAID` and `closed_at` was set. Future payments for this customer return 409 `deployment_inactive`.

**Expected.** This is the success path. A follow-up business process (ownership transfer, new deployment onboarding) is triggered outside this service.

## Operational tasks

### Rotating the HMAC secret
Currently a single-secret design. Rotation requires a brief window where the bank holds off on sending, the secret is replaced in configuration, and the api is restarted. Documented as a known limitation; multi-key support is listed under "Out of scope".

### Replaying a webhook manually
If ops needs to force a replay (e.g. after a known bank outage), post the same payload with the same `transaction_reference` and a correct HMAC signature. The service will return the originally stored response and no balance change will occur. This is safe by construction.

### Inspecting stored replay responses
```sql
SELECT transaction_reference, response_status, response_body::text
FROM payments
WHERE transaction_reference = '<ref>';
```
