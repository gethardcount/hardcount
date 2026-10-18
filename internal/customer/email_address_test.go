package customer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// TestCustomerEmail_RefusesNonBareAddress: the customer's email is the To
// address of every billing email, so it must be one bare address. Before,
// anything net/mail could parse was stored as typed, display name and
// quotes included.
func TestCustomerEmail_RefusesNonBareAddress(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newMemoryStore())
	nonBare := []string{
		`"x@evil.example> NOTIFY=NEVER"@example.com`,
		`"bob"@acme.test`,
		`Bob <bob@acme.test>`,
		`<bob@acme.test>`,
		`bob@acme.test (Bob)`,
	}

	for i, in := range nonBare {
		_, err := svc.Create(ctx, "t1", CreateInput{ExternalID: fmt.Sprintf("cus_bad_%d", i), DisplayName: "Bad", Email: in})
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "email" {
			t.Errorf("create with %q: err %v, want an email validation error", in, err)
		}
	}

	// Control: a bare address is stored, trimmed.
	c, err := svc.Create(ctx, "t1", CreateInput{ExternalID: "cus_ok", DisplayName: "OK", Email: " ap@acme.test "})
	if err != nil {
		t.Fatalf("create with a bare address: %v", err)
	}
	if c.Email != "ap@acme.test" {
		t.Errorf("stored email %q, want %q", c.Email, "ap@acme.test")
	}

	for _, in := range nonBare {
		_, err := svc.Update(ctx, "t1", c.ID, UpdateInput{Email: in})
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "email" {
			t.Errorf("update with %q: err %v, want an email validation error", in, err)
		}
	}
	if got, _ := svc.Get(ctx, "t1", c.ID); got.Email != "ap@acme.test" {
		t.Errorf("a refused update changed the stored email to %q", got.Email)
	}

	// Control: a bare address updates.
	u, err := svc.Update(ctx, "t1", c.ID, UpdateInput{Email: "billing@acme.test"})
	if err != nil || u.Email != "billing@acme.test" {
		t.Errorf("update with a bare address: email %q, err %v", u.Email, err)
	}

	// The CC list goes through the same check.
	_, err = svc.Create(ctx, "t1", CreateInput{
		ExternalID: "cus_cc_bad", DisplayName: "CC", Email: "ap@acme.test",
		AdditionalEmails: []string{`"x@evil.example> NOTIFY=NEVER"@example.com`},
	})
	if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "additional_emails" {
		t.Errorf("quoted CC entry: err %v, want an additional_emails validation error", err)
	}
}
