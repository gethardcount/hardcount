package errs

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClip(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{"short text is kept", "card declined", 500, "card declined"},
		{"text of exactly the limit is kept", "abcde", 5, "abcde"},
		{"ASCII is cut at the limit", "abcdefgh", 5, "abcde"},
		// "é" is 2 bytes (c3 a9) and takes bytes 4 and 5: a cut at 5 would
		// keep only c3.
		{"a 2-byte character across the cut is left out", "abcdé", 5, "abcd"},
		// "€" is 3 bytes and takes bytes 3 to 5.
		{"a 3-byte character across the cut is left out", "abc€", 5, "abc"},
		// "😀" is 4 bytes and takes bytes 2 to 5.
		{"a 4-byte character across the cut is left out", "ab😀", 5, "ab"},
		{"a character that ends at the cut is kept", "abcé", 5, "abcé"},
		{"a limit inside the first character gives empty text", "€uro", 2, ""},
		{"a limit of 0 gives empty text", "abc", 0, ""},
		{"a negative limit gives empty text", "abc", -1, ""},
		{"empty text stays empty", "", 10, ""},
		{"an invalid byte becomes U+FFFD", "a\xffb", 10, "a\uFFFDb"},
		{"a run of invalid bytes becomes one U+FFFD", "a\xff\xfe\xfdb", 10, "a\uFFFDb"},
		{"a NUL byte becomes U+FFFD", "a\x00b", 10, "a\uFFFDb"},
		// The input's last byte starts a character the input does not
		// finish: it is invalid, so it is replaced, and the cut then leaves
		// the replacement out.
		{"a character the input cut is replaced, then cut", "abcd\xc3", 5, "abcd"},
		// The U+FFFD for the invalid byte takes bytes 1 to 3, so a cut at 2
		// keeps only "a".
		{"the cut never splits a U+FFFD", "a\xffbc", 2, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Clip(tc.in, tc.maxBytes)
			if got != tc.want {
				t.Errorf("Clip(%q, %d) = %q, want %q", tc.in, tc.maxBytes, got, tc.want)
			}
			if !utf8.ValidString(got) || strings.Contains(got, "\x00") || len(got) > max(tc.maxBytes, 0) {
				t.Errorf("Clip(%q, %d) = %q: not valid UTF-8 without NUL, at most %d bytes", tc.in, tc.maxBytes, got, tc.maxBytes)
			}
		})
	}
}

// TestClip_EveryCutIsStorable cuts text that mixes 1- to 4-byte characters,
// invalid bytes and NUL bytes at every length, and checks each result is
// storable and as long as the limit allows.
func TestClip_EveryCutIsStorable(t *testing.T) {
	in := "aé€😀\xff\x00b" + strings.Repeat("ü😀x", 20)
	clean := strings.ReplaceAll(strings.ToValidUTF8(in, "\uFFFD"), "\x00", "\uFFFD")
	for n := 0; n <= len(clean)+1; n++ {
		got := Clip(in, n)
		if !utf8.ValidString(got) || strings.Contains(got, "\x00") || len(got) > n {
			t.Fatalf("Clip(in, %d) = %q: not valid UTF-8 without NUL, at most %d bytes", n, got, n)
		}
		if !strings.HasPrefix(clean, got) {
			t.Fatalf("Clip(in, %d) = %q, not a prefix of the cleaned input", n, got)
		}
		// Nothing more fits: the next character would pass the limit.
		if rest := clean[len(got):]; rest != "" {
			_, size := utf8.DecodeRuneInString(rest)
			if len(got)+size <= n {
				t.Fatalf("Clip(in, %d) = %q, left out a character that fits", n, got)
			}
		}
	}
}
