package breachedpw

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Contains is a binary search, so it is only right while the embedded
// file stays in the shape the generator writes. A hand edit that breaks
// the order would make some entries unfindable without any other error.
func TestListShape(t *testing.T) {
	all := entries()
	if len(all) < 20000 {
		t.Fatalf("list has %d entries; the generated file has about 30,000", len(all))
	}
	if !strings.HasSuffix(list, "\n") || strings.HasSuffix(list, "\n\n") {
		t.Error("passwords.txt must end with exactly one newline")
	}
	for i, e := range all {
		if !utf8.ValidString(e) || Normalize(e) != e {
			t.Errorf("entry %d %q is not valid lower-case UTF-8", i, e)
		}
		if len(e) < MinLength {
			t.Errorf("entry %d %q is shorter than %d bytes", i, e, MinLength)
		}
		if i > 0 && all[i-1] >= e {
			t.Fatalf("entries %d and %d are out of order or repeated: %q, %q", i-1, i, all[i-1], e)
		}
	}
}

func TestContains(t *testing.T) {
	for _, pw := range []string{"password1234", "PASSWORD1234", "PassWord1234", "1q2w3e4r5t6y"} {
		if !Contains(pw) {
			t.Errorf("Contains(%q) = false, want true", pw)
		}
	}
	// The first and last entries, to catch an off-by-one in the search.
	all := entries()
	for _, pw := range []string{all[0], all[len(all)-1]} {
		if !Contains(pw) {
			t.Errorf("Contains(%q) = false, want true", pw)
		}
	}
	for _, pw := range []string{"", "tangerine-orbit-quietly-47", "password123"} {
		if Contains(pw) {
			t.Errorf("Contains(%q) = true, want false", pw)
		}
	}
}
