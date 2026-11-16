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
// request a reset. The sign-in's and the password reset's transactions are
// tested on the real store (login_integration_test.go,
// confirm_reset_integration_test.go). SignIn here runs its hook with no
// transaction; ResetPassword answers resetErr, or refuses.
type fakeUserStore struct {
	// loginUser, when set, is returned by GetByEmail on a matching email so the
	// login path can run end-to-end; tenants is returned by TenantsForUser,
	// and SignIn signs in to the first of them.
	loginUser *domain.User
	tenants   []domain.UserTenant
	// createTokenErr, when set, is returned by CreateResetToken — the real
	// store returns ErrResetSendCapped there once the account is over its
	// per-window send budget.
	createTokenErr error
	// liveLink makes LookupResetToken find a live token.
	liveLink bool
	// resetErr, when set, is what ResetPassword returns.
	resetErr error
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
func (f *fakeUserStore) SignIn(_ context.Context, _, _ string, _ time.Time, then func(*sql.Tx, string) error) (string, error) {
	if len(f.tenants) == 0 {
		return "", errNoWorkspace
	}
	if err := then(nil, f.tenants[0].TenantID); err != nil {
		return "", err
	}
	return f.tenants[0].TenantID, nil
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
	if f.resetErr != nil {
		return domain.User{}, f.resetErr
	}
	return domain.User{}, errors.New("fakeUserStore: the password reset is tested on the real store")
}
func (f *fakeUserStore) LookupResetToken(ctx context.Context, tokenHash string) (string, error) {
	if f.liveLink {
		return "usr_1", nil
	}
	return "", errs.ErrNotFound
}

// stubSessionStore satisfies session.Store and keeps nothing: the sessions
// of the sign-in, the sign-out and the mode switch are tested on the real
// store.
type stubSessionStore struct{}

func (stubSessionStore) Insert(context.Context, session.Session) error { return nil }
func (stubSessionStore) InsertTx(context.Context, *sql.Tx, session.Session) error {
	return nil
}
func (stubSessionStore) RevokeTx(context.Context, *sql.Tx, string) (bool, error) {
	return false, nil
}
func (stubSessionStore) GetByIDHash(context.Context, string) (session.Session, error) {
	return session.Session{}, session.ErrNotFound
}
func (stubSessionStore) EndAllForUserTx(context.Context, *sql.Tx, string) error { return nil }
func (stubSessionStore) EndForMemberTx(context.Context, *sql.Tx, string, string) error {
	return nil
}
func (stubSessionStore) EndWith(context.Context, string, func(*sql.Tx, session.Session) error) (session.Session, bool, error) {
	return session.Session{}, false, errors.New("stubSessionStore: the sign-out is tested on the real store")
}
func (stubSessionStore) SetLivemodeWith(context.Context, string, bool, func(*sql.Tx, session.Session) error) (session.Session, bool, error) {
	return session.Session{}, false, errors.New("stubSessionStore: the mode switch is tested on the real store")
}

// recordingAuditTx satisfies AuditTxWriter and records each row it is asked
// to write.
type recordingAuditTx struct {
	rows []audit.Entry
}

func (r *recordingAuditTx) LogInTx(_ context.Context, _ *sql.Tx, e audit.Entry) error {
	r.rows = append(r.rows, e)
	return nil
}

// newFakeService is a Service on store whose sessions are kept nowhere and
// whose audit rows are only recorded.
func newFakeService(store Store) *Service {
	svc, _ := newRecordingService(store)
	return svc
}

// newRecordingService is newFakeService that also returns the audit
// recorder.
func newRecordingService(store Store) (*Service, *recordingAuditTx) {
	auditor := &recordingAuditTx{}
	return NewService(store, clock.Real(), session.NewService(stubSessionStore{}), auditor), auditor
}

// stubEmailSender satisfies the handler's EmailSender dependency.
type stubEmailSender struct{}

func (stubEmailSender) SendPasswordReset(ctx context.Context, tenantID, email, resetLink string) error {
	return nil
}
