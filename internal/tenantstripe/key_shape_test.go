package tenantstripe

import (
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// Every key below is an obviously fake string that carries only the real
// prefix. validateKeyShape reads nothing but the prefix, so nothing more is
// needed, and a secret scanner has nothing to flag.
const (
	fakeSKTest = "sk_test_FAKE_not_a_key"
	fakeSKLive = "sk_live_FAKE_not_a_key"
	fakeRKTest = "rk_test_FAKE_not_a_key"
	fakeRKLive = "rk_live_FAKE_not_a_key"
	fakePKTest = "pk_test_FAKE_not_a_key"
	fakePKLive = "pk_live_FAKE_not_a_key"
	fakeWHSec  = "whsec_FAKE_not_a_secret"
)

// TestValidateKeyShape pins the check that keeps a key of one mode out of the
// other mode's row. The store writes whatever it is handed under the requested
// livemode, so this function is the only place a live key is refused for the
// test-mode row (and a test key for the live row).
func TestValidateKeyShape(t *testing.T) {
	cases := []struct {
		name        string
		livemode    bool
		secret      string
		publishable string
		webhook     string
		wantField   string // "" means accepted
	}{
		// Accepted: test mode.
		{name: "test: secret + publishable", secret: fakeSKTest, publishable: fakePKTest},
		{name: "test: restricted + publishable", secret: fakeRKTest, publishable: fakePKTest},
		{name: "test: with webhook secret", secret: fakeSKTest, publishable: fakePKTest, webhook: fakeWHSec},

		// Accepted: live mode.
		{name: "live: secret + publishable", livemode: true, secret: fakeSKLive, publishable: fakePKLive},
		{name: "live: restricted + publishable", livemode: true, secret: fakeRKLive, publishable: fakePKLive},
		{name: "live: with webhook secret", livemode: true, secret: fakeSKLive, publishable: fakePKLive, webhook: fakeWHSec},

		// Refused: a key of the other mode.
		{name: "test: live secret", secret: fakeSKLive, publishable: fakePKTest, wantField: "secret_key"},
		{name: "test: live restricted", secret: fakeRKLive, publishable: fakePKTest, wantField: "secret_key"},
		{name: "test: live publishable", secret: fakeSKTest, publishable: fakePKLive, wantField: "publishable_key"},
		{name: "test: live secret and live publishable", secret: fakeSKLive, publishable: fakePKLive, wantField: "secret_key"},
		{name: "live: test secret", livemode: true, secret: fakeSKTest, publishable: fakePKLive, wantField: "secret_key"},
		{name: "live: test restricted", livemode: true, secret: fakeRKTest, publishable: fakePKLive, wantField: "secret_key"},
		{name: "live: test publishable", livemode: true, secret: fakeSKLive, publishable: fakePKTest, wantField: "publishable_key"},
		{name: "live: test secret and test publishable", livemode: true, secret: fakeSKTest, publishable: fakePKTest, wantField: "secret_key"},

		// Refused: a key of the wrong kind in a field.
		{name: "publishable key in the secret field", secret: fakePKTest, publishable: fakePKTest, wantField: "secret_key"},
		{name: "webhook secret in the secret field", secret: fakeWHSec, publishable: fakePKTest, wantField: "secret_key"},
		{name: "secret key in the publishable field", secret: fakeSKTest, publishable: fakeSKTest, wantField: "publishable_key"},
		{name: "restricted key in the publishable field", secret: fakeSKTest, publishable: fakeRKTest, wantField: "publishable_key"},
		{name: "secret key in the webhook field", secret: fakeSKTest, publishable: fakePKTest, webhook: fakeSKTest, wantField: "webhook_secret"},
		{name: "live: secret key in the webhook field", livemode: true, secret: fakeSKLive, publishable: fakePKLive, webhook: fakeSKLive, wantField: "webhook_secret"},

		// Refused: missing or malformed.
		{name: "empty secret", publishable: fakePKTest, wantField: "secret_key"},
		{name: "empty publishable", secret: fakeSKTest, wantField: "publishable_key"},
		{name: "prefix in the wrong case", secret: "SK_TEST_FAKE_not_a_key", publishable: fakePKTest, wantField: "secret_key"},
		{name: "prefix not at the start", secret: "x" + fakeSKTest, publishable: fakePKTest, wantField: "secret_key"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateKeyShape(tc.secret, tc.publishable, tc.webhook, tc.livemode)
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want refused on %q, got accepted", tc.wantField)
			}
			if !errors.Is(err, errs.ErrValidation) {
				t.Errorf("want a validation error, got %v", err)
			}
			if got := errs.Field(err); got != tc.wantField {
				t.Errorf("refused on field %q, want %q", got, tc.wantField)
			}
		})
	}
}
