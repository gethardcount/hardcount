package domain

import "time"

type APIKey struct {
	ID         string     `json:"id"`
	KeyPrefix  string     `json:"key_prefix"`
	KeyHash    string     `json:"-"`
	KeySalt    string     `json:"-"`        // hex-encoded 16-byte salt for SHA-256 hashing
	KeyType    string     `json:"key_type"` // platform, secret
	Livemode   bool       `json:"livemode"`
	Name       string     `json:"name"`
	TenantID   string     `json:"tenant_id"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}
