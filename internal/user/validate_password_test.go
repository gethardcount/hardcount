package user

import (
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/errs"
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
