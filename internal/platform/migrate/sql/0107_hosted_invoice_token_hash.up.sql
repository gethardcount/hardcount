-- 0107: stop storing the hosted-invoice public_token in plaintext.
--
-- The token IS the hosted-invoice URL credential — anyone holding it can view
-- the invoice. Stored plaintext, a DB snapshot leak yielded directly-replayable
-- URLs. Split it into two columns matching the customer-email pattern:
--   * public_token_hash      — deterministic SHA-256 blind index, used to
--                              resolve a presented URL token to its invoice.
--                              Irreversible: a snapshot of it is not replayable.
--   * public_token_encrypted — AES-GCM ciphertext (app key, not in the DB),
--                              decrypted only to rebuild the URL on re-send so
--                              the hosted URL stays stable across sends.
-- The raw token is no longer stored on the invoice. It is still written to
-- email_outbox.payload (key public_token) for each email that links to the
-- invoice, and removed from that row when the email is sent or skipped; a
-- failed row keeps it (internal/email/outbox.go, comment corrected 2026-10-13).
ALTER TABLE invoices ADD COLUMN public_token_encrypted text;
ALTER TABLE invoices ADD COLUMN public_token_hash text;

-- Backfill the lookup hash from any existing plaintext tokens so existing
-- hosted-invoice URLs keep resolving (encode(sha256(token::bytea),'hex') matches
-- invoice.HashPublicToken). public_token_encrypted stays NULL for pre-existing
-- rows — the app re-encrypts on the next token rotation; a re-send before then
-- simply omits the (now unreconstructable) hosted URL.
UPDATE invoices
   SET public_token_hash = encode(sha256(public_token::bytea), 'hex')
 WHERE public_token IS NOT NULL AND public_token <> '';

CREATE UNIQUE INDEX idx_invoices_public_token_hash
    ON invoices (public_token_hash)
    WHERE public_token_hash IS NOT NULL;

DROP INDEX IF EXISTS idx_invoices_public_token;
ALTER TABLE invoices DROP COLUMN public_token;
