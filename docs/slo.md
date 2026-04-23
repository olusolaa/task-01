# Service-Level Objectives

## Traffic expectations

- **Sustained:** 100,000 payment notifications / minute (≈1,667 req/s)
- **Burst:** up to 3× sustained for short windows (bank reconciliation retries)

## Latency

| Percentile | Target (ms) |
|------------|------------:|
| p50        |          10 |
| p90        |          30 |
| p99        |         100 |

Measured at the load balancer, from request receipt to final byte of response. Verified in `load/steady.js` with a threshold assertion.

## Availability

- **Write path (`POST /payments`)**: 99.9% monthly, giving an error budget of ~43 minutes.
- **Read path (`GET /customers/{id}/balance`)**: 99.5% monthly (lower because it is a convenience endpoint; the write path is the load-bearing one).

Error budget spend:
- Unplanned outages (database unavailable, app crash loop)
- Intentional risky deploys (the first 10 minutes after a release may draw from the budget)
- Scheduled maintenance (pre-announced, uses a portion of the budget)

## Correctness

There is no acceptable rate of drift. Any non-zero drift detected by the reconciliation endpoint is a paging alert, not a budgeted loss. The correctness bar is stricter than the availability bar because the business impact is asymmetric: a missed request can be retried; a wrong balance cannot be detected without comparing to the ledger.

## Alerts

| Signal | Condition | Severity |
|---|---|---|
| `paybook_http_request_duration_seconds{path="/payments"}:p99` > 200ms for 5m | latency | warn |
| `paybook_http_request_duration_seconds{path="/payments"}:p99` > 500ms for 5m | latency | page |
| `paybook_http_requests_total{path="/payments",status=~"5.."}:rate` > 1% for 5m | error rate | page |
| `paybook_http_requests_total{path="/payments",status="401"}:rate` 5x baseline | HMAC spike | warn |
| readiness probe returning 503 for 2m | db reachability | page |
| any detected drift on `/balance` | correctness | page |

All alert rules are narrative; a prometheus `alerts.yml` is out of scope for this exercise.
