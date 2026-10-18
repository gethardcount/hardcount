package tenant

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

var nonBareEmails = []string{
	`"x@evil.example> NOTIFY=NEVER"@example.com`,
	`"billing"@acme.test`,
	`Acme Billing <billing@acme.test>`,
	`<billing@acme.test>`,
	`billing@acme.test (Acme)`,
	`billing@acme.test, ar@acme.test`,
}

// TestValidateSettings_CompanyEmailIsOneBareAddress: company_email gets the
// same check as every other address. The old hand check wanted only an "@"
// and a dot after it, so "Acme Billing <billing@acme.test>" was stored.
func TestValidateSettings_CompanyEmailIsOneBareAddress(t *testing.T) {
	for _, in := range append(nonBareEmails, "billing@localhost") {
		err := validateSettings(&domain.TenantSettings{CompanyEmail: in})
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "company_email" {
			t.Errorf("company_email %q: err %v, want a company_email validation error", in, err)
		}
	}

	// Control: a bare address is stored trimmed; empty stays allowed.
	ts := &domain.TenantSettings{CompanyEmail: " billing@acme.test "}
	if err := validateSettings(ts); err != nil {
		t.Fatalf("bare company_email refused: %v", err)
	}
	if ts.CompanyEmail != "billing@acme.test" {
		t.Errorf("stored company_email %q, want %q", ts.CompanyEmail, "billing@acme.test")
	}
	if err := validateSettings(&domain.TenantSettings{CompanyEmail: ""}); err != nil {
		t.Errorf("empty company_email refused: %v", err)
	}
}

// TestRunBootstrap_OwnerEmailIsOneBareAddress: the owner email is the
// dashboard sign-in and the To address of password-reset emails. It used to
// need only an "@". The check runs before any write, so the database is
// never reached and a nil one is enough here.
func TestRunBootstrap_OwnerEmailIsOneBareAddress(t *testing.T) {
	errPastEmail := errors.New("reached the password step")
	deps := BootstrapDeps{
		HashPassword: func(string) (string, error) { return "", errPastEmail },
		CreateUserTx: func(context.Context, *sql.Tx, string, string, string, string) (domain.User, error) {
			return domain.User{}, errors.New("must not be reached")
		},
		SeedDunningTx: func(context.Context, *sql.Tx, string, func(domain.DunningPolicy, bool) error) error {
			return errors.New("must not be reached")
		},
	}
	run := func(email string) error {
		_, err := RunBootstrap(context.Background(), nil, deps, BootstrapOpts{OwnerEmail: email, OwnerPassword: "a-strong-password-12ch"})
		return err
	}

	for _, in := range append(nonBareEmails, "tenant-b@local") {
		if err := run(in); !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "owner_email" {
			t.Errorf("owner_email %q: err %v, want an owner_email validation error", in, err)
		}
	}

	// Control: a bare address, and the default, pass the check and reach
	// the password step.
	for _, in := range []string{"owner@acme.test", ""} {
		if err := run(in); !errors.Is(err, errPastEmail) {
			t.Errorf("owner_email %q: err %v, want to reach the password step", in, err)
		}
	}
}
