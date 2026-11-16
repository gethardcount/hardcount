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
// transaction; ResetPassword only counts its calls and answers resetErr, or
// refuses.
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
	// resetCalls counts ResetPassword calls.
	resetCalls int
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
	f.resetCalls++
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

// recordingSessionStore satisfies session.Store and records the sessions
// the login path inserts and revokes.
type recordingSessionStore struct {
	inserted []session.Session
	revoked  []string // id hashes
}

func (r *recordingSessionStore) Insert(ctx context.Context, s session.Session) error {
	r.inserted = append(r.inserted, s)
	return nil
}
func (r *recordingSessionStore) InsertTx(ctx context.Context, tx *sql.Tx, s session.Session) error {
	r.inserted = append(r.inserted, s)
	return nil
}
func (r *recordingSessionStore) RevokeTx(ctx context.Context, tx *sql.Tx, idHash string) error {
	r.revoked = append(r.revoked, idHash)
	return nil
}
func (r *recordingSessionStore) GetByIDHash(ctx context.Context, idHash string) (session.Session, error) {
	return session.Session{}, session.ErrNotFound
}
func (r *recordingSessionStore) Revoke(ctx context.Context, idHash string) error { return nil }
func (r *recordingSessionStore) EndAllForUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	return nil
}
func (r *recordingSessionStore) EndForMemberTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	return nil
}
func (r *recordingSessionStore) UpdateLivemode(ctx context.Context, idHash string, livemode bool) error {
	return nil
}

// recordingAuditTx satisfies AuditTxWriter and records each row with the
// actor and the client address on its ctx, read the way audit.Logger.LogInTx
// reads them.
type recordingAuditTx struct {
	rows []recordedAuditTx
}

type recordedAuditTx struct {
	entry              audit.Entry
	actorType, actorID string
	ip                 string
}

func (r *recordingAuditTx) LogInTx(ctx context.Context, _ *sql.Tx, e audit.Entry) error {
	at, aid := audit.ResolveActor(ctx)
	r.rows = append(r.rows, recordedAuditTx{entry: e, actorType: at, actorID: aid, ip: audit.ClientIP(ctx)})
	return nil
}

// newFakeService is a Service on store whose sessions and audit rows are
// only recorded.
func newFakeService(store Store) *Service {
	svc, _, _ := newRecordingService(store)
	return svc
}

// newRecordingService is newFakeService that also returns the recorders.
func newRecordingService(store Store) (*Service, *recordingSessionStore, *recordingAuditTx) {
	sessions := &recordingSessionStore{}
	auditor := &recordingAuditTx{}
	return NewService(store, clock.Real(), session.NewService(sessions), auditor), sessions, auditor
}

// stubEmailSender satisfies the handler's EmailSender dependency.
type stubEmailSender struct{}

func (stubEmailSender) SendPasswordReset(ctx context.Context, tenantID, email, resetLink string) error {
	return nil
}
