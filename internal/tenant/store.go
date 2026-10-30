package tenant

import (
	"context"

	"github.com/gethardcount/hardcount/internal/domain"
)

type Store interface {
	Get(ctx context.Context, id string) (domain.Tenant, error)
	UpdateStatus(ctx context.Context, id string, status domain.TenantStatus) (domain.Tenant, error)
}
