// Package errs provides structured domain errors with machine-readable codes.
// The API layer picks an error's HTTP status from its kind (a sentinel such as
// ErrValidation, matched with errors.Is), never from its message. An error
// with no kind, a DomainError with only a code included, is a server fault and
// answers 500.
package errs
