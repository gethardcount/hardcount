package auth

import (
	"context"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Store is the persistence interface for API keys.
type Store interface {
	Create(ctx context.Context, key domain.APIKey) (domain.APIKey, error)
	Get(ctx context.Context, tenantID, id string) (domain.APIKey, error)
	GetByPrefix(ctx context.Context, prefix string) (domain.APIKey, error)
	Revoke(ctx context.Context, tenantID, id string) (domain.APIKey, error)
	// ScheduleExpiry sets expires_at on a key that is not revoked, without
	// revoking it. Rotation uses it when a grace period is requested.
	// ValidateKey rejects expired keys the same way it rejects revoked ones.
	// But with revoked_at unset, GetByPrefix still finds the key through the
	// grace window, so requests in flight can still authenticate.
	ScheduleExpiry(ctx context.Context, tenantID, id string, expiresAt time.Time) (domain.APIKey, error)
	// List returns one page of the keys and how many match.
	List(ctx context.Context, filter ListFilter) ([]domain.APIKey, int, error)
	TouchLastUsed(ctx context.Context, id string, usedAt time.Time) error
}

// ListFilter selects a page of a tenant's keys. Role, when set, matches
// key_type.
type ListFilter struct {
	TenantID string
	Role     string
	Limit    int
	Offset   int
}
