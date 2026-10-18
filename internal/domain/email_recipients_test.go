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
// what the operator typed: the quoted local part
// "x@evil.example> NOTIFY=NEVER" comes back unquoted, and SMTP then reads
// it as RCPT TO:<x@evil.example> plus a parameter.
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
