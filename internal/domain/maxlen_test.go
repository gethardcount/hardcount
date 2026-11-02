package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/errs"
)

func TestMaxLen_CountsCharactersNotBytes(t *testing.T) {
	cases := []struct {
		name  string
		value string
		max   int
		want  bool // true = error expected
	}{
		{"ascii under", strings.Repeat("a", 10), 10, false},
		{"ascii over", strings.Repeat("a", 11), 10, true},
		// 10 Japanese letters = 30 UTF-8 bytes; must pass a max of 10 characters.
		{"multibyte under", strings.Repeat("あ", 10), 10, false},
		{"multibyte over", strings.Repeat("あ", 11), 10, true},
		// Accented Latin: 10 letters, 20 bytes.
		{"accented under", strings.Repeat("é", 10), 10, false},
		{"accented over", strings.Repeat("é", 11), 10, true},
		{"empty", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := MaxLen("display_name", tc.value, tc.max)
			if tc.want {
				if !errors.Is(err, errs.ErrValidation) {
					t.Fatalf("MaxLen(%q, %d) = %v, want validation error", tc.value, tc.max, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("MaxLen(%q, %d) = %v, want nil", tc.value, tc.max, err)
			}
		})
	}
}
