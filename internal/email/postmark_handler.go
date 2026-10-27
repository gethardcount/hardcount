package email

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// PostmarkHandler ingests Postmark's outbound-message webhooks (ADR-098):
// Delivery, Bounce, SpamComplaint. It closes the two gaps the SMTP-handoff
// model leaves open with an async-validating provider: 'dispatched' only
// means the relay took the message (Postmark 250-accepts then decides
// later), and the sync RCPT-time bounce classifier never fires at all —
// Postmark reports every recipient verdict asynchronously, through these
// webhooks.
//
// Trust model — deliberately NOT the Stripe shape. Stripe is BYOS: a
// per-tenant endpoint id in the URL resolves a per-tenant HMAC secret.
// Postmark is ONE platform-level account (env SMTP_*) serving all tenants,
// and Postmark signs nothing — its webhook auth is HTTP Basic Auth on the
// configured URL. So: one platform route, constant-time Basic Auth as the
// gate, and tenant + livemode resolved from OUR stored outbox row via the
// stamped correlation metadata — never trusted off the blind POST. The
// 96-bit-random outbox id doubles as the per-event capability: even a
// caller holding the Basic Auth credential can only affect rows whose ids
// it knows, and each row only ever resolves to its own tenant.
//
// Response-code contract, tuned to Postmark's retry semantics (403 stops
// all retries; other non-2xx retries — Bounce ~10x/10.5h, Delivery only
// ~3x/21min): bad credentials → 401 (retryable, so a misconfig stays
// visible in Postmark's activity feed instead of silently dying);
// deterministically-unprocessable-but-authenticated payloads (oversize,
// bad JSON, unknown RecordType, absent/unresolvable correlation) → 200 ack
// + WARN (a retry cannot fix a deterministic error); only genuine
// transient failures (DB down) → 5xx, redelivering into idempotent writes.
type PostmarkHandler struct {
	store      OutboxDeliveryStore
	suppressor RecipientSuppressor
	user, pass string
}

// OutboxDeliveryStore is what the webhook needs from the outbox: resolve
// the stamped row id back to a row (tenant + livemode + primary
// recipient) and record the provider-confirmed outcome. Satisfied by
// *OutboxStore.
type OutboxDeliveryStore interface {
	GetByID(ctx context.Context, outboxID string) (OutboxRow, error)
	MarkDeliveryState(ctx context.Context, tenantID, outboxID, state string) (bool, error)
}

// RecipientSuppressor records provider-confirmed recipient facts (hard
// bounce / spam complaint) on matching customer rows. Unlike the Sender's
// fire-and-forget BounceReporter, errors RETURN: a transient DB failure
// must 5xx so Postmark redelivers into the idempotent writes. Zero
// matches (a never-a-customer CC alias) is benign — implementations
// return nil for it (ADR-082: no persistent per-CC state).
type RecipientSuppressor interface {
	SuppressBounced(ctx context.Context, tenantID, email, reason string) error
	SuppressComplained(ctx context.Context, tenantID, email, reason string) error
}

// maxPostmarkBodySize caps the webhook body. Postmark payloads are small
// (we never enable Content/Dump fields); 64KB is the same cap the Stripe
// path uses.
const maxPostmarkBodySize = 64 * 1024

// NewPostmarkHandler builds the webhook receiver. Empty credentials are
// allowed at construction (boot warns at the wiring site); every request
// is then rejected 401 — loud in Postmark's activity feed, never an open
// unauthenticated endpoint.
func NewPostmarkHandler(store OutboxDeliveryStore, user, pass string) *PostmarkHandler {
	return &PostmarkHandler{store: store, user: user, pass: pass}
}

// SetSuppressor wires the customer-suppression bridge. Nil is tolerated
// (blind-index key not configured): delivery_state ingestion still works;
// bounce/complaint suppression degrades to a WARN, matching the SMTP
// path's degraded mode.
func (h *PostmarkHandler) SetSuppressor(s RecipientSuppressor) { h.suppressor = s }

// postmarkEvent is the union of the Delivery / Bounce / SpamComplaint
// payload fields Velox consumes. Bounce and SpamComplaint carry the
// recipient in Email; Delivery carries it in Recipient.
type postmarkEvent struct {
	RecordType  string            `json:"RecordType"`
	MessageID   string            `json:"MessageID"`
	Metadata    map[string]string `json:"Metadata"`
	Recipient   string            `json:"Recipient"`
	Email       string            `json:"Email"`
	Type        string            `json:"Type"`
	TypeCode    int               `json:"TypeCode"`
	Inactive    bool              `json:"Inactive"`
	Description string            `json:"Description"`
}

func (h *PostmarkHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	// Basic Auth FIRST, before the body is read — an unauthenticated
	// probe shouldn't even buffer a payload. Constant-time over both
	// halves; unconfigured credentials compare equal to nothing.
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="velox-webhooks"`)
		respond.Unauthorized(w, r, "unauthorized")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPostmarkBodySize+1))
	if err != nil {
		// A torn read is transport-transient — let Postmark redeliver.
		respond.InternalError(w, r)
		return
	}
	if len(body) > maxPostmarkBodySize {
		// Deterministic: redelivery re-sends the same oversize payload,
		// so a non-2xx buys nothing (Delivery retries only ~3x/21min).
		slog.Warn("postmark webhook: body exceeds size cap — acked and dropped", "cap_bytes", maxPostmarkBodySize)
		respond.JSON(w, r, http.StatusOK, map[string]string{"status": "skipped", "reason": "payload too large"})
		return
	}

	var evt postmarkEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		slog.Warn("postmark webhook: unparseable JSON — acked and dropped", "error", err.Error())
		respond.JSON(w, r, http.StatusOK, map[string]string{"status": "skipped", "reason": "invalid json"})
		return
	}

	switch evt.RecordType {
	case "Delivery", "Bounce", "SpamComplaint":
	default:
		// Open / Click / SubscriptionChange / future types — not ours.
		respond.JSON(w, r, http.StatusOK, map[string]string{"status": "ignored", "reason": "record type not ingested"})
		return
	}

	// Soft/transient bounce: Postmark retries it and every major ESP
	// (SES, Mailgun, Postmark, Resend) treats it as retry-then-surface,
	// never suppression. Recording nothing is the deliberate design —
	// classification is delegated to the provider (Inactive = Postmark
	// deactivated the address; HardBounce = its permanent verdict), the
	// same conservative bias as the SMTP-path classifier: when in doubt,
	// never mark a recipient dead.
	if evt.RecordType == "Bounce" && !postmarkBouncePermanent(evt) {
		slog.Info("postmark webhook: transient bounce — not recorded",
			"bounce_type", evt.Type, "type_code", evt.TypeCode)
		respond.JSON(w, r, http.StatusOK, map[string]string{"status": "ignored", "reason": "transient bounce"})
		return
	}

	// Correlation: the stamped outbox id is the ONLY tenant resolution.
	// Absent or unresolvable → ack + WARN; never guess a tenant off a
	// blind POST (mis-attributing across tenants sharing an email would
	// corrupt another tenant's suppression state).
	outboxID := evt.Metadata[CorrelationMetadataKey]
	if outboxID == "" {
		slog.Warn("postmark webhook: no correlation metadata — acked and dropped",
			"record_type", evt.RecordType, "message_id", evt.MessageID)
		respond.JSON(w, r, http.StatusOK, map[string]string{"status": "skipped", "reason": "no correlation metadata"})
		return
	}
	row, err := h.store.GetByID(r.Context(), outboxID)
	if errors.Is(err, sql.ErrNoRows) {
		slog.Warn("postmark webhook: unknown outbox id — acked and dropped",
			"outbox_id", outboxID, "record_type", evt.RecordType)
		respond.JSON(w, r, http.StatusOK, map[string]string{"status": "skipped", "reason": "unknown outbox id"})
		return
	}
	if err != nil {
		slog.Error("postmark webhook: outbox lookup failed", "outbox_id", outboxID, "error", err)
		respond.InternalError(w, r)
		return
	}

	// Stage the ROW's livemode (stamped by the 0021 trigger at enqueue —
	// authoritative; Postmark has no livemode concept to cross-check).
	// Tenant flows as an explicit parameter below, also from the row.
	ctx := postgres.WithLivemode(r.Context(), row.Livemode)

	var state, recipient, cause string
	switch evt.RecordType {
	case "Delivery":
		state, recipient = DeliveryDelivered, evt.Recipient
	case "Bounce":
		state, recipient = DeliveryBounced, evt.Email
		cause = scrubbedCause("postmark "+evt.Type, evt.Description)
	case "SpamComplaint":
		state, recipient = DeliveryComplained, evt.Email
		cause = scrubbedCause("postmark spam complaint", evt.Description)
	}

	// The row's delivery_state reflects the PRIMARY recipient only — an
	// event naming a CC address must not flip it (ADR-082 per-recipient
	// attribution; the primary's copy may have delivered fine).
	primary, _ := row.Payload["to"].(string)
	if recipient != "" && strings.EqualFold(recipient, primary) {
		if _, err := h.store.MarkDeliveryState(ctx, row.TenantID, row.ID, state); err != nil {
			slog.Error("postmark webhook: delivery-state write failed",
				"outbox_id", row.ID, "state", state, "error", err)
			respond.InternalError(w, r)
			return
		}
	}

	// Suppression targets the EVENT's address (a CC bounce suppresses
	// the CC customer, never the primary), scoped to the row's tenant.
	if state == DeliveryBounced || state == DeliveryComplained {
		if h.suppressor == nil {
			slog.Warn("postmark webhook: no suppressor wired (blind-index key unset) — recipient state not recorded",
				"outbox_id", row.ID, "record_type", evt.RecordType)
		} else if recipient != "" {
			var supErr error
			if state == DeliveryComplained {
				supErr = h.suppressor.SuppressComplained(ctx, row.TenantID, recipient, cause)
			} else {
				supErr = h.suppressor.SuppressBounced(ctx, row.TenantID, recipient, cause)
			}
			if supErr != nil {
				slog.Error("postmark webhook: suppression write failed",
					"outbox_id", row.ID, "record_type", evt.RecordType, "error", supErr)
				respond.InternalError(w, r)
				return
			}
		}
		slog.Info("postmark webhook: recipient verdict recorded",
			"outbox_id", row.ID, "tenant_id", row.TenantID,
			"record_type", evt.RecordType, "bounce_type", evt.Type,
			"state", state, "primary_recipient", strings.EqualFold(recipient, primary))
	}

	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "processed"})
}

func (h *PostmarkHandler) authorized(r *http.Request) bool {
	if h.user == "" || h.pass == "" {
		return false
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(h.user)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(h.pass)) == 1
	return userOK && passOK
}

// postmarkBouncePermanent reports whether a Bounce event is a permanent
// recipient verdict. The provider's own deactivation decision (Inactive)
// is the primary signal — Postmark suspends sending to the address when
// it judged the failure permanent — with HardBounce as the explicit
// verdict name. Everything else (SoftBounce, Transient, DnsError, sender-
// side codes like DMARCPolicy/SMTPApiError) is NOT a recipient bounce:
// same conservative bias as isPermanentSMTPBounce — a sender-side problem
// must never mark a recipient's mailbox dead (2026-05-29 hardening).
func postmarkBouncePermanent(evt postmarkEvent) bool {
	return evt.Inactive || evt.Type == "HardBounce" || evt.TypeCode == 1
}

// scrubbedCause builds the stored bounce reason from provider free-text,
// PII-scrubbed at ingress (emails/last4 stay out of customer rows and
// logs — the #485 lesson).
func scrubbedCause(prefix, description string) string {
	d := strings.TrimSpace(description)
	if d == "" {
		return prefix
	}
	return prefix + ": " + errs.Scrub(d)
}
