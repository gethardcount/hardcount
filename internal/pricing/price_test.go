package pricing

import (
	"strings"
	"testing"
)

// TestCheckMatch_LongLabelNameQuotedBetweenCharacters: a dimension_match
// label name over 100 bytes is refused, and the error quotes the name's first
// 20 bytes and "…". When byte 20 falls inside a character, the quote ends
// before that character. A plain byte cut would show half of it, as \xc3.
func TestCheckMatch_LongLabelNameQuotedBetweenCharacters(t *testing.T) {
	kept := strings.Repeat("a", 19)
	_, err := checkMatch("dimension_match", map[string]string{kept + "é" + strings.Repeat("b", 100): "v"})
	if err == nil {
		t.Fatal("checkMatch accepted a 121-byte label name")
	}
	if want := `label "` + kept + `…" is longer than 100 characters`; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}
