package tenant

import (
	"context"
	"database/sql"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) Get(ctx context.Context, id string) (domain.Tenant, error) {
	ctx, cancel := context.WithTimeout(ctx, s.db.QueryTimeout)
	defer cancel()

	var t domain.Tenant
	err := s.db.Pool.QueryRowContext(ctx, `
		SELECT id, name, status, created_at, updated_at
		FROM tenants WHERE id = $1
	`, id).Scan(&t.ID, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.Tenant{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Tenant{}, err
	}
	return t, nil
}

func (s *PostgresStore) UpdateStatus(ctx context.Context, id string, status domain.TenantStatus) (domain.Tenant, error) {
	ctx, cancel := context.WithTimeout(ctx, s.db.QueryTimeout)
	defer cancel()

	var t domain.Tenant
	err := s.db.Pool.QueryRowContext(ctx, `
		UPDATE tenants SET status = $1, updated_at = $2
		WHERE id = $3
		RETURNING id, name, status, created_at, updated_at
	`, status, time.Now().UTC(), id).Scan(&t.ID, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.Tenant{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Tenant{}, err
	}
	return t, nil
}
