-- Create a non-superuser app role for the application.
-- Superusers bypass RLS, so the app must connect as a non-superuser.
-- The idle-transaction timeout matches deploy/compose/postgres-init.sh and
-- docs/ops/postgres-requirements.md, so dev and tests run as production does.
CREATE ROLE velox_app WITH LOGIN PASSWORD 'velox_app';
GRANT ALL PRIVILEGES ON DATABASE velox TO velox_app;
GRANT ALL ON SCHEMA public TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO velox_app;
ALTER ROLE velox_app SET idle_in_transaction_session_timeout = '30s';

-- Separate test database — integration tests truncate all data,
-- so they must NOT share the dev database.
CREATE DATABASE velox_test OWNER velox;
\connect velox_test
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE ROLE velox_test_app WITH LOGIN PASSWORD 'velox_test_app';
GRANT ALL PRIVILEGES ON DATABASE velox_test TO velox_test_app;
GRANT ALL ON SCHEMA public TO velox_test_app;
ALTER DEFAULT PRIVILEGES FOR ROLE velox IN SCHEMA public GRANT ALL ON TABLES TO velox_test_app;
ALTER DEFAULT PRIVILEGES FOR ROLE velox IN SCHEMA public GRANT ALL ON SEQUENCES TO velox_test_app;
ALTER ROLE velox_test_app SET idle_in_transaction_session_timeout = '30s';
