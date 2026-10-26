package recipe

import (
	"context"
	"database/sql"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Store is the persistence boundary for recipe_instances. The cross-domain
// entities a recipe creates (meters, plans and their prices) are owned by
// the pricing domain's store; this Store only tracks the thin
// instantiation index.
//
// Methods come in two families:
//
//   - Plain methods (GetByKey, List) own their own short-lived transaction.
//   - *Tx variants accept an existing *sql.Tx so Service.Instantiate can
//     thread one transaction across multiple cross-domain calls and
//     guarantee an all-or-nothing commit on the whole graph.
type Store interface {
	// GetByKey returns the existing instance for (tenant, recipe_key) or
	// errs.ErrNotFound. Used by GET /v1/recipes to populate the
	// `instantiated` field on each list entry.
	GetByKey(ctx context.Context, tenantID, recipeKey string) (domain.RecipeInstance, error)

	// List returns all instances for the tenant, newest first. Powers the
	// dashboard's "recipes installed" view.
	List(ctx context.Context, tenantID string) ([]domain.RecipeInstance, error)

	// GetByKeyTx mirrors GetByKey but reuses an existing transaction.
	// Returns (zero, errs.ErrNotFound) if no instance exists. Used as the
	// idempotency gate in Instantiate (badge present → apply is a no-op).
	GetByKeyTx(ctx context.Context, tx *sql.Tx, tenantID, recipeKey string) (domain.RecipeInstance, error)

	// CreateTx inserts a new instance row inside the caller's transaction.
	// The caller is responsible for committing or rolling back.
	CreateTx(ctx context.Context, tx *sql.Tx, inst domain.RecipeInstance) (domain.RecipeInstance, error)
}
