-- +goose Up
-- The set_livemode trigger stamps each new row's livemode from the
-- transaction's app.livemode setting. It read an unset setting as live, so
-- a bypass-RLS insert that forgot to set the mode wrote a test-mode row as
-- live, with no error. It now accepts only 'on' and 'off' and refuses the
-- insert otherwise. An empty string counts as unset: Postgres returns '' on
-- a pooled connection whose earlier transaction set the value locally.
-- postgres.BeginTx sets the mode on every TxTenant; bypass-RLS writers set it
-- themselves with set_config('app.livemode', 'on' or 'off', true).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.set_livemode_from_session() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    mode text := current_setting('app.livemode', true);
BEGIN
    IF mode = 'on' THEN
        NEW.livemode := true;
    ELSIF mode = 'off' THEN
        NEW.livemode := false;
    ELSE
        RAISE EXCEPTION 'insert into % needs app.livemode set to on or off, got %',
            TG_TABLE_NAME, coalesce(quote_literal(mode), 'no value')
            USING HINT = 'Run the insert in a transaction that calls set_config(''app.livemode'', ''on'' or ''off'', true).';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.set_livemode_from_session() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.livemode := (current_setting('app.livemode', true) IS DISTINCT FROM 'off');
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
