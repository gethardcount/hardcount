package postgres

import "strings"

// likeEscaper escapes the LIKE and ILIKE metacharacters (% and _) and the
// default escape character (backslash). A user-supplied search term then
// matches literally inside a pattern such as "%" + EscapeLike(term) + "%".
// Postgres' default ESCAPE character is backslash, so call sites need no
// explicit ESCAPE clause.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// EscapeLike returns s with all LIKE and ILIKE metacharacters escaped. Use
// it for every operator-typed search term that reaches an ILIKE pattern.
// Without it, a term like "100%" matches everything, and "_" matches any
// single character.
func EscapeLike(s string) string {
	return likeEscaper.Replace(s)
}
