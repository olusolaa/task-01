-- Seed 1000 customers, 1000 deployments, 1000 virtual accounts.
-- Idempotent: re-running is a no-op once seed has been applied.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM customers WHERE id = 'GIG00001') THEN
        RAISE NOTICE 'seed already applied, skipping';
        RETURN;
    END IF;

    INSERT INTO customers (id)
    SELECT 'GIG' || LPAD(gs::text, 5, '0')
    FROM generate_series(1, 1000) AS gs;

    INSERT INTO deployments (customer_id, value_kobo, term_weeks, current_balance_kobo, started_at)
    SELECT
        'GIG' || LPAD(gs::text, 5, '0'),
        100000000,
        50,
        100000000,
        now() - ((gs % 50) || ' weeks')::interval
    FROM generate_series(1, 1000) AS gs;

    INSERT INTO virtual_accounts (va_number, deployment_id)
    SELECT 'VA' || d.customer_id, d.id
    FROM deployments d;
END $$;
