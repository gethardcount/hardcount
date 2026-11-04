// Package crypto encrypts stored values, such as PII fields and API secrets,
// with AES-256-GCM. It also computes blind indexes: keyed hashes that let an
// encrypted field be found by exact match.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

const encPrefix = "enc:"

// Encryptor provides AES-256-GCM encryption for PII fields.
// A nil-key encryptor (created via NewNoop) passes values through unchanged.
type Encryptor struct {
	key []byte // nil = noop
}

// NewEncryptor creates an encryptor from a 64-character hex-encoded key (32 bytes).
func NewEncryptor(hexKey string) (*Encryptor, error) {
	if len(hexKey) != 64 {
		return nil, fmt.Errorf("encryption key must be exactly 64 hex characters (32 bytes), got %d", len(hexKey))
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("encryption key is not valid hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("decoded key must be 32 bytes, got %d", len(key))
	}
	return &Encryptor{key: key}, nil
}

// NewNoop returns an encryptor that passes values through without
// encryption. It is used when HARDCOUNT_ENCRYPTION_KEY is not set, in
// development or during a migration.
func NewNoop() *Encryptor {
	return &Encryptor{key: nil}
}

// IsEnabled reports whether encryption is on.
func (e *Encryptor) IsEnabled() bool {
	return e != nil && e.key != nil
}

// Encrypt encrypts plaintext with AES-256-GCM and returns
// "enc:<base64(nonce+ciphertext)>". An empty string comes back as is, since
// there is nothing to hide. When encryption is off (noop), it returns
// plaintext unchanged.
func (e *Encryptor) Encrypt(plaintext string) (string, error) {
	if !e.IsEnabled() {
		return plaintext, nil
	}
	if plaintext == "" {
		return "", nil
	}

	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	encoded := base64.StdEncoding.EncodeToString(ciphertext)
	return encPrefix + encoded, nil
}

// Decrypt decrypts a value that has the "enc:" prefix. Any other value comes
// back as is: plaintext passes through, for compatibility during a migration.
func (e *Encryptor) Decrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.HasPrefix(value, encPrefix) {
		// Not encrypted. Return it as is, for backward compatibility during a
		// migration.
		return value, nil
	}
	if !e.IsEnabled() {
		// The value is encrypted, but there is no key to decrypt it.
		return "", fmt.Errorf("encrypted value found but no encryption key configured")
	}

	encoded := strings.TrimPrefix(value, encPrefix)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}

	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return string(plaintext), nil
}

// Blinder computes a deterministic, keyed hash of a value, so a field
// encrypted at rest can still be looked up by exact match. The HMAC key is
// separate from the AES encryption key on purpose. An attacker who gets one
// key should not also gain the ability to reverse the other.
//
// It suits normalised, fairly low-entropy inputs such as email addresses and
// phone numbers. It does not suit password-like fields, which need a real
// password hash. A nil Blinder passes values through unchanged, so callers
// can still run in dev environments without the key configured.
type Blinder struct {
	key []byte // nil = noop
}

// NewBlinder creates a Blinder from a 64-character hex-encoded key (32
// bytes). The key is separate from the encryption key, so the two can rotate
// independently.
func NewBlinder(hexKey string) (*Blinder, error) {
	if len(hexKey) != 64 {
		return nil, fmt.Errorf("blind-index key must be exactly 64 hex characters (32 bytes), got %d", len(hexKey))
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("blind-index key is not valid hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("decoded blind-index key must be 32 bytes, got %d", len(key))
	}
	return &Blinder{key: key}, nil
}

// NewNoopBlinder returns a Blinder that returns the empty string for any
// input. It is used when HARDCOUNT_EMAIL_BIDX_KEY is not configured. The
// column then gets NULL or empty, and the magic-link lookup path silently
// returns "no match".
func NewNoopBlinder() *Blinder { return &Blinder{key: nil} }

// IsEnabled reports whether the blinder has a key configured. Callers use it
// to decide whether to run the magic-link code path at all.
func (b *Blinder) IsEnabled() bool { return b != nil && b.key != nil }

// Blind returns hex-encoded HMAC-SHA256(key, value). An empty input and an
// unconfigured blinder both return "". That value never matches, so it is
// safe to write into the blind-index column.
func (b *Blinder) Blind(value string) string {
	if !b.IsEnabled() || value == "" {
		return ""
	}
	mac := hmac.New(sha256.New, b.key)
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}
