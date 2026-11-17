package session

import (
	"context"
	"database/sql"
	"time"
)

// Service issues, resolves and ends sessions, and switches their mode, over a
// Store.
type Service struct {
	store Store
}

// NewService returns a Service backed by store. Sessions last DefaultTTL.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// IssueInput is what IssueTx needs to mint a session for a user whose
// password the caller has verified.
type IssueInput struct {
	UserID    string
	TenantID  string
	Livemode  bool
	UserAgent string
	IP        string
}

// IssueTx creates a session row on the caller's transaction, so it commits or
// rolls back with the caller's change, and returns the raw cookie value: the
// only time it is shown. It does not commit. tx must be a bypass transaction:
// a session row is written before any tenant context exists.
func (s *Service) IssueTx(ctx context.Context, tx *sql.Tx, in IssueInput) (rawID string, sess Session, err error) {
	rawID, err = newRawID()
	if err != nil {
		return "", Session{}, err
	}
	now := time.Now().UTC()
	sess = Session{
		IDHash:    HashID(rawID),
		UserID:    in.UserID,
		TenantID:  in.TenantID,
		Livemode:  in.Livemode,
		CreatedAt: now,
		ExpiresAt: now.Add(DefaultTTL),
		UserAgent: in.UserAgent,
		IP:        in.IP,
	}
	if err := s.store.InsertTx(ctx, tx, sess); err != nil {
		return "", Session{}, err
	}
	return rawID, sess, nil
}

// Resolve returns the live session for a raw cookie value. An empty, unknown,
// revoked or expired value returns ErrNotFound, which Authenticate treats as
// no cookie, so these cases cannot be told apart and session ids cannot be
// enumerated.
func (s *Service) Resolve(ctx context.Context, rawID string) (Session, error) {
	if rawID == "" {
		return Session{}, ErrNotFound
	}
	sess, err := s.store.GetByIDHash(ctx, HashID(rawID))
	if err != nil {
		return Session{}, err
	}
	if sess.RevokedAt != nil || !time.Now().UTC().Before(sess.ExpiresAt) {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

// End ends the live session whose raw cookie value is rawID and calls then on
// the same bypass transaction, so the sign-out and then's writes, such as its
// audit row, commit or roll back together. then must not commit or roll back
// tx; its error rolls the sign-out back and is returned. End returns the
// ended session and whether a live one was found; with none, nothing changes
// and then is not called.
func (s *Service) End(ctx context.Context, rawID string, then func(tx *sql.Tx, sess Session) error) (Session, bool, error) {
	if rawID == "" {
		return Session{}, false, nil
	}
	return s.store.EndWith(ctx, HashID(rawID), then)
}

// EndTx ends the live session whose raw cookie value is rawID, whoever it
// belongs to, on the caller's transaction, and reports whether it ended one.
// It does not commit. tx must be a bypass transaction, to reach the session
// whoever it belongs to, in either mode.
func (s *Service) EndTx(ctx context.Context, tx *sql.Tx, rawID string) (bool, error) {
	if rawID == "" {
		return false, nil
	}
	return s.store.EndTx(ctx, tx, HashID(rawID))
}

// EndAllForUserTx ends every session of a user, in every workspace and mode,
// on the caller's transaction. It does not commit. tx must be a bypass
// transaction: a tenant transaction's row-level security hides the user's
// sessions in other workspaces and in the other mode.
func (s *Service) EndAllForUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	return s.store.EndAllForUserTx(ctx, tx, userID)
}

// EndForMemberTx ends every session of a user in one workspace, in both
// modes, on the caller's transaction; other workspaces are not touched. It
// does not commit. tx must be a bypass transaction: a tenant transaction's
// row-level security hides the user's sessions in the other mode.
func (s *Service) EndForMemberTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	return s.store.EndForMemberTx(ctx, tx, userID, tenantID)
}

// SetLivemode switches the live session whose raw cookie value is rawID to
// livemode and calls then on the same transaction, under End's contract. It
// returns the session in its new mode and whether a live one was found. A
// session already in that mode is returned as it is, and then is not called:
// nothing changed, so there is nothing to record.
func (s *Service) SetLivemode(ctx context.Context, rawID string, livemode bool, then func(tx *sql.Tx, sess Session) error) (Session, bool, error) {
	if rawID == "" {
		return Session{}, false, nil
	}
	return s.store.SetLivemodeWith(ctx, HashID(rawID), livemode, then)
}
