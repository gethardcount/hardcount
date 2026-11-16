package session

import (
	"context"
	"database/sql"
	"time"
)

// Service is the auth-flow facade over Store. Issue mints a session for an
// authenticated user (the caller has already verified the password).
// Resolve looks up a raw cookie value and returns the Session if it is still
// active. Revoke handles logout.
type Service struct {
	store Store
}

// NewService returns a Service backed by store. Sessions last DefaultTTL.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// IssueInput is the contract for Issue and IssueTx: everything needed to
// mint a session row for a user whose password the caller has verified.
type IssueInput struct {
	UserID    string
	TenantID  string
	Livemode  bool
	UserAgent string
	IP        string
}

// Issue creates a session row and returns the raw cookie value the caller
// should set on the response. The raw value is shown to the caller exactly
// once. The database stores sha256(raw), so a snapshot cannot be replayed.
func (s *Service) Issue(ctx context.Context, in IssueInput) (rawID string, sess Session, err error) {
	rawID, sess, err = newSession(in)
	if err != nil {
		return "", Session{}, err
	}
	if err := s.store.Insert(ctx, sess); err != nil {
		return "", Session{}, err
	}
	return rawID, sess, nil
}

// IssueTx is Issue on the caller's transaction, so the session commits or
// rolls back with the caller's change. It does not commit. tx must be a
// bypass transaction: a session row is written before any tenant context
// exists.
func (s *Service) IssueTx(ctx context.Context, tx *sql.Tx, in IssueInput) (rawID string, sess Session, err error) {
	rawID, sess, err = newSession(in)
	if err != nil {
		return "", Session{}, err
	}
	if err := s.store.InsertTx(ctx, tx, sess); err != nil {
		return "", Session{}, err
	}
	return rawID, sess, nil
}

// newSession returns a new raw cookie value and the session row for it.
func newSession(in IssueInput) (string, Session, error) {
	rawID, err := newRawID()
	if err != nil {
		return "", Session{}, err
	}
	now := time.Now().UTC()
	return rawID, Session{
		IDHash:     HashID(rawID),
		UserID:     in.UserID,
		TenantID:   in.TenantID,
		Livemode:   in.Livemode,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(DefaultTTL),
		UserAgent:  in.UserAgent,
		IP:         in.IP,
	}, nil
}

// Resolve looks up a raw cookie value and returns the Session if it is
// active. Revoked or expired rows return ErrNotFound. The middleware turns
// both into a 401, so session ids cannot be enumerated.
func (s *Service) Resolve(ctx context.Context, rawID string) (Session, error) {
	if rawID == "" {
		return Session{}, ErrNotFound
	}
	sess, err := s.store.GetByIDHash(ctx, HashID(rawID))
	if err != nil {
		return Session{}, err
	}
	if !sess.IsActive(time.Now()) {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

// Revoke marks the session row as revoked. It is idempotent: revoking an
// already-revoked or missing row does nothing.
func (s *Service) Revoke(ctx context.Context, rawID string) error {
	if rawID == "" {
		return nil
	}
	return s.store.Revoke(ctx, HashID(rawID))
}

// EndTx ends the session whose raw cookie value is rawID on the caller's
// transaction, whoever it belongs to, so the sign-out commits or rolls back
// with the caller's change. An empty, unknown or already ended rawID ends
// nothing. It does not commit. tx must be a bypass transaction: a tenant
// transaction's row-level security hides the session in the other mode.
func (s *Service) EndTx(ctx context.Context, tx *sql.Tx, rawID string) error {
	if rawID == "" {
		return nil
	}
	return s.store.RevokeTx(ctx, tx, HashID(rawID))
}

// EndAllForUserTx ends every session of a user, in every workspace and mode,
// on the caller's transaction, so the sign-out commits or rolls back with the
// caller's change. It does not commit. tx must be a bypass transaction: a
// tenant transaction's row-level security hides the user's sessions in
// other workspaces and in the other mode.
func (s *Service) EndAllForUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	return s.store.EndAllForUserTx(ctx, tx, userID)
}

// EndForMemberTx ends every session of a user in one workspace, in both
// modes, on the caller's transaction, so the sign-out commits or rolls back
// with the caller's change. Sessions in other workspaces are not touched. It
// does not commit. tx must be a bypass transaction: a tenant transaction's
// row-level security hides the user's sessions in the other mode.
func (s *Service) EndForMemberTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	return s.store.EndForMemberTx(ctx, tx, userID, tenantID)
}

// SetLivemode switches the active mode (test or live) on the cookie session.
// The same operator switches modes without signing in again, and every later
// request gets the new mode through session.Resolve.
func (s *Service) SetLivemode(ctx context.Context, rawID string, livemode bool) error {
	if rawID == "" {
		return ErrNotFound
	}
	return s.store.UpdateLivemode(ctx, HashID(rawID), livemode)
}
