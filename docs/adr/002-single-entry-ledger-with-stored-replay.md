# ADR 002: Single-entry append-only ledger with stored replay response

**Status:** accepted
**Date:** 2025-11-07

## Context

Two correctness requirements drive the ledger shape:

1. **Balance must be recoverable from history.** The current balance on the deployment row is a cache. Given the payment ledger, we must be able to recompute it. This gives us audit, replay, and a live drift probe at the `/balance` endpoint.
2. **Idempotency must be byte-identical, not just semantic.** The bank delivers at-least-once. The same `transaction_reference` will arrive more than once in normal operation. Every replay must return exactly the same bytes that the first request produced, regardless of how the service's response shape evolves.

## Decision

**Append-only `payments` table.** One row per notification, never updated, never deleted. The application role is granted `INSERT, UPDATE, SELECT` — no `DELETE`, no `TRUNCATE`. The `UPDATE` grant is used only for the deployment row; the migration does not (and cannot, given the role's grants) allow ledger mutation by the service.

**Single-entry, not double-entry.** This service tracks one customer-side balance (the outstanding receivable). Double-entry becomes valuable when there are multiple accounts whose balances must agree in aggregate (cash, fees, commissions, refunds). That scope is explicitly not in this service. See *Alternatives*.

**Stored response body on every payment row.** At decide-time the service marshals the final response JSON, stores it in `response_body` (BYTEA) and `response_status` (SMALLINT), and then returns those exact bytes to the client. A replay returns the same bytes verbatim, for the lifetime of the row. `BYTEA` is chosen over `JSONB` because JSONB reparses on read, meaning key order and whitespace can change between original and replay.

**Balance cache on `deployments.current_balance_kobo`.** Updated in the same transaction as the payment insert. The `/customers/{id}/balance` endpoint recomputes from the ledger and returns both stored and computed values plus the drift, so a poisoned cache is surfaced rather than silent.

## Consequences

**Positive.** Audit-friendly by construction. Replay is trivially correct. Future reconciliation cron jobs just recompute. Refunds, when added, become new ledger rows with negative effect — schema carries over.

**Negative.** Storing the response body doubles the row size on average (a few hundred bytes per row). At 100k/minute this is ~15GB/year — negligible and worth the correctness guarantee. Rows cannot be edited to fix a bad response; we would append a compensating row instead.

## Alternatives considered

**Double-entry ledger (accounts + journal_entries + ledger_entries).** The fintech-native answer and the right choice once the service owns multiple accounts. For this scope (one receivable per deployment) it is overscoped: two rows per payment, signed sums to compute a balance, and a trial-balance invariant to enforce. If fees, refunds, or company books enter scope, the migration to double-entry is clean (the payments table becomes the journal; new tables add accounts and entries).

**Reconstruct the response on replay.** Query the payment row, infer the original response from stored fields. Bug farm: any future business-logic change that alters response shape drifts the replay. Rejected.

**Skip the balance cache; compute on every read.** Cheap to write, slow to read, and every request path would run an aggregation. Rejected; the cache is a write-time cost we pay once per payment.
