package main

import "testing"

func TestParseForceVersion(t *testing.T) {
	if v, err := parseForceVersion([]string{"179"}); err != nil || v != 179 {
		t.Fatalf("got %d, %v", v, err)
	}
	if v, err := parseForceVersion([]string{"0"}); err != nil || v != 0 {
		t.Fatalf("zero: got %d, %v", v, err)
	}
	for _, args := range [][]string{nil, {"-1"}, {"abc"}, {"1", "2"}} {
		if _, err := parseForceVersion(args); err == nil {
			t.Errorf("%q: want error", args)
		}
	}
}
