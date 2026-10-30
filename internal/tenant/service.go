package tenant

import (
	"context"
	"database/sql"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

// AuditEmitter is the narrow in-tx audit seam (ADR-090).
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// Service reads and updates accounts. Hardcount creates an account only
// through RunBootstrap (POST /v1/bootstrap or cmd/hardcount-bootstrap). No
// route creates or lists accounts across a server (ADR-133).
type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) Get(ctx context.Context, id string) (domain.Tenant, error) {
	if id == "" {
		return domain.Tenant{}, errs.Required("id")
	}
	return s.store.Get(ctx, id)
}

func (s *Service) UpdateStatus(ctx context.Context, id string, status domain.TenantStatus) (domain.Tenant, error) {
	if id == "" {
		return domain.Tenant{}, errs.Required("id")
	}
	return s.store.UpdateStatus(ctx, id, status)
}
