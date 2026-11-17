package errs

import (
	"errors"
	"fmt"
	"testing"
)

func TestDomainError_Error(t *testing.T) {
	t.Run("without cause", func(t *testing.T) {
		err := New("test_code", "something went wrong")
		if err.Error() != "something went wrong" {
			t.Errorf("got %q", err.Error())
		}
	})

	t.Run("with cause", func(t *testing.T) {
		cause := fmt.Errorf("db connection failed")
		err := Wrap(cause, "db_error", "database unavailable")
		if err.Error() != "database unavailable: db connection failed" {
			t.Errorf("got %q", err.Error())
		}
	})
}

func TestDomainError_Unwrap(t *testing.T) {
	cause := fmt.Errorf("original error")
	err := Wrap(cause, "code", "wrapped")

	if !errors.Is(err, cause) {
		t.Error("Unwrap should return cause")
	}
}

func TestCode(t *testing.T) {
	t.Run("extracts code from DomainError", func(t *testing.T) {
		err := New("billing_setup_incomplete", "needs stripe")
		if Code(err) != "billing_setup_incomplete" {
			t.Errorf("got %q", Code(err))
		}
	})

	t.Run("extracts code from wrapped DomainError", func(t *testing.T) {
		inner := New("inner_code", "inner")
		outer := fmt.Errorf("outer: %w", inner)
		if Code(outer) != "inner_code" {
			t.Errorf("got %q", Code(outer))
		}
	})

	t.Run("returns empty for non-DomainError", func(t *testing.T) {
		err := fmt.Errorf("plain error")
		if Code(err) != "" {
			t.Errorf("got %q, want empty", Code(err))
		}
	})

	t.Run("returns empty for nil", func(t *testing.T) {
		if Code(nil) != "" {
			t.Error("nil should return empty")
		}
	})
}

func TestRequired(t *testing.T) {
	err := Required("external_id")
	if err.Field != "external_id" {
		t.Errorf("field: got %q", err.Field)
	}
	if err.Error() != "external_id is required" {
		t.Errorf("message: got %q", err.Error())
	}
	if !errors.Is(err, ErrValidation) {
		t.Error("Required should be an ErrValidation")
	}
}

func TestInvalid(t *testing.T) {
	err := Invalid("email", "must contain @")
	if err.Field != "email" {
		t.Errorf("field: got %q", err.Field)
	}
	if err.Error() != "must contain @" {
		t.Errorf("message: got %q", err.Error())
	}
	if !errors.Is(err, ErrValidation) {
		t.Error("Invalid should be an ErrValidation")
	}
}

func TestAlreadyExists(t *testing.T) {
	err := AlreadyExists("code", "plan code \"acme-pro\" already exists")
	if err.Field != "code" {
		t.Errorf("field: got %q", err.Field)
	}
	if !errors.Is(err, ErrAlreadyExists) {
		t.Error("AlreadyExists should be an ErrAlreadyExists")
	}
}

func TestField(t *testing.T) {
	t.Run("extracts field from DomainError", func(t *testing.T) {
		if Field(Required("foo")) != "foo" {
			t.Errorf("got %q, want foo", Field(Required("foo")))
		}
	})

	t.Run("extracts field from wrapped DomainError", func(t *testing.T) {
		err := fmt.Errorf("outer: %w", Required("bar"))
		if Field(err) != "bar" {
			t.Errorf("got %q, want bar", Field(err))
		}
	})

	t.Run("returns empty for plain error", func(t *testing.T) {
		if Field(fmt.Errorf("plain")) != "" {
			t.Error("plain error should have no field")
		}
	})

	t.Run("returns empty for nil", func(t *testing.T) {
		if Field(nil) != "" {
			t.Error("nil should have no field")
		}
	})
}

func TestSentinelErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		sentinel error
	}{
		{"NotFound", fmt.Errorf("customer: %w", ErrNotFound), ErrNotFound},
		{"AlreadyExists", fmt.Errorf("dup: %w", ErrAlreadyExists), ErrAlreadyExists},
		{"InvalidState", fmt.Errorf("state: %w", ErrInvalidState), ErrInvalidState},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, tt.sentinel) {
				t.Errorf("errors.Is should match %v", tt.sentinel)
			}
		})
	}
}
