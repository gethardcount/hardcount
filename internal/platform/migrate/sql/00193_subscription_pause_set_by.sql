-- +goose Up
-- A collection pause records who set it: 'operator' (the API or the
-- dashboard) or 'dunning' (a policy's pause final action). A dunning pause
-- lifts when the subscription owes nothing; an operator pause lifts only
-- when the operator resumes it or at its resumes_at.
ALTER TABLE subscriptions ADD COLUMN pause_collection_set_by TEXT;

-- Pauses already set were set under "until the operator resumes", so they
-- keep that.
UPDATE subscriptions SET pause_collection_set_by = 'operator'
WHERE pause_collection_behavior IS NOT NULL;

-- The pair CHECK makes a writer that sets or clears the pause without this
-- column fail. 00194 checks the rows already there.
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_pause_collection_set_by_check
    CHECK (pause_collection_set_by IN ('operator', 'dunning')) NOT VALID;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_pause_collection_set_by_pair_check
    CHECK ((pause_collection_behavior IS NULL) = (pause_collection_set_by IS NULL)) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local.
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_pause_collection_set_by_pair_check;
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_pause_collection_set_by_check;
ALTER TABLE subscriptions DROP COLUMN pause_collection_set_by;
