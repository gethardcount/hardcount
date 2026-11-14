-- +goose Up
-- idempotency_keys keeps a request's answer for a replay. An answer that
-- carries a secret shown only in that answer is not to be kept, so the
-- requests that give one skip that layer: creating or rotating an API key,
-- creating a webhook endpoint or rotating its signing secret, rotating a
-- customer's cost dashboard token, and creating a card setup session. This
-- deletes the answers kept for them before.
--
-- idempotency_keys forces row-level security, and the user that runs
-- migrations may be subject to it, so the file turns the bypass on for its
-- own transaction.
SET LOCAL app.bypass_rls = 'on';
DELETE FROM idempotency_keys
WHERE http_method = 'POST'
  AND (http_path ~ '^/v1/api-keys/?$'
       OR http_path ~ '^/v1/api-keys/[^/]+/rotate/?$'
       OR http_path ~ '^/v1/webhook-endpoints/endpoints/?$'
       OR http_path ~ '^/v1/webhook-endpoints/endpoints/[^/]+/rotate-secret/?$'
       OR http_path ~ '^/v1/customers/[^/]+/rotate-cost-dashboard-token/?$'
       OR http_path ~ '^/v1/customers/[^/]+/payment-methods/setup-session/?$');

-- +goose Down
-- Nothing to restore: the deleted answers held secrets.
SELECT 1;
