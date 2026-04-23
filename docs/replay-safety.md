# Replay safety walkthrough

The bank delivers webhooks at-least-once. Exactly-once is a myth: any system that tries to guarantee it under network failure is fooling itself. The practical answer is *idempotent operations* — the second delivery produces the same effect as the first, which is no effect at all after the initial apply.

This service's idempotency story has three moving parts and I'll walk them through one race at a time.

## The unique constraint is the root

`payments.transaction_reference` has a `UNIQUE` index. This is the only thing that makes any of the rest work. If two rows could have the same reference, every other guarantee collapses.

## The happy-path race: replay arrives after the original

**Timeline.**
1. Bank delivers request A, service applies it, commits, returns 201 with body X.
2. Bank delivers request A again (retry), service arrives in the same handler.
3. Handler's first SQL is `SELECT ... FROM payments WHERE transaction_reference = $1`. It hits.
4. Service returns status and body X from the stored columns, with `Idempotent-Replayed: true`.
5. No deployment lock taken, no balance change, no side effect.

This is the cheap path and it covers 99% of replays in practice.

## The tight race: two concurrent deliveries of the same reference

**Timeline.**
1. Request A and request A' arrive at the service within microseconds of each other.
2. Both start a transaction. Both run the `SELECT ... WHERE transaction_reference = $1` lookup. Both miss (no row yet).
3. Both call `LockRoutingDeployment` which takes `FOR UPDATE` on the deployment row. One acquires the lock; the other blocks.
4. The lock winner runs `decide()`, builds response bytes, and runs:
   ```sql
   INSERT INTO payments (...) VALUES (...)
   ON CONFLICT (transaction_reference) DO NOTHING
   RETURNING id
   ```
   `RETURNING` returns a row. Winner updates deployment balance, commits, releases lock.
5. The loser is unblocked, acquires the lock, runs `decide()` on the (now updated) deployment state, builds response bytes for what it *thinks* the outcome should be, and runs the same `INSERT ... ON CONFLICT`. This time `ON CONFLICT` fires; `RETURNING` returns nothing.
6. The loser detects the empty return, re-queries `SELECT ... FROM payments WHERE transaction_reference = $1`, finds the winner's row, and returns the winner's stored response.

Two requests. One row. One balance change. Both responses are byte-identical because they come from the same stored bytes. This is the race the `ON CONFLICT DO NOTHING RETURNING id` pattern exists for.

## The pathological race: duplicate reference for different customers

Banks issue globally unique references, so "same reference, different customers" should never happen. But the code handles it anyway because the idempotency contract is on `transaction_reference`, not on `(customer_id, transaction_reference)`.

**Timeline.** Same as the tight race, except the loser's deployment lock is on a different row than the winner's. The loser still hits the `ON CONFLICT` on insert, still re-reads, and still returns the winner's response. The loser's `customer_id` never gets a payment row. This is a business anomaly (why is a bank sending us two different customer ids for one reference?) and the response body will carry whichever customer won the race, which is the correct answer given only one of them actually reduced a balance.

## Why the response is stored, not reconstructed

The response body encodes:
- the transaction reference (always available)
- the deployment id and state (available from the lock)
- the amount applied and balance after (computed at decide-time)
- the applicable status code

If we reconstructed the response on replay, any future change to the response shape — a new field, a renamed field, a different error code — would drift the replay response away from the original. The byte-identical guarantee would rot silently.

Storing the bytes costs maybe 400 bytes per row. At 100k payments/minute that's 58 MB per day, 21 GB per year. Not a concern at this scope and the right trade every time.

## Test evidence

- `TestApply_IdempotentReplay_ByteIdentical` — explicit assertion that the second response is exactly the same bytes as the first.
- `TestApply_ConcurrentDuplicates_ExactlyOneApplied` — 100 goroutines fire the same reference. Exactly one row ends up in `payments`.
- `TestProperty_ReplayByteIdentical` — rapid-generated payments with random amounts and statuses; each is then replayed up to 10 times. Every replay is byte-equal.
- `TestProperty_ExactlyOneApplicationPerRef` — rapid-generated worker counts from 10 to 150, each storm with a single reference. Always exactly one row.
