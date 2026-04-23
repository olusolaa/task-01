# Load tests

Two k6 scenarios probe the service under realistic traffic.

## Steady

`steady.js` runs 2,000 req/s for 60 seconds across ~1,000 customers. The brief calls for 100,000 notifications/minute; 2,000 TPS is 1.2× that, providing headroom against bursts. Thresholds: p99 < 100ms, error rate < 1%.

```
BASE_URL=http://localhost:8080 \
HMAC_SECRET=dev_secret_change_in_production \
k6 run load/steady.js
```

## Burst / replay storm

`burst.js` simulates a bank reconciliation retry storm: the `fresh` scenario posts 1,000 unique refs per second, and the `replay` scenario (starting five seconds later) posts 1,000 duplicates-from-the-same-pool per second. The invariant under test is that replays return the same response as the original and never double-apply.

```
BASE_URL=http://localhost:8080 \
HMAC_SECRET=dev_secret_change_in_production \
k6 run load/burst.js
```

## Reading the output

k6 prints a summary block on exit:
- `http_req_duration` — p50/p90/p95/p99 latencies
- `http_req_failed` — fraction of non-2xx/3xx responses
- `checks` — rate of passing assertions from `check()` calls

Exports go to `load/results/*.json` when invoked via `make load-steady` / `make load-burst`, kept out of git except for the directory placeholder.

## Installing k6

macOS: `brew install k6`. Linux: see <https://k6.io/docs/get-started/installation>.
