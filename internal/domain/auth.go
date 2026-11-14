package domain

import "time"

// An API key's status: active keys authenticate, expired and revoked ones do
// not.
const (
	APIKeyStatusActive  = "active"
	APIKeyStatusExpired = "expired"
	APIKeyStatusRevoked = "revoked"
)

// APIKey is a stored API key as the API returns it, never with its raw value
// or its hash. KeyPrefix is the key's lookup prefix: its type prefix
// (hc_secret_live_ and so on) and the first 12 hex characters of its secret,
// by which a request's key is found and an operator recognizes it.
type APIKey struct {
	ID         string     `json:"id"`
	KeyPrefix  string     `json:"key_prefix"`
	KeyType    string     `json:"key_type"` // secret or ingest
	Status     string     `json:"status"`   // APIKeyStatus*, from statusAt in auth
	Livemode   bool       `json:"livemode"`
	Name       string     `json:"name"`
	TenantID   string     `json:"tenant_id"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}
