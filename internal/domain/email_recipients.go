package domain

import (
	"fmt"
	"net/mail"
	"strings"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// MaxAdditionalEmails caps a customer's CC list (ADR-082). Enforced at
// the service layer — the column stores ciphertext, so no DB CHECK can
// count entries.
const MaxAdditionalEmails = 10

// ValidateEmail is the one syntax check for every email address Velox
// accepts or sends to. It returns the address net/mail parsed, which
// callers store and send to.
//
// The value, trimmed, must be exactly one bare address (an RFC 5322
// addr-spec), so the address parsed is the address typed. net/mail also
// accepts a display name ("Bob <bob@x.com>"), angle brackets, a comment
// and a quoted local part, and returns an Address that differs from the
// input. A quoted local part comes back unquoted, so the address in the
// SMTP envelope could differ from the one typed and shown in the Cc
// header. All of these are refused.
//
// The domain must also contain a dot: net/mail allows `foo@bar`, which is
// valid RFC syntax but no use for delivery. Bounce-driven suppression
// catches the addresses that pass and still do not deliver.
func ValidateEmail(field, email string) (string, error) {
	email = strings.TrimSpace(email)
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return "", errs.Invalid(field, "invalid email")
	}
	if addr.Address != email {
		return "", errs.Invalid(field, "invalid email: enter one address, such as name@example.com, with no name, quotes or angle brackets")
	}
	// net/mail always returns local@domain.
	domainPart := addr.Address[strings.LastIndex(addr.Address, "@")+1:]
	if !strings.Contains(domainPart, ".") || strings.HasSuffix(domainPart, ".") {
		return "", errs.Invalid(field, "invalid email: domain must contain a dot")
	}
	return addr.Address, nil
}

// NormalizeAdditionalEmails validates and canonicalizes a CC list
// (ADR-082): trim → ValidateEmail (one bare address each; a display
// name, quotes or angle brackets are refused) → lowercase →
// case-insensitive dedupe → reject entries equal to the primary → cap.
// Returns nil for an effectively-empty list. Shared by the customer
// service (stored list) and the send handlers (per-send override) so
// both enforce identical rules.
func NormalizeAdditionalEmails(list []string, primaryEmail string) ([]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	primary := strings.ToLower(strings.TrimSpace(primaryEmail))
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, raw := range list {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		addr, err := ValidateEmail("additional_emails", e)
		if err != nil {
			return nil, err
		}
		norm := strings.ToLower(addr)
		if norm == primary && primary != "" {
			return nil, errs.Invalid("additional_emails", "must not repeat the primary email — it always receives billing emails")
		}
		if seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, norm)
	}
	if len(out) > MaxAdditionalEmails {
		return nil, errs.Invalid("additional_emails", fmt.Sprintf("at most %d additional emails are allowed", MaxAdditionalEmails))
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
