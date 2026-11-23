-- +goose Up
-- Test clocks are removed. Delete every test clock and every customer on one,
-- with everything they own, children first so no RESTRICT foreign key trips.
-- A clock is test mode only (its CHECK), and the app only ever put test-mode
-- customers on one (customers has no CHECK for it); every statement reaches
-- rows through a clock's customers, so no live row is touched. Kept: the audit
-- log, and tenant config (meters, plans and their prices, dunning policies,
-- API keys). The statements are the clock teardown's, for every clock.
SET LOCAL app.bypass_rls = 'on';
DELETE FROM invoice_dunning_events WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL));
DELETE FROM invoice_dunning_runs WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL));
DELETE FROM tax_calculations WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL));
DELETE FROM invoice_stripe_refunds WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL));
DELETE FROM credit_note_line_items WHERE credit_note_id IN (SELECT id FROM credit_notes WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL));
DELETE FROM credit_notes WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL);
DELETE FROM invoice_line_items WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL));
DELETE FROM payment_update_tokens WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL);
DELETE FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL);
DELETE FROM usage_events WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL);
DELETE FROM customer_credit_ledger WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL);
DELETE FROM subscriptions WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL);
DELETE FROM customer_price_overrides WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL);
DELETE FROM plan_price_version_clocks;
DELETE FROM customer_billing_profiles WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL);
DELETE FROM payment_methods WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL);
DELETE FROM customer_portal_sessions WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL);
DELETE FROM customer_portal_magic_links WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NOT NULL);
DELETE FROM email_outbox WHERE test_clock_id IS NOT NULL;
DELETE FROM customers WHERE test_clock_id IS NOT NULL;
DELETE FROM test_clocks;

-- +goose Down
-- Nothing to undo: deleted rows cannot be brought back, and the tables and
-- columns are unchanged.
SELECT 1;
