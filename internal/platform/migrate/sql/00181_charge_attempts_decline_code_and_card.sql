-- +goose Up
-- Each charge attempt keeps the card it charged and the bank's decline code
-- (#966). A dunning retry reads them to skip a card the bank refused for good
-- (lost, stolen and the rest of domain.HardDeclineCodes) until another card
-- is the customer's default. Empty on rows written before this file: Stripe
-- never told Velox these facts for them, so nothing is guessed.
ALTER TABLE invoice_charge_attempts
    ADD COLUMN decline_code TEXT NOT NULL DEFAULT '',
    ADD COLUMN stripe_payment_method_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE invoice_charge_attempts
    DROP COLUMN stripe_payment_method_id,
    DROP COLUMN decline_code;
