# Approach

## What I understood the brief to be

Mobility entrepreneurs receive a productive asset (valued at ₦1,000,000 over 50 weeks). They earn with it and pay back into a virtual account issued at deployment. Every successful transfer reduces the outstanding balance instantly. We implement the REST API that applies an incoming payment notification to the right customer account, at scale (100,000 notifications/minute ≈ 1,667 TPS).

This is a payments-ingest service. Everything else (onboarding, deployment creation, refunds, reporting) is out of scope.

## The three load-bearing decisions

1. **Virtual account is the real routing primitive, not `customer_id`.**
   The brief says the bank pays into a *virtual account* issued at deployment. The sample payload carries `customer_id`, but the production routing key is the virtual account number (1:1 with deployment). I model `virtual_accounts` as a first-class table and document that in production the webhook would carry `va_number`; for this exercise the payload provides `customer_id` so I route by `customer_id → oldest active deployment`. This keeps the domain correct and the code honest about what was simplified.

2. **Single-entry append-only ledger with stored replay response.**
   `payments` is append-only. `deployment.current_balance_kobo` is a cache of the projection `value − Σ(applied payments)`. Idempotency comes from a unique constraint on `transaction_reference` plus the full response body stored on the payment row — so a replay is byte-identical forever, regardless of future business-logic changes. Double-entry isn't needed at this scope (no company books, no fees, no refunds); the ADR explains when it would be.

3. **Sync Postgres, no queue.**
   1.7k TPS is well inside a single Postgres instance's write envelope (5–10k small-write TPS on modest hardware). The brief says *"instantly updating the current position"* — a queue would invert that. The README describes the evolution path (Kafka partitioned by `va_number`) for when sustained traffic crosses 10k TPS or multi-region comes into scope.

## Assumptions I am flagging, not hiding

| Ambiguity | Decision | Rationale |
|---|---|---|
| Payload has `customer_id` not `deployment_id`; what if a customer has many active deployments? | Apply to oldest-active; document that VA is the real key | Aligns with the physical reality of one VA per deployment |
| `transaction_date` has no timezone | Parse as UTC, store `TIMESTAMPTZ`, reject if more than 24h in the future | Only safe default; bank contract should pin this down |
| `transaction_amount` is a string | Accept strict integer kobo only (`"10000"`); reject decimals, scientific, leading zeros, signs | The field is a string precisely so we own the semantics |
| Overpayment (amount > outstanding) | `409 overpayment`, payment recorded as `REJECTED`, balance untouched | Simplest and auditable; money sits in the VA for ops |
| Non-`COMPLETE` statuses | Recorded for audit with `result=RECORDED`, balance untouched | Keeps the audit trail honest |
| `REVERSED` is declared in the status enum but not wired | Out of scope — reversal is a separate flow that needs its own design | Flagged in "Not in scope" |

## What I deliberately do not build

- Kafka / NATS / any message broker
- Kubernetes manifests
- Multi-region / sharding
- gRPC
- Full OpenAPI spec
- A web UI
- Reversals, fees, commissions, multi-currency
- Customer/deployment onboarding endpoints
- Rate limiting per source IP (HMAC is the webhook gate)

Each of these has a paragraph in the README under *"Out of scope and why"*. Their absence is deliberate, not forgotten.

## How I prove it works

- **Integration tests** against real Postgres via `docker compose up` — apply, replay, overpayment, unknown VA, bad HMAC, graceful-shutdown-mid-write.
- **Property tests** with `pgregory.net/rapid` — six invariants including `balance == value − Σ(applied)` under random payment sequences, idempotent replay is byte-identical, balance monotonic non-negative, HMAC rejection leaves no trace.
- **Chaos scripts** — DB killed mid-load, clock skew, app↔DB network partition.
- **k6 load scenarios** — steady 2k TPS and a replay-storm (5k new + 5k duplicates interleaved at 2k TPS). Results committed under `load/results/`.
- **`make demo`** — a shell script that exercises the full API surface end-to-end against a running stack.

## How the code is shaped

```
cmd/api/main.go          thin wiring, ~60 lines
internal/config          env loader, fail-fast
internal/money           Kobo newtype, strict parser
internal/payments        handler / service / repo / model / errors
internal/reconciliation  recompute-from-ledger projection
internal/http            server + middleware (hmac, requestid, metrics, recover)
migrations/              schema + least-privilege GRANTs in-file
test/integration         testcontainers-free, uses compose postgres
test/property            rapid-based invariants
test/chaos               failure-mode matrix
load/                    k6 scenarios + committed results
docs/adr                 three decisions with trade-offs admitted
docs/                    architecture, runbook, SLO, replay-safety
```

Three packages, not ten. One binary, not three. Explicit wiring in `main.go`, no DI framework, no package-level state.

## Decisions I pushed back on

1. **Balance as `NUMERIC` vs `int64` kobo.** The obvious generic choice is `NUMERIC(20,4)` — it "handles money." I chose `int64` kobo: faster, no rounding, and the compiler catches accidental float contamination via a `Kobo` newtype. ADR 003.
2. **Idempotency by "look then insert" vs `ON CONFLICT`.** The naive flow has a race window. Using `INSERT ... ON CONFLICT DO NOTHING RETURNING id` under a row-locked deployment closes it.
3. **Reconstructing the replay response vs storing it.** Reconstructing is a bug farm — any future business-logic change drifts the response between original and replay. I store `response_status` and `response_body` JSONB on the payment row. Replay returns bytes verbatim.

That's the whole thesis. What follows is evidence.
