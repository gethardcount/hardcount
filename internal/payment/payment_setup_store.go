package payment

import (
	"context"
	"database/sql"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// PaymentSetupStore persists Stripe customer/payment method data.
type PaymentSetupStore interface {
	UpsertPaymentSetup(ctx context.Context, tenantID string, ps domain.CustomerPaymentSetup) (domain.CustomerPaymentSetup, error)
	// UpsertPaymentSetupAudited runs the caller-supplied audit emission on
	// the same tx as the persisted mapping write (ADR-090) — used by the
	// checkout.session.completed flip so the webhook's only durable
	// mutation carries its evidence atomically.
	UpsertPaymentSetupAudited(ctx context.Context, tenantID string, ps domain.CustomerPaymentSetup, emit func(tx *sql.Tx) error) (domain.CustomerPaymentSetup, error)
	GetPaymentSetup(ctx context.Context, tenantID, customerID string) (domain.CustomerPaymentSetup, error)
}
