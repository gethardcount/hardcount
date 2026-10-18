// Command gen rebuilds internal/user/breachedpw/passwords.txt from a
// downloaded copy of the source list. The breachedpw package comment
// names the source and shows how to run it.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"log"
	"maps"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sagarsuperuser/velox/internal/user/breachedpw"
)

// replaced is the hand-picked list ValidatePassword used before the
// breached list. Kept so that nothing it refused is accepted now; the
// length rule drops the entries that are too short to choose anyway.
var replaced = []string{
	"password", "password123", "123456789012", "qwertyuiopasdf",
	"administrator", "changeme123", "letmein12345", "iloveyou1234",
	"welcome12345", "admin12345678",
}

func main() {
	src := flag.String("src", "", "downloaded source list: one password per line")
	out := flag.String("out", "internal/user/breachedpw/passwords.txt", "file to write")
	flag.Parse()
	if *src == "" {
		log.Fatal("-src is required")
	}
	raw, err := os.ReadFile(*src)
	if err != nil {
		log.Fatal(err)
	}

	lines := strings.Split(string(raw), "\n")
	set := map[string]struct{}{}
	for _, pw := range append(lines, replaced...) {
		pw = strings.TrimSuffix(pw, "\r")
		if !utf8.ValidString(pw) {
			continue // a password from a JSON request is always valid UTF-8
		}
		key := breachedpw.Normalize(pw)
		// ValidatePassword checks the length of the password as typed,
		// then looks up its lower-case form; keep the entry if either
		// form is long enough.
		if len(pw) < breachedpw.MinLength && len(key) < breachedpw.MinLength {
			continue
		}
		set[key] = struct{}{}
	}

	sorted := slices.Sorted(maps.Keys(set))
	body := strings.Join(sorted, "\n") + "\n"
	if err := os.WriteFile(*out, []byte(body), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("source sha256 %x: %d lines in, %d entries out, %d bytes\n",
		sha256.Sum256(raw), len(lines), len(sorted), len(body))
}
