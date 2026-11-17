package tenantstripe

import (
	"context"
	"errors"
	"testing"

	hardcountauth "github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

var (
	aKey     = hardcountauth.Caller{KeyID: "hc_key_1", KeyType: hardcountauth.KeyTypeSecret}
	aSession = hardcountauth.Caller{UserID: "hc_usr_1"}
)

// inMode is a request context for the caller c in the mode live.
func inMode(c hardcountauth.Caller, live bool) context.Context {
	return hardcountauth.WithCaller(postgres.WithLivemode(context.Background(), live), c)
}

// TestCheckMode pins who manages which mode's Stripe connection. Every caller
// manages only the connection of its own mode: an API key the mode it was
// made in, a dashboard session the mode it is in. A caller with no mode on its
// ctx is refused.
func TestCheckMode(t *testing.T) {
	cases := []struct {
		name   string
		ctx    context.Context
		target bool
		want   error
	}{
		{"test key, test connection", inMode(aKey, false), false, nil},
		{"test key, live connection", inMode(aKey, false), true, errs.ErrForbidden},
		{"live key, live connection", inMode(aKey, true), true, nil},
		{"live key, test connection", inMode(aKey, true), false, errs.ErrForbidden},
		{"test-mode session, test connection", inMode(aSession, false), false, nil},
		{"test-mode session, live connection", inMode(aSession, false), true, errs.ErrForbidden},
		{"live-mode session, live connection", inMode(aSession, true), true, nil},
		{"live-mode session, test connection", inMode(aSession, true), false, errs.ErrForbidden},
		{"key with no mode", hardcountauth.WithCaller(context.Background(), aKey), false, postgres.ErrLivemodeUnset},
		{"session with no mode", hardcountauth.WithCaller(context.Background(), aSession), true, postgres.ErrLivemodeUnset},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkMode(tc.ctx, tc.target)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("checkMode = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("checkMode = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestService_RefusesOtherModeBeforeWriting: each write the service takes from
// a request checks the mode before anything else, for a key and a session
// alike. The service here has no store, so a write that skipped the check
// would reach the nil store and panic instead of returning ErrForbidden.
func TestService_RefusesOtherModeBeforeWriting(t *testing.T) {
	svc := NewService(nil, nil)
	const tenantID = "hc_ten_mode_scope"
	for _, tc := range []struct {
		name   string
		caller hardcountauth.Caller
	}{{"test key", aKey}, {"test-mode session", aSession}} {
		ctx := inMode(tc.caller, false)
		_, err := svc.Connect(ctx, tenantID, ConnectInput{Livemode: true, SecretKey: fakeSKLive, PublishableKey: fakePKLive})
		if !errors.Is(err, errs.ErrForbidden) {
			t.Errorf("Connect live with a %s = %v, want ErrForbidden", tc.name, err)
		}
		if _, err := svc.SetWebhookSecret(ctx, tenantID, true, fakeWHSec); !errors.Is(err, errs.ErrForbidden) {
			t.Errorf("SetWebhookSecret live with a %s = %v, want ErrForbidden", tc.name, err)
		}
		if err := svc.Delete(ctx, tenantID, true); !errors.Is(err, errs.ErrForbidden) {
			t.Errorf("Delete live with a %s = %v, want ErrForbidden", tc.name, err)
		}
	}
}
