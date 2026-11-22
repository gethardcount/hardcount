package errs

import (
	"errors"
	"fmt"
	"testing"
)

// Error is the Message, then the Cause when there is one.
func TestDomainError_ErrorIsMessageThenCause(t *testing.T) {
	t.Run("without cause", func(t *testing.T) {
		err := &DomainError{Message: "something went wrong"}
		if err.Error() != "something went wrong" {
			t.Errorf("got %q", err.Error())
		}
	})

	t.Run("with cause", func(t *testing.T) {
		cause := fmt.Errorf("db connection failed")
		err := &DomainError{Message: "database unavailable", Cause: cause}
		if err.Error() != "database unavailable: db connection failed" {
			t.Errorf("got %q", err.Error())
		}
	})
}

// errors.Is and errors.As look through a DomainError to its Cause.
func TestDomainError_UnwrapsToItsCause(t *testing.T) {
	cause := fmt.Errorf("original error")
	err := &DomainError{Message: "wrapped", Cause: cause}

	if !errors.Is(err, cause) {
		t.Error("Unwrap should return cause")
	}
}

// Code finds a DomainError's Code anywhere in the chain, and is "" without one.
func TestCode_FindsTheCodeAnywhereInTheChain(t *testing.T) {
	t.Run("extracts code from DomainError", func(t *testing.T) {
		err := Invalid("url", "must start with https://").WithCode("url_not_https")
		if Code(err) != "url_not_https" {
			t.Errorf("got %q", Code(err))
		}
	})

	t.Run("extracts code from wrapped DomainError", func(t *testing.T) {
		inner := NotFound("plan").WithCode("inner_code")
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

// Each constructor makes a DomainError of its kind, and of no other, with
// the field, message and code it is given.
func TestConstructors_SetKindFieldMessageAndCode(t *testing.T) {
	kinds := []error{ErrNotFound, ErrAlreadyExists, ErrInvalidState, ErrForbidden, ErrValidation, ErrUnavailable}
	cause := errors.New("connection reset")
	cases := []struct {
		name                 string
		err                  *DomainError
		kind                 error
		field, message, code string
	}{
		{"Required", Required("external_id"), ErrValidation, "external_id", "external_id is required", ""},
		{"Invalid", Invalid("email", "must contain @"), ErrValidation, "email", "must contain @", ""},
		{"AlreadyExists", AlreadyExists("code", `plan code "acme-pro" already exists`), ErrAlreadyExists, "code", `plan code "acme-pro" already exists`, ""},
		{"InvalidState", InvalidState("cannot rotate a revoked key"), ErrInvalidState, "", "cannot rotate a revoked key", ""},
		{"Forbidden", Forbidden("this key cannot do that"), ErrForbidden, "", "this key cannot do that", ""},
		{"NotFound", NotFound("customer"), ErrNotFound, "", "customer not found", "not_found"},
		{"Unavailable", Unavailable("proration_unavailable", "please retry", cause), ErrUnavailable, "", "please retry", "proration_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Field != tc.field || tc.err.Message != tc.message || tc.err.Code != tc.code {
				t.Errorf("got field %q message %q code %q, want %q %q %q", tc.err.Field, tc.err.Message, tc.err.Code, tc.field, tc.message, tc.code)
			}
			for _, k := range kinds {
				if got, want := errors.Is(tc.err, k), k == tc.kind; got != want {
					t.Errorf("errors.Is(err, %v) = %v, want %v", k, got, want)
				}
			}
		})
	}
}

// Field finds a DomainError's Field anywhere in the chain, and is "" without one.
func TestField_FindsTheFieldAnywhereInTheChain(t *testing.T) {
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
