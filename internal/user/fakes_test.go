package user

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/session"
)

// fakeUserStore is an in-memory Store for the handler tests that sign in or
// request a reset. The password reset itself is tested on the real store
// (confirm_reset_integration_test.go), so ResetPassword here refuses.
type fakeUserStore struct {
	// loginUser, when set, is returned by GetByEmail on a matching email so the
	// login path can run end-to-end; tenants is returned by TenantsForUser.
	loginUser *domain.User
	tenants   []domain.UserTenant
	// createTokenErr, when set, is returned by CreateResetToken — the real
	// store returns ErrResetSendCapped there once the account is over its
	// per-window send budget.
	createTokenErr error
}

func (f *fakeUserStore) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	if f.loginUser != nil && f.loginUser.Email == email {
		return *f.loginUser, nil
	}
	return domain.User{}, errs.ErrNotFound
}
func (f *fakeUserStore) GetByID(ctx context.Context, id string) (domain.User, error) {
	return domain.User{}, errs.ErrNotFound
}
func (f *fakeUserStore) TouchLastLogin(ctx context.Context, id string, at time.Time) error {
	return nil
}
func (f *fakeUserStore) TenantsForUser(ctx context.Context, userID string) ([]domain.UserTenant, error) {
	return f.tenants, nil
}
func (f *fakeUserStore) CreateResetToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (domain.PasswordResetToken, error) {
	if f.createTokenErr != nil {
		return domain.PasswordResetToken{}, f.createTokenErr
	}
	return domain.PasswordResetToken{}, nil
}
func (f *fakeUserStore) ResetPassword(context.Context, string, string, func(*sql.Tx, string) error) (domain.User, error) {
	return domain.User{}, errors.New("fakeUserStore: the password reset is tested on the real store")
}
func (f *fakeUserStore) LookupResetToken(ctx context.Context, tokenHash string) (string, error) {
	return "", errs.ErrNotFound
}

// recordingSessionStore satisfies session.Store and records the sessions
// the login path inserts.
type recordingSessionStore struct {
	inserted []session.Session
}

func (r *recordingSessionStore) Insert(ctx context.Context, s session.Session) error {
	r.inserted = append(r.inserted, s)
	return nil
}
func (r *recordingSessionStore) GetByIDHash(ctx context.Context, idHash string) (session.Session, error) {
	return session.Session{}, session.ErrNotFound
}
func (r *recordingSessionStore) Revoke(ctx context.Context, idHash string) error { return nil }
func (r *recordingSessionStore) RevokeAllForUser(ctx context.Context, userID string) error {
	return nil
}
func (r *recordingSessionStore) EndAllForUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	return nil
}
func (r *recordingSessionStore) UpdateLivemode(ctx context.Context, idHash string, livemode bool) error {
	return nil
}

// refusingAuditTx satisfies AuditTxWriter for handler tests whose route
// writes no in-transaction audit row.
type refusingAuditTx struct{}

func (refusingAuditTx) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	return errors.New("refusingAuditTx: this test writes no in-transaction audit row")
}

// newFakeService is a Service on store for the handler tests that never
// reach the password reset's transaction.
func newFakeService(store Store) *Service {
	return NewService(store, clock.Real(), session.NewService(&recordingSessionStore{}), refusingAuditTx{})
}

// stubEmailSender satisfies the handler's EmailSender dependency.
type stubEmailSender struct{}

func (stubEmailSender) SendPasswordReset(ctx context.Context, tenantID, email, resetLink string) error {
	return nil
}
