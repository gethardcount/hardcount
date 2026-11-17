package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// A key is a type prefix, hc_{type}_{mode}_, then secretBytes random bytes in
// lowercase hex. Its lookup prefix (the type prefix and the first lookupHexLen
// hex characters) finds its row, which holds a salted hash of the whole key
// (hashKey). api_keys_prefix_shape_check states the same shape.
const (
	secretBytes  = 32
	lookupHexLen = 12
)

const saltBytes = 16

// typePrefix returns a key's type prefix, "hc_{type}_{mode}_": hc_secret_live_,
// hc_ingest_test_ and so on. The visible mode lets an operator spot a test
// key in a production config.
func typePrefix(kt KeyType, livemode bool) string {
	mode := "test"
	if livemode {
		mode = "live"
	}
	return "hc_" + string(kt) + "_" + mode + "_"
}

// typePrefixes are the type prefixes a key can have: each key type, live and
// test.
var typePrefixes = func() []string {
	var ps []string
	for _, kt := range keyTypes {
		ps = append(ps, typePrefix(kt, true), typePrefix(kt, false))
	}
	return ps
}()

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
// and its stored hash.
func mintKey(kt KeyType, livemode bool) (raw, lookup string, h storedHash, err error) {
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", "", storedHash{}, fmt.Errorf("generate key: %w", err)
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", "", storedHash{}, fmt.Errorf("generate salt: %w", err)
	}
	secretHex := hex.EncodeToString(secret)
	p := typePrefix(kt, livemode)
	raw = p + secretHex
	return raw, p + secretHex[:lookupHexLen], storedHash{salt: hex.EncodeToString(salt), hash: hashKey(salt, raw)}, nil
}

// hashKey returns hex(SHA-256(salt || raw)). An empty salt is valid: keys the
// setup command made before salts were added have none.
func hashKey(salt []byte, raw string) string {
	sum := sha256.Sum256(append(append([]byte{}, salt...), raw...))
	return hex.EncodeToString(sum[:])
}
