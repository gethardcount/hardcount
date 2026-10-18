package user

import (
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/user/breachedpw"
)

// The reset-password and accept-invite pages show these reasons after the
// word "Password", so each must read as the rest of that sentence, and the
// 72 limit must be stated.
func TestValidatePasswordReasons(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		want string
	}{
		{"too short", "short", "must be at least 12 characters"},
		{"common", "Administrator", "is one of the most common passwords. Pick a different one"},
		{"too long in ASCII", strings.Repeat("a", 73), "is too long. Use at most 72 characters; accented and non-Latin letters count as two or more"},
		// 37 accented letters are 74 bytes: refused although only 37 characters.
		{"too long in bytes", strings.Repeat("é", 37), "is too long. Use at most 72 characters; accented and non-Latin letters count as two or more"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidatePassword(c.pw)
			if err == nil || err.Error() != c.want || errs.Field(err) != "password" {
				t.Fatalf("ValidatePassword = %v (field %q), want %q on password", err, errs.Field(err), c.want)
			}
		})
	}
	if err := ValidatePassword(strings.Repeat("a", 72)); err != nil && strings.Contains(err.Error(), "too long") {
		t.Errorf("72 ASCII characters refused as too long: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("é", 36)); err != nil && strings.Contains(err.Error(), "too long") {
		t.Errorf("36 accented letters (72 bytes) refused as too long: %v", err)
	}
}

// ADR-095 Phase 0(a): passwords of 12 or more characters that appear in
// the top million of a breach corpus are refused, whatever their case.
// The hand-picked 10-entry list this replaced let all of these through.
func TestValidatePasswordRefusesBreached(t *testing.T) {
	const want = "is one of the most common passwords. Pick a different one"
	for _, pw := range []string{
		"Password1234", // ADR-095's own example
		"qwertyuiop123",
		"1q2w3e4r5t6y",
		"Q1W2E3R4T5Y6",
		"iloveyouforever",
		"123qweasdzxc",
		"admin12345678", // in the old hand-picked list, not in the corpus
	} {
		err := ValidatePassword(pw)
		if err == nil || err.Error() != want || errs.Field(err) != "password" {
			t.Errorf("ValidatePassword(%q) = %v, want %q on password", pw, err, want)
		}
	}
	// The list drops entries shorter than its own minimum. If that ever
	// rose above MinPasswordLength, passwords in between would go unchecked.
	if breachedpw.MinLength != MinPasswordLength {
		t.Errorf("breachedpw.MinLength = %d, MinPasswordLength = %d: change MinLength and rebuild the list (see the breachedpw package comment)",
			breachedpw.MinLength, MinPasswordLength)
	}
	// Control: passwords that are not in the list still pass.
	for _, pw := range []string{"tangerine-orbit-quietly-47", strings.Repeat("a", 72)} {
		if err := ValidatePassword(pw); err != nil {
			t.Errorf("ValidatePassword(%q) = %v, want nil", pw, err)
		}
	}
}
