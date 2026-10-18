package dashmembers

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// TestInvite_RefusesNonBareEmail: the invite address becomes the new
// member's sign-in email and the To address of the invite email, so it gets
// the same check as a customer's email. Before, anything net/mail could
// parse was accepted, display name and quotes included. The check runs
// before the store and the server-settings refusals, so a nil store is
// enough here.
func TestInvite_RefusesNonBareEmail(t *testing.T) {
	ctx := context.Background()
	svc := NewService(nil, nil, nil, nil, nil, nil, "")
	for _, in := range []string{
		`"x@evil.example> NOTIFY=NEVER"@example.com`,
		`"bob"@acme.test`,
		`Bob <bob@acme.test>`,
		`<bob@acme.test>`,
		`bob@acme.test (Bob)`,
		`bob@localhost`,
	} {
		_, err := svc.Invite(ctx, "t1", "u1", in)
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "email" {
			t.Errorf("invite %q: err %v, want an email validation error", in, err)
		}
	}

	// Control: a bare address passes the check and reaches the next
	// refusal (this server has no dashboard address).
	if _, err := svc.Invite(ctx, "t1", "u1", " Bob@Example.com "); !errors.Is(err, errs.ErrInvalidState) {
		t.Errorf("invite with a bare address: err %v, want the invalid-state refusal that follows the check", err)
	}
}
