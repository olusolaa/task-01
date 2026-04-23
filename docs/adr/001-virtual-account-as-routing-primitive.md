# ADR 001: Virtual account as the routing primitive

**Status:** accepted
**Date:** 2025-11-07

## Context

The brief states:

> Customer payments come in through bank transfers into their virtual accounts which the company provided upon asset deployment.

A virtual account is 1:1 with a deployment. In production, when a bank webhook fires, the wire payload carries the virtual account number, and the system looks up the deployment from it.

The sample payload provided with the brief carries `customer_id`, not `va_number`. That creates an ambiguity: if a single `customer_id` can map to multiple active deployments, which balance does a payment reduce?

## Decision

Model `virtual_accounts` as a first-class table with `va_number` as its primary key and a foreign key to `deployments`. The relationship is 1:1 enforced at the schema level (`UNIQUE` on `deployment_id`).

For the exercise, accept the payload as given and route by `customer_id` using the policy **oldest active deployment, falling back to oldest inactive**. The fallback lets the service return a specific `deployment_inactive` 409 instead of a misleading 404 when every deployment for a customer is closed.

In production the webhook shape would change to include `va_number`, and `LockRoutingDeployment` would look up the deployment by `va_number` directly, with no ambiguity.

## Consequences

**Positive.** The data model reflects the real domain primitive. Adding the production code path later is a query change, not a schema migration. Multiple deployments per customer (foreseeable as the business scales) work without policy gymnastics.

**Negative.** Slightly more schema than the literal payload requires. The `virtual_accounts` table is populated by the seed script but not used by the apply code path in this exercise.

## Alternatives considered

**Route by `customer_id` only, no `virtual_accounts` table.** Simpler schema, but silently hides the correct abstraction and bakes in the "oldest active" policy at the schema level. Rejected because the abstraction is load-bearing once the business has more than one deployment per customer.

**Require `deployment_id` in the payload.** Cleanest from the service's perspective, but the payload is dictated by the bank webhook contract — we cannot change it unilaterally. Rejected as out-of-scope.

**Defer routing to an upstream service.** Have a webhook-intake service resolve `va_number` → `customer_id` + `deployment_id` and pass a fully-resolved payload downstream. Good design for a larger system. Rejected as over-scoped for this service.
