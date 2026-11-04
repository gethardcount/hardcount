package session

import (
	"context"
	"time"
)

// Service is the auth-flow facade over Store. Issue mints a session for an
// authenticated user (the caller has already verified the password).
// Resolve looks up a raw cookie value and returns the Session if it is still
// active. Revoke handles logout.
type Service struct {
	store Store
	now   func() time.Time
	ttl   time.Duration
}

// NewService wires defaults: real wall clock, DefaultTTL.
func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now, ttl: DefaultTTL}
}

// IssueInput is the contract for Issue. Callers (the login handler) pass
// everything needed to mint a session row. The user has already been
// authenticated by user.Service.Authenticate.
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
	rawID, err = newRawID()
	if err != nil {
		return "", Session{}, err
	}
	now := s.now().UTC()
	sess = Session{
		IDHash:     HashID(rawID),
		UserID:     in.UserID,
		TenantID:   in.TenantID,
		Livemode:   in.Livemode,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(s.ttl),
		UserAgent:  in.UserAgent,
		IP:         in.IP,
	}
	if err := s.store.Insert(ctx, sess); err != nil {
		return "", Session{}, err
	}
	return rawID, sess, nil
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
	if !sess.IsActive(s.now()) {
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

// RevokeAllForUser revokes every active session of a user. The
// password-reset flow calls it after the new password is set, so a session
// minted from a stolen cookie cannot outlive the credential change. It is
// idempotent: with no active sessions it does nothing.
func (s *Service) RevokeAllForUser(ctx context.Context, userID string) error {
	if userID == "" {
		return nil
	}
	return s.store.RevokeAllForUser(ctx, userID)
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
