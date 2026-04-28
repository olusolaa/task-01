# ADR 003: int64 kobo over NUMERIC(20,4); naira on the wire

> Update: clarified the wire-format / storage split. Storage type is unchanged.


**Status:** accepted
**Date:** 2025-11-07

## Context

The service handles monetary amounts. Go's floating-point types are unsafe for money (rounding errors accumulate; `0.1 + 0.2` is a classic). Three reasonable representations exist:

1. `NUMERIC(p, s)` in Postgres, mapped to `decimal.Decimal` or `pgtype.Numeric` in Go.
2. `BIGINT` in Postgres holding minor units (kobo), mapped to `int64` in Go.
3. A named integer type wrapping `int64` so the Go compiler prevents float contamination.

The brief uses integer kobo in the payload (`"10000"`) and the currency is NGN, which has 2 decimal places (100 kobo per naira). There is no sub-kobo accounting need.

## Decision

**Storage and arithmetic: `int64` kobo.** `BIGINT` in the database, wrapped in a named `money.Kobo` type (`type Kobo int64`) in Go.

```go
type Kobo int64
```

`Kobo` is distinct from `int64`: you cannot pass an untyped integer literal or a float expression where a `Kobo` is expected. The only way to construct one is through a parser (`money.ParseKobo`, `money.ParseNaira`) or a type conversion, and a grep for `Kobo(` shows every construction site during review.

**Wire format: naira string, up to 2 decimal places.** The bank webhook field `transaction_amount` is interpreted as naira. `money.ParseNaira` accepts integer-naira (`"10000"` → 1,000,000 kobo) or up to 2 fractional digits (`"100.50"` → 10,050 kobo) and rejects everything else (3+ decimals, scientific, signs, leading zeros, whitespace, empty). Conversion happens once at the service boundary; the rest of the codebase reasons in `Kobo`.

## Consequences

**Positive.** Integer arithmetic, no rounding, no decimal library dependency, fast. The Postgres `CHECK (amount_kobo > 0)` and `CHECK (current_balance_kobo >= 0)` work as plain integer comparisons. The `int64` range (≈ 9.2 × 10^18 kobo ≈ ₦92 quadrillion) is sufficient for any plausible volume.

**Negative.** Switching to a currency with more than 2 decimal places (some crypto settlements, some FX) would require a schema migration to pick a new scale. Not a real concern for this service.

## Alternatives considered

**`NUMERIC(20, 4)` with a decimal library.** The textbook "safe money" choice. Rejected because it adds a dependency (`shopspring/decimal` or `pgtype.Numeric`), the database comparison and aggregation semantics are trickier to reason about under concurrency (row versions grow faster with wider columns), and we gain nothing from sub-kobo precision.

**`int64` without a named type.** Works, but loses compile-time protection. Someone refactoring could accidentally write `balance * 1.1` and the compiler would not stop them. Rejected.
