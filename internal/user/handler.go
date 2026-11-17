package user

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/request"
	"github.com/gethardcount/hardcount/internal/session"
)

// Handler wires the dashboard auth endpoints. ADR-011.
//
// Routes (mounted under /v1/auth):
//
//	POST /login                     email + password → mint cookie
//	POST /logout                    end the session, clear the cookie
//	POST /mode                      switch the session's mode
//	POST /password-reset/request    email → send reset link
//	POST /password-reset/confirm    token + new password → set
type Handler struct {
	users            *Service
	cookie           session.CookieConfig
	email            EmailSender // required — production always wires the adapter
	dashboardBaseURL string      // canonical dashboard origin for reset links; never from request headers. empty => reset emails disabled
	smtpConfigured   bool        // SMTP wired at boot (email.Sender.IsConfigured); drives the email_delivery hint
	auditLogger      AuditRecorder
	throttle         *LoginThrottle // nil only in unit tests that build a bare handler
}

// SetLoginThrottle wires the sign-in throttle (throttle.go). Production always
// wires it in internal/api; the e2e tests there prove it. A handler without
// one checks every password, as before the throttle.
func (h *Handler) SetLoginThrottle(t *LoginThrottle) { h.throttle = t }

// AuditRecorder writes the audit row of the password-reset request. A nil
// recorder leaves that request with no row, which the unit tests that build
// a bare handler rely on.
type AuditRecorder interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

// SetAuditLogger wires the recorder for the password-reset request. A
// sign-in, a sign-out, a mode switch and a completed reset write their row
// on their own transaction (Service.Login, Logout, SetMode, ConfirmReset).
// A failed sign-in writes no row: it has no workspace to file one under, and
// a row would say the address exists. It goes to the security log.
func (h *Handler) SetAuditLogger(a AuditRecorder) { h.auditLogger = a }

// auditResetRequest writes the audit row of a password-reset request for the
// account userID, outside a transaction. The row has no actor (anyone can ask
// for a reset) and is filed in test mode, as every account event is: the
// route has no session, so ctx carries no mode. A failed write is logged and
// the request still answers.
func (h *Handler) auditResetRequest(ctx context.Context, r *http.Request, tenantID, userID string) {
	if h.auditLogger == nil || tenantID == "" {
		return
	}
	ctx = postgres.WithLivemode(ctx, false)
	if err := h.auditLogger.Log(ctx, tenantID, "password_reset_requested", "user", userID, "", nil); err != nil {
		slog.ErrorContext(ctx, "audit: password reset request row not written", "tenant_id", tenantID, "error", err)
	}
}

// EmailSender is the narrow surface this handler uses to dispatch
// password-reset emails. Satisfied in production by an adapter over
// internal/email's Sender / OutboxSender. SMTP misconfiguration
// surfaces as a SendPasswordReset error logged here; the response to
// the user stays generic to prevent enumeration.
type EmailSender interface {
	SendPasswordReset(ctx context.Context, tenantID, email, resetLink string) error
}

// NewHandler wires the dependencies. emailSender is required —
// password-reset requests will nil-panic if it's nil. dashboardBaseURL is
// the canonical dashboard origin used to build password-reset links; it is
// never derived from the request (Host header) to prevent host-header
// poisoning / token theft. When empty, password-reset emails are not sent
// (requestPasswordReset fails safe). Set it to your dashboard URL — in
// split-origin dev (Vite on :5173 vs API on :8080) that's the SPA URL.
func NewHandler(users *Service, cookie session.CookieConfig, emailSender EmailSender, dashboardBaseURL string, smtpConfigured bool) *Handler {
	return &Handler{
		users:            users,
		cookie:           cookie,
		email:            emailSender,
		dashboardBaseURL: strings.TrimRight(strings.TrimSpace(dashboardBaseURL), "/"),
		smtpConfigured:   smtpConfigured,
	}
}

// Routes returns the dashboard auth surface. Mount under /v1/auth.
// All routes are intentionally outside session middleware: they're
// either pre-session (login, password-reset) or take the cookie via
// r.Cookie directly (logout, mode).
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/login", h.login)
	r.Post("/logout", h.logout)
	r.Post("/mode", h.setMode)
	r.Post("/password-reset/request", h.requestPasswordReset)
	r.Post("/password-reset/confirm", h.confirmPasswordReset)
	// GET /password-reset/check?token=… — non-consuming validity probe
	// for the reset-password page on mount. 200 = renderable form, 422
	// = link is invalid/expired/used. Idempotent so it's a GET.
	r.Get("/password-reset/check", h.checkPasswordResetToken)
	return r
}

type LoginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginResponse is POST /v1/auth/login's 200.
type LoginResponse struct {
	UserID    string `json:"user_id"`
	TenantID  string `json:"tenant_id"`
	Email     string `json:"email"`
	Livemode  bool   `json:"livemode"`
	ExpiresAt string `json:"expires_at"`
}

// login authenticates the operator's email + password and mints a
// session cookie. Failures collapse into a single 401 with a
// constant-time bcrypt check on the not-found path so we don't leak
// account existence via response timing. The session, the end of the
// session the request's cookie named, last_login_at and the login audit
// row commit together (Service.Login).
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req LoginReq
	if !respond.DecodeStrict(w, r, &req) {
		return
	}
	if req.Email == "" {
		respond.ValidationField(w, r, "email", "email is required")
		return
	}
	if req.Password == "" {
		respond.ValidationField(w, r, "password", "password is required")
		return
	}

	// The address TrustedClientIP resolved. The failed-login log, the throttle,
	// the session row and the audit row all use this one value; none of them
	// reads a forwarding header the client could have written.
	ip := request.Of(r.Context()).ClientIP()

	// The sign-in throttle counts this attempt before the password check, so
	// a refused attempt costs no account lookup and no bcrypt. It is keyed on
	// the typed email, so an email with no account is refused the same way.
	var verdict ThrottleVerdict
	if h.throttle != nil {
		var err error
		verdict, err = h.throttle.Reserve(r, req.Email)
		if err != nil {
			// Fail closed: sign-in needs the database anyway.
			slog.ErrorContext(r.Context(), "auth: sign-in throttle failed; sign-in refused", "ip", ip, "error", err)
			recordLogin("error")
			respond.Error(w, r, http.StatusServiceUnavailable, "authentication_unavailable",
				"Sign-in is unavailable right now. Try again in a minute.")
			return
		}
		if !verdict.Allowed {
			slog.WarnContext(r.Context(), "auth: sign-in throttled",
				"account", verdict.Account, "ip", ip, "retry_after_s", retryAfterSeconds(verdict.RetryAfter))
			recordLogin("throttled")
			writeLoginThrottled(w, r, verdict.RetryAfter)
			return
		}
	}

	// The session the request's cookie named ends in the sign-in's
	// transaction: the new cookie replaces it in this browser.
	var oldSession string
	if c, err := r.Cookie(session.CookieName); err == nil {
		oldSession = c.Value
	}
	res, err := h.users.Login(r.Context(), LoginInput{
		Email:         req.Email,
		Password:      req.Password,
		UserAgent:     r.UserAgent(),
		IP:            ip,
		SessionCookie: oldSession,
	})
	if err != nil {
		if errors.Is(err, ErrBadCredentials) {
			// Failed logins are a SOC-2 CC6.1 signal (brute force / credential
			// stuffing). They can't go to the per-tenant audit_log — there's no
			// resolved tenant pre-auth, and recording email-existence there would
			// be an enumeration oracle — so they land in the structured security
			// log (the right home for pre-auth events). The line names the
			// email's throttle subject and, when the email has an account, its
			// user id: never the typed email, which would sit in the log sink
			// with no way to erase it.
			slog.WarnContext(r.Context(), "auth: failed login attempt",
				"account", verdict.Account, "user_id", wrongPasswordUserID(err), "ip", ip, "reason", "bad_credentials")
			recordLogin("bad_credentials")
			respond.Unauthorized(w, r, "invalid email or password")
			return
		}
		// A store failure is a server error. A refusal is not: the right
		// password for a user with no tenant (a removed member) gets a 409.
		rec := &statusRecorder{ResponseWriter: w}
		respond.FromError(rec, r, err, "user")
		if rec.status >= http.StatusInternalServerError {
			recordLogin("error")
		} else {
			recordLogin("refused")
		}
		return
	}

	if h.throttle != nil {
		// The committed sign-in marks this browser and clears the counts this
		// attempt was held by (LoginThrottle.SignedIn). It runs after the
		// commit, so a sign-in that rolled back clears nothing. A failure here
		// does not undo the sign-in; the counts fade within a day.
		if err := h.throttle.SignedIn(w, r, req.Email, verdict); err != nil {
			slog.ErrorContext(r.Context(), "auth: sign-in throttle not cleared after a sign-in", "user_id", res.User.ID, "error", err)
		}
	}

	recordLogin("ok")
	h.cookie.SetCookie(w, res.RawSessionID, res.Session.ExpiresAt)
	respond.JSON(w, r, http.StatusOK, LoginResponse{
		UserID:    res.User.ID,
		TenantID:  res.TenantID,
		Email:     res.User.Email,
		Livemode:  res.Session.Livemode,
		ExpiresAt: res.Session.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// SetModeReq is POST /v1/auth/mode's body. Livemode is required: a pointer,
// so a body without it is told apart from false.
type SetModeReq struct {
	Livemode *bool `json:"livemode"`
}

// setMode switches the cookie session between test and live mode. The same
// operator switches without signing in again. The switch and its
// mode_changed audit row commit together (Service.SetMode): if the row
// cannot be written the answer is 500 and the mode is unchanged. A missing
// cookie, or a session that is unknown, ended or expired, is a 401.
func (h *Handler) setMode(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(session.CookieName)
	if err != nil || c.Value == "" {
		respond.Unauthorized(w, r, "missing session — sign in at /login")
		return
	}

	var req SetModeReq
	if !respond.DecodeStrict(w, r, &req) {
		return
	}
	if req.Livemode == nil {
		respond.ValidationField(w, r, "livemode", "livemode is required")
		return
	}

	sess, found, err := h.users.SetMode(r.Context(), c.Value, *req.Livemode)
	if err != nil {
		// The session keeps its mode.
		slog.ErrorContext(r.Context(), "mode switch failed; the session keeps its mode", "error", err)
		respond.InternalError(w, r)
		return
	}
	if !found {
		respond.Unauthorized(w, r, "invalid or expired session")
		return
	}
	session.NameActor(r.Context(), sess)

	respond.JSON(w, r, http.StatusOK, map[string]any{
		"livemode": sess.Livemode,
	})
}

// logout ends the session the cookie names and clears the cookie. The
// sign-out and its logout audit row commit together (Service.Logout). If
// they cannot, the answer is 500 and the cookie stays: a browser is never
// told it signed out while its session still works. A cookie for a session
// that is unknown, ended or expired ends nothing, writes nothing and still
// clears the cookie.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	c, err := r.Cookie(session.CookieName)
	if err != nil || c.Value == "" {
		// No cookie: nothing to end and nothing to audit. Say so, or the
		// audit-coverage detector reads a 204 with no row as a mutation that
		// lost its row.
		audit.MarkSkip(r.Context())
		// Deliberately no ClearCookie here. "No cookie arrived" does not mean
		// the browser holds none: SameSite=Lax withholds it from every
		// cross-site request, so this branch is exactly where an attacker's
		// cross-site POST lands. A Set-Cookie that clears it would let any
		// website sign any operator out of the dashboard while the session it
		// cannot see stays live on the server. A request that cannot be
		// authenticated must not change the browser's cookies. When a cookie
		// is truly absent, clearing it would change nothing anyway.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	sess, found, err := h.users.Logout(r.Context(), c.Value)
	if err != nil {
		// The session still works, so the cookie stays.
		slog.ErrorContext(r.Context(), "sign-out failed; the session is still signed in", "error", err)
		respond.InternalError(w, r)
		return
	}
	if found {
		session.NameActor(r.Context(), sess)
	} else {
		// An unknown, ended or expired session: nothing changed, nothing to
		// record.
		audit.MarkSkip(r.Context())
	}

	h.cookie.ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

type RequestResetReq struct {
	Email string `json:"email"`
}

// requestPasswordReset issues a reset token (if the email matches a
// user) and emails the operator a reset link. Always returns 200 with
// a generic message — never confirms whether the email matched a
// user, to avoid account enumeration.
func (h *Handler) requestPasswordReset(w http.ResponseWriter, r *http.Request) {
	var req RequestResetReq
	if !respond.DecodeStrict(w, r, &req) {
		return
	}
	if req.Email == "" {
		respond.ValidationField(w, r, "email", "email is required")
		return
	}

	plaintext, tenantID, targetUserID, err := h.users.IssueResetToken(r.Context(), req.Email)
	switch {
	case errors.Is(err, ErrResetSendCapped):
		// Per-account send cap (store-enforced): no token was issued, so no
		// email goes out. Deliberately NO early return and no bespoke body —
		// it falls through to the one shared response every outcome uses.
		//
		// That matters more here than it did for the old per-address budget:
		// this cap is keyed on the ACCOUNT, so an address with no account can
		// never be capped. A distinguishable "you're throttled" reply would
		// therefore be a reliable account-existence oracle — four requests
		// and an attacker knows. WARN, not ERROR: hitting a 3-per-hour cap is
		// routine, and error-level noise here would read as a system failure.
		slog.Warn("password reset throttled — send skipped", "reason", "per-account cap")
	case err != nil:
		slog.Error("password reset issue failed", "err", err)
		// Generic response — don't expose internal failures
	}

	if plaintext != "" && tenantID != "" {
		// Send is best-effort: failure is logged but never surfaces to
		// the response, since the response shape is fixed at "if your
		// email is on file, you'll get a link" to avoid email
		// enumeration. SMTP misconfiguration shows up as a logged
		// SendPasswordReset error.
		resetLink, ok := h.buildResetLink(plaintext)
		if !ok {
			// DASHBOARD_BASE_URL is unset. Refuse to derive the link origin
			// from the request Host header — a poisoned Host would email the
			// victim a reset link pointing at an attacker domain, leaking the
			// token. Fail safe: don't send a poisonable link. Operator must
			// set DASHBOARD_BASE_URL to enable password-reset emails.
			slog.Error("password reset email not sent: DASHBOARD_BASE_URL is unset; refusing to build a reset link from request headers", "tenant_id", tenantID)
		} else {
			// This handler runs pre-auth (the /v1/auth group has no session
			// middleware), so ctx carries no livemode. The reset email is
			// account-plane, and the production email path is the outbox, whose
			// TxTenant refuses to open without a livemode — so without this the
			// send failed and the operator never got the link. Same gap the audit
			// write below already handles (auditResetRequest sets it too). Canonical
			// (test) partition, like every account-plane row.
			sendCtx := postgres.WithLivemode(r.Context(), false)
			if sendErr := h.email.SendPasswordReset(sendCtx, tenantID, req.Email, resetLink); sendErr != nil {
				slog.Error("password reset email send failed", "err", sendErr)
			}
		}
		// Audit the reset request against the matched account's tenant. The actor is
		// anonymous (any unauthenticated party can trigger a reset email) so it
		// records as 'system'. Only written on a match, so it neither leaks to the
		// requester (the response is identical match-or-not) nor pollutes the log
		// with spray attempts.
		//
		// The row POINTS AT the account (resource_id = the user id) and does not
		// STORE its address — neither in the label nor in metadata, where it used
		// to sit twice. audit_log is append-only, so an email written here could
		// never be erased; the reader resolves it from the users row, which can.
		h.auditResetRequest(r.Context(), r, tenantID, targetUserID)
	} else {
		// No account matched (or issuance failed): no token exists, nothing was
		// mutated, and deliberately nothing is written — a row here would turn
		// the audit log into the account-existence oracle the fixed 200 exists to
		// deny. Declared so the coverage detector reads "nothing to audit" rather
		// than "a mutation lost its row".
		audit.MarkSkip(r.Context())
	}

	// Whether reset emails can actually be DELIVERED on this deployment —
	// server-global (SMTP wired AND DASHBOARD_BASE_URL set), computed
	// independently of whether the email matched a user, so it leaks no
	// account existence (it's deployment posture, not account state). Lets the
	// UI tell a self-hoster their email isn't configured rather than promising
	// a link that can never arrive.
	emailDelivery := "ok"
	if h.dashboardBaseURL == "" || !h.smtpConfigured {
		emailDelivery = "not_configured"
	}
	respond.JSON(w, r, http.StatusOK, map[string]string{
		"message":        "if an account exists for that email, a password-reset link has been sent",
		"email_delivery": emailDelivery,
	})
}

type ConfirmResetReq struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// checkPasswordResetToken probes a reset token's validity without
// consuming it. 200 with {"valid": true} when the token is currently
// usable; a 422 error with code invalid_or_expired when it's expired,
// already used, or unknown. The reset-password page hits this on
// mount so it can render "this link is no longer valid" instead of
// a form the user fills in only to be rejected at submit.
func (h *Handler) checkPasswordResetToken(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if err := h.users.CheckResetToken(r.Context(), token); err != nil {
		respond.ValidationCoded(w, r, "token", "invalid_or_expired", "the reset link is unknown, expired or already used")
		return
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{"valid": true})
}

// confirmPasswordReset sets a new password from a reset link. The password
// change, the sign-out of every session of the user and the audit row commit
// together (Service.ConfirmReset). A password that fails the check is a 422
// on param password; a dead link is a 422 on param token.
func (h *Handler) confirmPasswordReset(w http.ResponseWriter, r *http.Request) {
	var req ConfirmResetReq
	if !respond.DecodeStrict(w, r, &req) {
		return
	}
	if req.Token == "" {
		respond.ValidationField(w, r, "token", "token is required")
		return
	}
	if req.Password == "" {
		respond.ValidationField(w, r, "password", "password is required")
		return
	}

	u, err := h.users.ConfirmReset(r.Context(), req.Token, req.Password)
	if err != nil {
		respond.FromError(w, r, err, "password_reset")
		return
	}

	// The reset link proved the inbox. It marks this browser, so a person
	// held out by guesses from many addresses gets back in: reset from this
	// browser, then sign in from it. It leaves the email's count running for
	// other browsers (LoginThrottle.InboxProved). Logged, not surfaced: the
	// password is already changed.
	if h.throttle != nil {
		if err := h.throttle.InboxProved(w, r, u.Email); err != nil {
			slog.ErrorContext(r.Context(), "auth: sign-in throttle did not mark the browser after a password reset", "user_id", u.ID, "error", err)
		}
	}

	respond.JSON(w, r, http.StatusOK, map[string]string{
		"message": "password updated — sign in with your new password",
	})
}

// writeLoginThrottled answers an attempt the sign-in throttle refused: 429,
// Retry-After in seconds, and the wait in whole minutes. Real and unknown
// emails get the same answer.
func writeLoginThrottled(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	secs := retryAfterSeconds(wait)
	minutes := (secs + 59) / 60
	unit := "minutes"
	if minutes == 1 {
		unit = "minute"
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	respond.Error(w, r, http.StatusTooManyRequests, "login_throttled",
		fmt.Sprintf("Too many sign-in attempts. Try again in %d %s.", minutes, unit))
}

// statusRecorder keeps the status a response was written with, so the
// sign-in metric can tell a refusal from a server error.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// retryAfterSeconds rounds a wait up to whole seconds, at least 1.
func retryAfterSeconds(wait time.Duration) int {
	secs := int((wait + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// buildResetLink constructs the operator-facing URL for the reset
// confirmation page from the configured DASHBOARD_BASE_URL. It never
// derives the origin from the request (Host header, X-Forwarded-Proto):
// a password-reset link is a security-token carrier, and a poisoned Host
// header would let an attacker email the victim a link pointing at their
// own domain to capture the token. Returns ok=false when
// DASHBOARD_BASE_URL is unset so the caller can fail safe rather than
// send a poisonable link. Single-origin prod deployments still set
// DASHBOARD_BASE_URL to their canonical dashboard URL.
func (h *Handler) buildResetLink(token string) (string, bool) {
	if h.dashboardBaseURL == "" {
		return "", false
	}
	return h.dashboardBaseURL + "/reset-password?token=" + token, true
}
