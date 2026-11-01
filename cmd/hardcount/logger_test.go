package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestNewLogger_LevelOrRefusal: the server's logger filters at LOG_LEVEL,
// and an unknown level is an error, on which serve does not start. It used
// to mean info without a word.
// Mutation-verify: ignore ParseLogLevel's error in newLogger → no error.
func TestNewLogger_LevelOrRefusal(t *testing.T) {
	t.Setenv("LOG_LEVEL", "verbose")
	if _, err := newLogger(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("LOG_LEVEL=verbose: err %v, want a refusal naming LOG_LEVEL", err)
	}

	t.Setenv("LOG_LEVEL", "Warning")
	var out bytes.Buffer
	logger, err := newLogger(&out)
	if err != nil {
		t.Fatalf("LOG_LEVEL=Warning: %v", err)
	}
	logger.Info("dropped")
	logger.Warn("kept")
	if strings.Contains(out.String(), "dropped") || !strings.Contains(out.String(), "kept") {
		t.Fatalf("a warn logger wrote %q, want only the warning", out.String())
	}
}
