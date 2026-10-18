// Package breachedpw holds the breached-password list that
// user.ValidatePassword checks when a password is set or reset (ADR-095,
// "Offline breached-password"). The list is compiled into the binary, so
// it cannot be missing at runtime and needs no network.
//
// Source: ignis-1M.txt, the one million most common passwords in about a
// billion leaked credentials, most common first, from
// https://github.com/ignis-sec/Pwdb-Public at commit
// 9b53917431ed5cd57cd6cbec8c3a19cf9a5cd8dd (wordlists/ignis-1M.txt,
// SHA-256 e9a88f67aafe65496682dc374559ee714e978bee50314767494c3e37a18c9fc8).
// MIT licence, copyright 2020 Ata Hakçıl; the full text is in LICENSE
// next to this file. SecLists ships the same file as
// Passwords/Common-Credentials/Pwdb_top-1000000.txt.
//
// passwords.txt keeps only the entries a user could still choose: valid
// UTF-8 and at least MinLength bytes, because ValidatePassword refuses
// anything shorter before it looks here. It also keeps the ten entries
// of the hand-picked list this replaced, so nothing refused before is
// accepted now. Entries are lower-cased, unique and sorted, so a lookup
// is a binary search. That leaves 29,983 entries in 427,514 bytes, which
// is what the list adds to the binary.
//
// To refresh it, download a new source file and rebuild in a release.
// The list is never fetched at runtime:
//
//	curl -sSfLo /tmp/ignis-1M.txt https://raw.githubusercontent.com/ignis-sec/Pwdb-Public/<commit>/wordlists/ignis-1M.txt
//	go run ./internal/user/breachedpw/gen -src /tmp/ignis-1M.txt
//
// then update the commit, SHA-256 and counts above.
package breachedpw

import (
	_ "embed"
	"slices"
	"strings"
	"sync"
)

// MinLength is the shortest entry kept. It must equal
// user.MinPasswordLength; a test in package user checks that. If the
// minimum changes, change this and rebuild the list.
const MinLength = 12

//go:embed passwords.txt
var list string

// entries splits the embedded list once, on first use. The strings point
// into the embedded data, so only the slice itself is allocated.
var entries = sync.OnceValue(func() []string {
	return strings.Split(strings.TrimSuffix(list, "\n"), "\n")
})

// Normalize is the form passwords are stored and compared in. The
// generator and Contains both use it, so they always agree.
func Normalize(password string) string {
	return strings.ToLower(password)
}

// Contains reports whether password, in any letter case, is on the list.
func Contains(password string) bool {
	_, found := slices.BinarySearch(entries(), Normalize(password))
	return found
}
