package invoice

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// PublicTokenPrefix is the hc_pinv_ identifier that tags hosted-invoice URL
// tokens. Kept as a package constant so the rotate endpoint and any future
// backfill job use the same format.
const PublicTokenPrefix = "hc_pinv_"

// GeneratePublicToken creates a 256-bit random token for the hosted invoice
// page. The token is the URL — non-guessable by design and shareable via
// email or any public channel. Prefix mirrors the hc_pt_ style used by
// payment-update tokens; livemode is NOT encoded in the token because the
// server resolves it from the underlying invoice row.
func GeneratePublicToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate public token: %w", err)
	}
	return PublicTokenPrefix + hex.EncodeToString(buf), nil
}

// HashPublicToken is the deterministic blind index used to resolve a presented
// URL token to its invoice row without storing the raw token. The token already
// carries 256 bits of entropy, so a plain SHA-256 (no keyed HMAC) is sufficient
// — the hash is unguessable and irreversible, so a DB snapshot yields nothing
// replayable. Must match the SQL backfill in migration 0107
// (encode(sha256(public_token::bytea),'hex')).
func HashPublicToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}
