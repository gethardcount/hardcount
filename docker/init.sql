-- Create a non-superuser app role for the application.
-- Superusers bypass RLS, so the app must connect as a non-superuser.
-- The idle-transaction timeout matches deploy/compose/postgres-init.sh and
-- docs/ops/postgres-requirements.md, so dev and tests run as production does.
CREATE ROLE hardcount_app WITH LOGIN PASSWORD 'hardcount_app';
GRANT ALL PRIVILEGES ON DATABASE hardcount TO hardcount_app;
GRANT ALL ON SCHEMA public TO hardcount_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO hardcount_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO hardcount_app;
ALTER ROLE hardcount_app SET idle_in_transaction_session_timeout = '30s';

-- Separate test database — integration tests truncate all data,
-- so they must NOT share the dev database.
CREATE DATABASE hardcount_test OWNER hardcount;
\connect hardcount_test
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE ROLE hardcount_test_app WITH LOGIN PASSWORD 'hardcount_test_app';
GRANT ALL PRIVILEGES ON DATABASE hardcount_test TO hardcount_test_app;
GRANT ALL ON SCHEMA public TO hardcount_test_app;
ALTER DEFAULT PRIVILEGES FOR ROLE hardcount IN SCHEMA public GRANT ALL ON TABLES TO hardcount_test_app;
ALTER DEFAULT PRIVILEGES FOR ROLE hardcount IN SCHEMA public GRANT ALL ON SEQUENCES TO hardcount_test_app;
ALTER ROLE hardcount_test_app SET idle_in_transaction_session_timeout = '30s';
