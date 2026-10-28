package auth

import (
	"context"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

type Store interface {
	Create(ctx context.Context, key domain.APIKey) (domain.APIKey, error)
	Get(ctx context.Context, tenantID, id string) (domain.APIKey, error)
	GetByPrefix(ctx context.Context, prefix string) (domain.APIKey, error)
	Revoke(ctx context.Context, tenantID, id string) (domain.APIKey, error)
	// ScheduleExpiry sets expires_at on a non-revoked key without revoking it.
	// Used by rotation when a grace period is requested — ValidateKey rejects
	// expired keys the same way it rejects revoked ones, but leaving
	// revoked_at unset keeps the key visible to GetByPrefix through the
	// grace window so in-flight requests can still authenticate.
	ScheduleExpiry(ctx context.Context, tenantID, id string, expiresAt time.Time) (domain.APIKey, error)
	// List returns one page of the keys and how many match.
	List(ctx context.Context, filter ListFilter) ([]domain.APIKey, int, error)
	TouchLastUsed(ctx context.Context, id string, usedAt time.Time) error
}

type ListFilter struct {
	TenantID string
	Role     string
	Limit    int
	Offset   int
}
