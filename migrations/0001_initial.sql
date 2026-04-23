-- 0001_initial: schema, constraints, indexes, least-privilege grants.
--
-- Designed to run as a database superuser against an empty or existing
-- database. Idempotent: safe to re-run.

BEGIN;

-- ---------------------------------------------------------------------------
-- Enum types
-- ---------------------------------------------------------------------------

DO $$ BEGIN
    CREATE TYPE deployment_state AS ENUM (
        'ACTIVE', 'FULLY_REPAID', 'DEFAULTED', 'WRITTEN_OFF'
    );
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

DO $$ BEGIN
    CREATE TYPE payment_status AS ENUM (
        'COMPLETE', 'PENDING', 'FAILED', 'REVERSED'
    );
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

DO $$ BEGIN
    CREATE TYPE application_result AS ENUM (
        'APPLIED', 'RECORDED', 'REJECTED'
    );
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- ---------------------------------------------------------------------------
-- Tables
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS customers (
    id         TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS deployments (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id          TEXT NOT NULL REFERENCES customers(id),
    value_kobo           BIGINT NOT NULL CHECK (value_kobo > 0),
    term_weeks           INTEGER NOT NULL CHECK (term_weeks > 0),
    current_balance_kobo BIGINT NOT NULL CHECK (current_balance_kobo >= 0),
    state                deployment_state NOT NULL DEFAULT 'ACTIVE',
    started_at           TIMESTAMPTZ NOT NULL,
    closed_at            TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (current_balance_kobo <= value_kobo),
    CHECK ((state = 'ACTIVE') = (closed_at IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_deployments_customer_active_started
    ON deployments(customer_id, started_at)
    WHERE state = 'ACTIVE';

-- virtual_accounts is the real routing primitive: 1:1 with deployment.
-- The payload in this exercise carries customer_id, so the code routes by
-- customer_id → oldest active deployment. In production the webhook would
-- carry va_number; the table exists to reflect the correct domain model.
CREATE TABLE IF NOT EXISTS virtual_accounts (
    va_number     TEXT PRIMARY KEY,
    deployment_id UUID NOT NULL UNIQUE REFERENCES deployments(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Append-only ledger. The application role has no DELETE grant: rows
-- cannot be removed by the service, only inserted.
CREATE TABLE IF NOT EXISTS payments (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_reference TEXT NOT NULL UNIQUE,
    customer_id           TEXT NOT NULL REFERENCES customers(id),
    deployment_id         UUID REFERENCES deployments(id),
    amount_kobo           BIGINT NOT NULL CHECK (amount_kobo > 0),
    status                payment_status NOT NULL,
    result                application_result NOT NULL,
    reject_reason         TEXT,
    response_status       SMALLINT NOT NULL CHECK (response_status BETWEEN 100 AND 599),
    response_body         BYTEA NOT NULL,
    transaction_date      TIMESTAMPTZ NOT NULL,
    received_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_balance_kobo  BIGINT,
    CHECK ((result = 'APPLIED')  = (applied_balance_kobo IS NOT NULL)),
    CHECK ((result = 'REJECTED') = (reject_reason IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_payments_deployment_applied
    ON payments(deployment_id, received_at DESC)
    WHERE result = 'APPLIED';

CREATE INDEX IF NOT EXISTS idx_payments_customer
    ON payments(customer_id);

CREATE INDEX IF NOT EXISTS idx_payments_non_applied_status
    ON payments(status)
    WHERE result <> 'APPLIED';

-- ---------------------------------------------------------------------------
-- Least-privilege application grants
--
-- The paybook_app role gets the minimum it needs:
--   customers         SELECT         (existence check)
--   deployments       SELECT, UPDATE (route lookup + balance/state transition)
--   virtual_accounts  SELECT         (not read in the current code path;
--                                     granted so future VA-keyed routing works
--                                     without a migration)
--   payments          SELECT, INSERT (ledger is append-only from the app's
--                                     point of view; no UPDATE grant means
--                                     ledger rows are immutable by the service)
--
-- Not granted on any table: DELETE, TRUNCATE, REFERENCES, TRIGGER.
-- Not granted at schema/db level: CREATE, DROP, ALTER.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'paybook_app') THEN
        GRANT CONNECT ON DATABASE paybook TO paybook_app;
        GRANT USAGE ON SCHEMA public TO paybook_app;

        GRANT SELECT                  ON customers        TO paybook_app;
        GRANT SELECT, UPDATE          ON deployments      TO paybook_app;
        GRANT SELECT                  ON virtual_accounts TO paybook_app;
        GRANT SELECT, INSERT          ON payments         TO paybook_app;
    END IF;
END $$;

COMMIT;
