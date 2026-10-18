package email

import (
	"context"
)

// correlationKey carries the email_outbox row id from the dispatcher to the
// transport so the MIME builders can stamp it as provider metadata
// (X-PM-Metadata-*). Ctx-value plumbing mirrors the established
// livemode-on-ctx convention (Dispatcher.handle pins postgres.WithLivemode
// the same way) — the alternative was threading an outbox_id parameter
// through all ten Send* signatures for the sake of one header line.
type correlationKey struct{}

// WithCorrelation returns a ctx carrying the outbox row id of the email
// being sent. Set by Dispatcher.handle; read by the MIME builders.
func WithCorrelation(ctx context.Context, outboxID string) context.Context {
	return context.WithValue(ctx, correlationKey{}, outboxID)
}

// CorrelationMetadataKey is the metadata key the provider echoes back in
// its Delivery/Bounce/SpamComplaint webhook payloads (the outbound header
// name minus the X-PM-Metadata- prefix). Hyphenated, not underscored:
// Postmark's metadata docs only demonstrate hyphenated key names, and the
// key must survive the header→metadata→webhook round-trip verbatim.
// 13 chars — within Postmark's 20-char key cap.
const CorrelationMetadataKey = "vlx-outbox-id"

// correlationHeaderName is the outbound MIME header Postmark strips before
// delivery and echoes back (string-valued) under
// Metadata[CorrelationMetadataKey]. It is the exact-row handle the webhook
// handler resolves tenant + livemode from (ADR-098) — the row is the
// authoritative source; nothing else is stamped (a second tenant_id copy
// could disagree with the row's).
const correlationHeaderName = "X-PM-Metadata-" + CorrelationMetadataKey

// correlationID returns the ctx's outbox id for the correlation header,
// or "" when the ctx carries none (direct Sender use outside the
// dispatcher); the header is then absent, never empty-valued. The value is
// our own generated 'vlx_emob_'+hex id, but it is validated anyway:
// anything outside the id alphabet (which can never contain CR/LF) or over
// Postmark's 80-char value cap drops the header rather than risk header
// injection or silent provider-side truncation.
func correlationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	if id == "" || len(id) > 80 || !safeMetadataValue(id) {
		return ""
	}
	return id
}

func safeMetadataValue(s string) bool {
	for _, r := range s {
		ok := r == '_' || r == '-' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}
