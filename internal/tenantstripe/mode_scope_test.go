package tenantstripe

import (
	"context"
	"errors"
	"testing"

	hardcountauth "github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

func principal(kt hardcountauth.KeyType, live bool) context.Context {
	return hardcountauth.WithKeyType(postgres.WithLivemode(context.Background(), live), kt)
}

// TestCheckMode pins who manages which mode's Stripe connection. A dashboard
// session manages both modes, from either mode. An API key manages only its
// own mode. A caller with no mode on its ctx is refused.
func TestCheckMode(t *testing.T) {
	cases := []struct {
		name   string
		ctx    context.Context
		target bool
		want   error
	}{
		{"test key, test connection", principal(hardcountauth.KeyTypeSecret, false), false, nil},
		{"test key, live connection", principal(hardcountauth.KeyTypeSecret, false), true, errs.ErrForbidden},
		{"live key, live connection", principal(hardcountauth.KeyTypeSecret, true), true, nil},
		{"live key, test connection", principal(hardcountauth.KeyTypeSecret, true), false, errs.ErrForbidden},
		{"test-mode session, live connection", principal(hardcountauth.KeyTypeSession, false), true, nil},
		{"live-mode session, test connection", principal(hardcountauth.KeyTypeSession, true), false, nil},
		{"key with no mode", hardcountauth.WithKeyType(context.Background(), hardcountauth.KeyTypeSecret), false, postgres.ErrLivemodeUnset},
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
// a request checks the mode before anything else. The service here has no
// store, so a write that skipped the check would reach the nil store and
// panic instead of returning ErrForbidden.
func TestService_RefusesOtherModeBeforeWriting(t *testing.T) {
	svc := NewService(nil, nil)
	ctx := principal(hardcountauth.KeyTypeSecret, false)
	const tenantID = "hc_ten_mode_scope"

	_, err := svc.Connect(ctx, tenantID, ConnectInput{Livemode: true, SecretKey: fakeSKLive, PublishableKey: fakePKLive})
	if !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("Connect live with a test key = %v, want ErrForbidden", err)
	}
	if _, err := svc.SetWebhookSecret(ctx, tenantID, true, fakeWHSec); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("SetWebhookSecret live with a test key = %v, want ErrForbidden", err)
	}
	if err := svc.Delete(ctx, tenantID, true); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("Delete live with a test key = %v, want ErrForbidden", err)
	}
	if _, err := svc.List(hardcountauth.WithKeyType(context.Background(), hardcountauth.KeyTypeSecret), tenantID); !errors.Is(err, postgres.ErrLivemodeUnset) {
		t.Errorf("List with no mode on ctx = %v, want ErrLivemodeUnset", err)
	}
}
