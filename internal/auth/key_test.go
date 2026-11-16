package auth

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// A key's last_used_at write is due at most once per lastUsedTouchInterval,
// per key: again once the interval has passed, and never held back by
// another key's write.
func TestLastUsedDue_OncePerIntervalPerKey(t *testing.T) {
	var d lastUsedDebounce
	t0 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	for _, step := range []struct {
		id   string
		at   time.Time
		want bool
	}{
		{"key_a", t0, true},
		{"key_a", t0.Add(30 * time.Second), false},
		{"key_b", t0.Add(30 * time.Second), true},
		{"key_a", t0.Add(lastUsedTouchInterval - time.Nanosecond), false},
		{"key_a", t0.Add(lastUsedTouchInterval), true},
		{"key_a", t0.Add(lastUsedTouchInterval + time.Second), false},
	} {
		if got := d.due(step.id, step.at); got != step.want {
			t.Errorf("due(%s, +%s) = %v, want %v", step.id, step.at.Sub(t0), got, step.want)
		}
	}
}

// A key's status at a moment: revoked once revoked, even before its expiry;
// otherwise expired at or after its expires_at; otherwise active.
func TestStatusAt(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	for _, c := range []struct {
		name string
		key  domain.APIKey
		want string
	}{
		{"no expiry", domain.APIKey{}, domain.APIKeyStatusActive},
		{"expires later", domain.APIKey{ExpiresAt: at(time.Second)}, domain.APIKeyStatusActive},
		{"expires now", domain.APIKey{ExpiresAt: at(0)}, domain.APIKeyStatusExpired},
		{"expired earlier", domain.APIKey{ExpiresAt: at(-time.Hour)}, domain.APIKeyStatusExpired},
		{"revoked", domain.APIKey{RevokedAt: at(-time.Minute)}, domain.APIKeyStatusRevoked},
		{"revoked before it expires", domain.APIKey{RevokedAt: at(-time.Minute), ExpiresAt: at(time.Hour)}, domain.APIKeyStatusRevoked},
		{"revoked after it expired", domain.APIKey{RevokedAt: at(-time.Minute), ExpiresAt: at(-time.Hour)}, domain.APIKeyStatusRevoked},
	} {
		if got := statusAt(c.key, now); got != c.want {
			t.Errorf("%s: status %q, want %q", c.name, got, c.want)
		}
	}
}
