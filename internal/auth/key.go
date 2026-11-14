package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// A key is its type prefix, hc_{type}_{mode}_, then a secret of secretBytes
// random bytes in lowercase hex: hc_secret_live_ and 64 hex characters. The
// table stores the key's lookup prefix, its type prefix and the first
// lookupHexLen hex characters of the secret, by which a request's key is
// found, and a salted hash of the whole key (hashKey). The table's
// api_keys_prefix_shape_check states the same shape for the lookup prefix.
const (
	secretBytes  = 32
	lookupHexLen = 12
)

// typePrefix returns a key's type prefix, "hc_{type}_{mode}_": hc_secret_live_,
// hc_ingest_test_ and so on. The visible mode lets an operator spot a test
// key in a production config. kt is secret or ingest, the types api_keys
// holds.
func typePrefix(kt KeyType, livemode bool) string {
	mode := "test"
	if livemode {
		mode = "live"
	}
	return "hc_" + string(kt) + "_" + mode + "_"
}

// typePrefixes are the four type prefixes a key can have.
var typePrefixes = []string{
	typePrefix(KeyTypeSecret, true), typePrefix(KeyTypeSecret, false),
	typePrefix(KeyTypeIngest, true), typePrefix(KeyTypeIngest, false),
}

// lookupPrefix returns the lookup prefix of raw, or false when raw is not a
// key: one of typePrefixes, then exactly 2*secretBytes lowercase hex
// characters.
func lookupPrefix(raw string) (string, bool) {
	for _, p := range typePrefixes {
		secret, ok := strings.CutPrefix(raw, p)
		if !ok {
			continue
		}
		if len(secret) != 2*secretBytes || strings.Trim(secret, "0123456789abcdef") != "" {
			return "", false
		}
		return p + secret[:lookupHexLen], true
	}
	return "", false
}

// storedHash is a key's salt and hash as the table stores them, both hex.
type storedHash struct {
	salt, hash string
}

// mintKey returns a new raw key of kt in mode livemode, its lookup prefix,
// and its stored hash with a fresh 16-byte salt.
func mintKey(kt KeyType, livemode bool) (raw, lookup string, h storedHash, err error) {
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", "", storedHash{}, fmt.Errorf("generate key: %w", err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", "", storedHash{}, fmt.Errorf("generate salt: %w", err)
	}
	secretHex := hex.EncodeToString(secret)
	p := typePrefix(kt, livemode)
	raw = p + secretHex
	return raw, p + secretHex[:lookupHexLen], storedHash{salt: hex.EncodeToString(salt), hash: hashKey(salt, raw)}, nil
}

// hashKey returns the stored hash of raw: hex(SHA-256(salt || raw)). Every
// new key has a fresh 16-byte salt. A key the setup command wrote in v0.9.3
// or earlier has an empty salt and still validates.
func hashKey(salt []byte, raw string) string {
	sum := sha256.Sum256(append(append([]byte{}, salt...), raw...))
	return hex.EncodeToString(sum[:])
}
