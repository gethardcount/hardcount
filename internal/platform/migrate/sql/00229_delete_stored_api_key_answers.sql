-- +goose Up
-- idempotency_keys keeps a request's answer for a replay. The requests that
-- issue an API key (POST /v1/api-keys and POST /v1/api-keys/{id}/rotate)
-- skip that layer, because their answer holds the key's secret; this deletes
-- the answers kept for them before.
--
-- idempotency_keys forces row-level security, and the user that runs
-- migrations may be subject to it, so the file turns the bypass on for its
-- own transaction.
SET LOCAL app.bypass_rls = 'on';
DELETE FROM idempotency_keys
WHERE http_method = 'POST'
  AND (http_path = '/v1/api-keys' OR http_path = '/v1/api-keys/'
       OR (http_path LIKE '/v1/api-keys/%' AND http_path LIKE '%/rotate'));

-- +goose Down
-- Nothing to restore: the deleted answers held the keys' secrets.
SELECT 1;
