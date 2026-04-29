# paybook

A payment-ingest service for a productive-asset financing platform.

Mobility entrepreneurs receive an asset valued at ₦1,000,000 over 50 weeks. They earn with it and pay back into a virtual account issued at deployment. Every successful transfer reduces the customer's outstanding balance. This service is the HTTP endpoint that applies those payments.

For the full reasoning behind what I built and why, start with [APPROACH.md](APPROACH.md). For the domain decisions, see the three [ADRs](docs/adr/). For a walk through the race conditions that idempotency has to survive, see [docs/replay-safety.md](docs/replay-safety.md).

## What this is

- One Go binary, one Postgres, no message broker, no cache, no Kubernetes.
- Handles the assignment's 100,000 notifications/minute (≈1,667 TPS) well inside the steady-state envelope of a single Postgres instance.
- Idempotent by construction: duplicate `transaction_reference` returns the original response byte-for-byte.
- Append-only ledger; the database role the service runs as has no `DELETE` or `TRUNCATE` grant.
- Six property-based invariants proven with `pgregory.net/rapid`.
- Chaos scripts that kill the database mid-traffic and assert integrity holds.

## Run it in three commands

```bash
make up         # docker compose: postgres + migrations + seed + api + prometheus
make demo       # end-to-end shell check; asserts every expected outcome
make test       # unit + integration + property, all under -race
```

Stack comes up on:
- `http://localhost:8080` — public API
- `http://localhost:9090` — `/metrics`
- `http://localhost:9091` — prometheus UI

Shut down with `make down`.

## HTTP API

### `POST /payments`

Signed by `X-Signature: hex(HMAC-SHA256(HMAC_SECRET, body))`.

```json
{
  "customer_id": "GIG00001",
  "payment_status": "COMPLETE",
  "transaction_amount": "10000",
  "transaction_date": "2025-11-07 14:54:16",
  "transaction_reference": "VPAY25110713542114478761522000"
}
```

Responses:

| Status | Body shape                                                                 | When |
|-------:|---|---|
|    201 | `{"status":"applied","transaction_reference":...,"amount_applied_naira":"10000.00","balance_after_naira":"990000.00",...}` | Fresh payment applied. |
|    201 | same bytes as original, with `Idempotent-Replayed: true` header            | Duplicate `transaction_reference`. |
|    202 | `{"status":"recorded","payment_status":"PENDING",...}`                     | `payment_status` other than `COMPLETE`. Recorded for audit, balance untouched. |
|    400 | `{"error":"invalid_amount"\|"invalid_date"\|"invalid_status"\|"invalid_payload"}` | Validation failed. |
|    401 | `{"error":"missing_signature"\|"invalid_signature"}`                       | HMAC verification failed; nothing persisted. |
|    404 | `{"error":"customer_not_found"\|"no_active_deployment"}`                   | Routing failed. |
|    409 | `{"error":"overpayment","outstanding_naira":"50000.00"}`                    | Amount exceeds current balance. |
|    409 | `{"error":"deployment_inactive","deployment_state":"FULLY_REPAID"}`         | Deployment not in `ACTIVE` state. |

### `GET /customers/{id}/balance`

Returns stored balance, computed balance (recomputed from the ledger), and the drift between them. In a correct system the drift is always zero; if it isn't, the endpoint surfaces it rather than hiding it.

```json
{
  "customer_id": "GIG00001",
  "deployments": [
    {
      "deployment_id": "...",
      "state": "ACTIVE",
      "value_naira": "1000000.00",
      "stored_balance_naira": "999750.00",
      "computed_balance_naira": "999750.00",
      "drift_naira": "0.00"
    }
  ]
}
```

### `GET /healthz`, `GET /readyz`, `GET /metrics`

Liveness, readiness (pings DB), and prometheus metrics respectively. Metrics is served on its own port so scrapers don't share the public listener.

## Approach in one screen

Every request runs this pipeline inside a single READ COMMITTED transaction:

1. **Cheap replay check**: `SELECT response_body FROM payments WHERE transaction_reference = $1`. If it hits, return those bytes and commit. Most replays end here without touching the deployment row.
2. **Lock the routing deployment**: `SELECT ... FROM deployments WHERE customer_id = $1 ORDER BY (state='ACTIVE') DESC, started_at ASC LIMIT 1 FOR UPDATE`. Serialises same-customer payments through the row lock.
3. **Decide**: a pure function over `(payment, deployment)` produces a `result` (APPLIED, RECORDED, REJECTED), an HTTP status, and a response body.
4. **Insert with `ON CONFLICT DO NOTHING RETURNING id`**: if another transaction beat us to the same `transaction_reference`, the RETURNING is empty and we re-read the stored response and return it.
5. **If APPLIED**, update `current_balance_kobo` (and transition state to `FULLY_REPAID` when it hits zero) in the same transaction.
6. Commit. Write the stored bytes to the response writer.

## Test evidence

Unit, integration, property, and chaos tests are layered. Each one exists because it proves something the others do not.

| Tier | What it proves | How to run |
|---|---|---|
| `internal/money` unit tests | 25-case strict parser rejects decimals, scientific, leading zeros, signs, whitespace, out-of-range | `make test-unit` |
| `internal/config` unit tests | Startup fails loudly on missing or malformed env | `make test-unit` |
| `internal/payments` unit tests | `decide()` is pure and covers every branch of the policy | `make test-unit` |
| `test/integration` — 12 scenarios | End-to-end via httptest: happy path, idempotent replay, 100 concurrent duplicates ending in exactly one applied, 50 concurrent different-refs converging to the correct balance, overpayment recorded as rejected, non-COMPLETE recorded for audit, HMAC rejection persists nothing, all eight invalid-amount variants, full 50-week repayment transitioning to FULLY_REPAID, balance endpoint reporting drift, deliberate cache poisoning detected | `make test` |
| `test/property` — six rapid invariants | Random sequences preserve `stored == value − Σ(applied)`; balance always in `[0, value]`; replay is byte-identical across up to 10 replays; under 10-150 concurrent workers with the same ref, exactly one row lands; HMAC rejections are side-effect-free; splitting `value_kobo` into random chunks always ends in state FULLY_REPAID | `make property` |
| `test/chaos` | DB killed mid-traffic, request-path fails cleanly, recovery is clean, stored == computed; future-dated payment returns 400 without persisting; app↔DB network partition and clean recovery | `make chaos-db-kill` etc. |
| `load/` | k6: 2,000 TPS steady and replay-storm scenarios | `make load-steady`, `make load-burst` |

`make test` runs the first five tiers with `-race` and will fail on any race condition. CI runs the same pass against a service-container Postgres on every push.

## Decisions I pushed back on

1. **`int64` kobo over `NUMERIC(20,4)`.** The safe-looking default was wrong for this workload: it added a dependency, widened the row, and gave nothing back. A named `Kobo` type catches float contamination at compile time. [ADR 003](docs/adr/003-int64-kobo-over-numeric.md).
2. **Virtual account as a first-class primitive even though the payload carries `customer_id`.** The payload shape does not change the domain shape. Routing by customer without modeling the VA would bake the wrong mental model into the schema. [ADR 001](docs/adr/001-virtual-account-as-routing-primitive.md).
3. **Stored replay response instead of reconstruction.** The cheap path is to rebuild the response on replay from payment fields. It rots the moment anyone changes response shape. Storing 400 bytes per row for byte-identical replay across every future business-logic change is worth it. [ADR 002](docs/adr/002-single-entry-ledger-with-stored-replay.md).

## What is deliberately not built

Listed so the absence is visible:

- Kafka / NATS / any broker. Narrated as the evolution path in [architecture.md](docs/architecture.md), not implemented.
- Kubernetes manifests, multi-region, any cloud-specific deploy.
- gRPC; the brief asked for REST.
- Full OpenAPI spec; the `POST /payments` shape is simple enough that a curl example carries the weight.
- Reversal / chargeback flow. `REVERSED` is declared in the payment_status enum for forward-compat but the handler does not implement the reversal effect. This needs its own design.
- Fees, commissions, multi-currency, customer onboarding, deployment creation, admin / ops dashboards.
- Rate limiting per source IP. HMAC is the webhook gate; ingress-level rate limiting is the right layer for this control.
- Multi-key HMAC rotation. The current wiring expects a single active secret. Rotation is a documented known limitation.

## Layout

```
APPROACH.md                 Read this first
README.md                   You are here
Makefile                    up, down, test, load-*, chaos-*, demo, build
Dockerfile                  multi-stage, distroless
docker-compose.yml          postgres + migrate + api + prometheus
cmd/api/main.go             60 lines of explicit wiring
internal/
  config/                   env loader, fail-fast
  money/                    Kobo newtype + strict parser
  payments/                 handler/service/repo/model/errors
  reconciliation/           stored vs computed balance
  server/                   router, middleware (hmac, request-id, metrics, recover), handlers
migrations/0001_initial.sql schema + least-privilege grants
scripts/                    init-roles, seed, demo, wait-for-db, prometheus.yml
test/
  integration/              end-to-end http scenarios
  property/                 rapid-based invariants
  chaos/                    failure-mode shell scripts
load/
  steady.js                 k6 steady-state
  burst.js                  k6 replay-storm
docs/
  adr/                      three load-bearing decisions
  architecture.md           request flow, schema, scale path
  runbook.md                incident triage
  slo.md                    targets and alerts
  replay-safety.md          idempotency proof walked through the races
```

## Interfaces: where they are and why

Interfaces are defined where they earn their keep, not at every layer:

- **`PaymentsService` and `ReconciliationService`** ([internal/server/interfaces.go](internal/server/interfaces.go)) — the handler's view of the business surface. Defined in the *consumer* package (`server`), not the producer, per Go's "accept interfaces, return structs" idiom. They buy two things: handler unit tests that substitute a fake and don't need a database ([payments_handler_test.go](internal/server/payments_handler_test.go) has six of them), and a clean seam for a future second implementation (e.g. the Kafka-buffered variant narrated in the scale path).
- **`Querier`** ([payments/repo.go](internal/payments/repo.go)) — pgx's `Tx` and `pgxpool.Pool` both satisfy it, so every repo method works inside or outside a transaction without duplicate signatures.
- **`Clock`** ([payments/service.go](internal/payments/service.go)) — lets the clock-skew validator be tested deterministically against a fixed time.

What's *not* extracted: there is no `PaymentsRepository` interface, no `Storer`, no `Logger` abstraction, no `mocks/` directory. The repo is a concrete `*Repo` because only one implementation exists, the service-to-repo path is covered by integration tests against real Postgres, and abstracting over a single implementation adds indirection without testability or flexibility gain. If a second repo appears, the interface appears with it.

This is a judgment call. Teams that enforce hexagonal/ports-and-adapters strictly will want more interfaces; teams that read Cheney on "don't mock what you don't own" will read this the same way. The trade-off is stated here so a reviewer can challenge it on merit, not guess at intent.

## Dependencies

`go.mod` pulls five modules worth naming:
- `github.com/jackc/pgx/v5` — the Postgres driver. Chosen over `database/sql` for richer types, connection tracing, and prepared-statement caching by default.
- `github.com/jackc/pgx/v5/pgxpool` — connection pool.
- `github.com/prometheus/client_golang` — metrics.
- `pgregory.net/rapid` — property-based testing (dev only).
- stdlib for everything else: `slog`, `net/http`, `encoding/json`, `crypto/hmac`, `crypto/sha256`.
