package errs

import (
	"strings"
	"unicode/utf8"
)

// Clip makes s safe to store in a Postgres text column and cuts it to at most
// maxBytes bytes. Use it for every text Hardcount shortens before it stores
// or sends it, such as an error message or a webhook receiver's answer.
//
// Postgres refuses text that is not valid UTF-8, and text that holds a NUL
// byte, with SQLSTATE 22021, so the whole write fails. A cut at a plain byte
// index can split a character, and text from outside can hold any bytes.
//
// Clip first replaces each run of bytes that is not valid UTF-8, and each NUL
// byte, with U+FFFD, the replacement character. It then cuts at the last
// character boundary at or before maxBytes, so it never splits a character.
// A maxBytes of 0 or less returns "".
func Clip(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.ReplaceAll(s, "\x00", "\uFFFD")
	if len(s) <= maxBytes {
		return s
	}
	// s is valid UTF-8 now, so stepping back over continuation bytes finds
	// the start of the character that byte maxBytes belongs to.
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
