# Architecture

One binary. One database. No queue, no broker, no cache. At 1,667 TPS of small writes, Postgres handles this inside its steady-state envelope; adding infrastructure before it earns its keep is the first way a system becomes hard to operate.

```
                          +--------+
  bank webhook  --HMAC-->  |  api  |  --SQL tx-->  [ postgres ]
                          +--------+
                            |    |
                            |    +--scrape--> [ prometheus ]
                            |
                            +--stdout JSON--> [ log pipeline ]
```

## Request path: applying a payment

```
POST /payments
  |
  v
RequestID       <-- generate or honor X-Request-ID; echoed on response
  |
  v
Recover         <-- panic -> 500, structured log
  |
  v
Logger          <-- one line per request with status + duration
  |
  v
Metrics         <-- counter by (method, path, status) + latency histogram
  |
  v
HMAC            <-- X-Signature vs HMAC-SHA256(secret, body); 401 on miss
  |
  v
Handler         <-- JSON decode (DisallowUnknownFields)
  |
  v
Service.ValidateAndParse
  |             amount   naira string, integer or up to 2 decimals
  |             date     UTC, <= now + clock_skew_grace
  |             status   one of { COMPLETE, PENDING, FAILED, REVERSED }
  v
Service.Apply   BEGIN READ COMMITTED
  |
  +--> LookupByTxnRef          (hit? return stored response, commit, done)
  |
  +--> LockRoutingDeployment   (FOR UPDATE on the chosen deployment row)
  |                            (404 if customer unknown; prefer oldest ACTIVE)
  |
  +--> decide(payment, dep)    pure function:
  |       non-COMPLETE         -> RECORDED  + 202
  |       state != ACTIVE      -> REJECTED  + 409 deployment_inactive
  |       amount > balance     -> REJECTED  + 409 overpayment
  |       otherwise            -> APPLIED   + 201
  |                              and optional transition to FULLY_REPAID
  |                              when new balance hits zero
  |
  +--> RecordPayment           INSERT ... ON CONFLICT DO NOTHING RETURNING id
  |       conflict             -> re-lookup stored response, return as replay
  |       inserted + APPLIED   -> UPDATE deployment balance/state
  |
  v
COMMIT
  |
  v
write response  <-- exact bytes stored on the payment row
                    Idempotent-Replayed: true on replays
```

## Idempotency model

The unique constraint on `payments.transaction_reference` is the load-bearing piece. The flow is:

1. Cheap lookup without locks. If the reference exists, return the stored response (common case for replays that arrive after the original has committed).
2. Take `FOR UPDATE` on the deployment row so same-customer payments serialise through it.
3. Insert with `ON CONFLICT DO NOTHING RETURNING id`. If the insert returns a row we are the winner. If it returns nothing, another transaction beat us between steps 1 and 3 — re-read the stored response and return it.

Two concurrent replays with the same reference end up with one `INSERT` succeeding and one returning the stored response. There is no code path that double-applies a payment.

## Data model

```
customers       (id TEXT PK, created_at)
deployments     (id UUID PK,
                 customer_id TEXT FK,
                 value_kobo BIGINT,
                 term_weeks INT,
                 current_balance_kobo BIGINT,                -- cache
                 state deployment_state,
                 started_at, closed_at, created_at)
virtual_accounts(va_number TEXT PK,
                 deployment_id UUID UNIQUE FK)               -- 1:1 with deployment
payments        (id UUID PK,
                 transaction_reference TEXT UNIQUE,          -- idempotency key
                 customer_id TEXT FK,
                 deployment_id UUID FK,
                 amount_kobo BIGINT,
                 status payment_status,
                 result application_result,
                 reject_reason TEXT,                         -- iff REJECTED
                 response_status SMALLINT,                   -- stored replay
                 response_body BYTEA,                        -- stored replay
                 transaction_date TIMESTAMPTZ,
                 received_at TIMESTAMPTZ,
                 applied_balance_kobo BIGINT)                -- iff APPLIED
```

Check constraints enforce the invariants that belong at the storage layer:
- `amount_kobo > 0`
- `current_balance_kobo BETWEEN 0 AND value_kobo`
- `(state = 'ACTIVE') = (closed_at IS NULL)`
- `(result = 'APPLIED') = (applied_balance_kobo IS NOT NULL)`
- `(result = 'REJECTED') = (reject_reason IS NOT NULL)`

The application role (`paybook_app`) gets a narrow grant set, table by table:

```
customers         SELECT          (existence check)
deployments       SELECT, UPDATE  (routing lookup; balance + state transition)
virtual_accounts  SELECT          (not read in the current code path)
payments          SELECT, INSERT  (append-only from the app's point of view)
```

No `DELETE`, no `TRUNCATE`, no `DROP`, no `UPDATE` on `payments`. The ledger is immutable by the database, not by convention.

## Scale path

The brief specifies 100,000 notifications/minute (≈1.7k TPS). A single modest Postgres handles 5-10k small-write TPS comfortably, so the sync architecture fits with headroom.

If sustained traffic ever crosses ~10k TPS or multi-region becomes scope:

1. **Kafka in front.** Partition by `va_number` (or `customer_id`) so same-account notifications stay ordered. The apply worker reads a partition and performs the same atomic write. `transaction_reference` uniqueness still prevents duplicates. Service trades latency for throughput and burst absorption.
2. **Shard Postgres.** Hash partition by `customer_id` (or `va_number`). The per-customer row lock becomes trivially cheap because the hot row lives on a specific shard.
3. **Read replicas for `/balance`.** The reconciliation endpoint is read-only; route it to a replica so hot write traffic isn't contended.

All three are narrative, not implemented. The README calls this out explicitly.
