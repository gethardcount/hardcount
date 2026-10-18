package domain

import (
	"errors"
	"reflect"
	"testing"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// TestNormalizeAdditionalEmails_RefusesNonBareAddress: a CC entry must be
// one bare address. net/mail accepts a quoted local part, a display name,
// angle brackets and a trailing comment, and returns an Address that is not
// what the operator typed (a quoted local part comes back unquoted).
func TestNormalizeAdditionalEmails_RefusesNonBareAddress(t *testing.T) {
	for _, in := range []string{
		`"x@evil.example> NOTIFY=NEVER"@example.com`,
		`"bob"@acme.test`,
		`Bob <bob@acme.test>`,
		`<bob@acme.test>`,
		`bob@acme.test (Bob)`,
	} {
		got, err := NormalizeAdditionalEmails([]string{in}, "ap@acme.test")
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "additional_emails" {
			t.Errorf("%q: got %v, err %v; want an additional_emails validation error", in, got, err)
		}
	}

	// Control: bare addresses, including the unusual but valid characters
	// a dot-atom allows, are kept, trimmed and lowercased.
	got, err := NormalizeAdditionalEmails([]string{" Finance@Acme.Test ", "o'neil+ar@acme.co.uk"}, "ap@acme.test")
	if err != nil {
		t.Fatalf("bare addresses refused: %v", err)
	}
	if want := []string{"finance@acme.test", "o'neil+ar@acme.co.uk"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestValidateEmail is the one check every address goes through. It
// returns the address net/mail parsed, which callers store and send to.
func TestValidateEmail(t *testing.T) {
	for in, want := range map[string]string{
		"ap@acme.test":         "ap@acme.test",
		"  Ap@Acme.Test  ":     "Ap@Acme.Test", // trimmed; case kept (callers lowercase where they dedupe)
		"o'neil+ar@acme.co.uk": "o'neil+ar@acme.co.uk",
		"josé@exämple.de":      "josé@exämple.de", // RFC 6532 UTF-8, accepted before and after
	} {
		got, err := ValidateEmail("email", in)
		if err != nil || got != want {
			t.Errorf("ValidateEmail(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"",
		"not an address",
		`"x@evil.example> NOTIFY=NEVER"@example.com`,
		`"bob"@acme.test`,
		"Bob <bob@acme.test>",
		"<bob@acme.test>",
		"bob@acme.test (Bob)",
		"bob@acme.test, eve@acme.test",
		"bob@acme.test\r\nBcc: v@evil.example",
		"bob@localhost",
	} {
		got, err := ValidateEmail("email", in)
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "email" || got != "" {
			t.Errorf("ValidateEmail(%q) = %q, %v; want an email validation error", in, got, err)
		}
	}
}
