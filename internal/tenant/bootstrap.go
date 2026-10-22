package tenant

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// BootstrapHandler provides a one-time setup endpoint for creating the
// first tenant + owner user + API keys. Protected by the
// VELOX_BOOTSTRAP_TOKEN env var; only works while no tenants exist.
// The actual provisioning is tenant.RunBootstrap — the single writer
// shared with cmd/velox-bootstrap (ADR-073).
type BootstrapHandler struct {
	db    *postgres.DB
	deps  BootstrapDeps
	token string // Required token from VELOX_BOOTSTRAP_TOKEN env var; empty = disabled
}

func NewBootstrapHandler(db *postgres.DB, deps BootstrapDeps) *BootstrapHandler {
	return &BootstrapHandler{
		db:    db,
		deps:  deps,
		token: strings.TrimSpace(os.Getenv("VELOX_BOOTSTRAP_TOKEN")),
	}
}

func (h *BootstrapHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.bootstrap)
	return r
}

// BootstrapRequest: the token travels in the body or the Authorization
// header, NEVER a query string — query strings land in proxy access
// logs. Owner fields are optional; defaults live in RunBootstrap.
type BootstrapRequest struct {
	TenantName    string `json:"tenant_name"`
	Token         string `json:"token"`
	OwnerEmail    string `json:"owner_email"`
	OwnerPassword string `json:"owner_password"`
}

type bootstrapResponse struct {
	Tenant             domain.Tenant `json:"tenant"`
	OwnerEmail         string        `json:"owner_email"`
	OwnerPassword      string        `json:"owner_password"`
	PasswordGenerated  bool          `json:"password_generated"`
	SecretKeyTest      string        `json:"secret_key_test"`
	SecretKeyLive      string        `json:"secret_key_live"`
	PublishableKeyTest string        `json:"publishable_key_test"`
	Message            string        `json:"message"`
}

func (h *BootstrapHandler) bootstrap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Guard order (ADR-073): already-bootstrapped FIRST, before any
	// token comparison. The old token-first order made the 403-vs-409
	// split a PERPETUAL token-validity oracle on bootstrapped installs;
	// checking bootstrapped-ness first also hides whether a token is
	// even configured. The trade: unauthenticated probes learn
	// virgin-install state and cost one cheap SELECT — accepted
	// (rate-limited since P12). This check is advisory for UX; the
	// authoritative guard is RunBootstrap's re-check under the
	// bootstrap advisory lock.
	var bootstrapped bool
	if err := h.db.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tenants)`).Scan(&bootstrapped); err != nil {
		respond.InternalError(w, r)
		return
	}
	if bootstrapped {
		respond.Error(w, r, http.StatusConflict, "invalid_request_error", "already_bootstrapped",
			"bootstrap already completed — tenants exist")
		return
	}

	if h.token == "" {
		respond.Error(w, r, http.StatusForbidden, "authentication_error", "forbidden",
			"bootstrap disabled — set VELOX_BOOTSTRAP_TOKEN env var to enable")
		return
	}

	var req BootstrapRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	provided := strings.TrimSpace(req.Token)
	if provided == "" {
		provided = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	// Constant-time compare so the bootstrap token can't be recovered by
	// timing how long the rejection takes (a byte-by-byte string != short-
	// circuits on the first mismatched byte). The empty-token disable case is
	// already handled above; subtle returns 0 on a length mismatch.
	if subtle.ConstantTimeCompare([]byte(provided), []byte(h.token)) != 1 {
		respond.Error(w, r, http.StatusForbidden, "authentication_error", "forbidden",
			"invalid bootstrap token")
		return
	}

	result, err := RunBootstrap(ctx, h.db, h.deps, BootstrapOpts{
		TenantName:      req.TenantName,
		OwnerEmail:      req.OwnerEmail,
		OwnerPassword:   req.OwnerPassword,
		FirstTenantOnly: true,
	})
	if err != nil {
		if errors.Is(err, ErrAlreadyBootstrapped) {
			// Race loser: another request committed between our
			// advisory check and RunBootstrap's authoritative one.
			respond.Error(w, r, http.StatusConflict, "invalid_request_error", "already_bootstrapped",
				"bootstrap already completed — tenants exist")
			return
		}
		// Validation (bad email, password under user.MinPasswordLength)
		// → 422 with the offending field; conflicts → 409. Nothing was
		// written in any of these cases — RunBootstrap validates before
		// its first write and runs one all-or-nothing tx.
		respond.FromError(w, r, err, "bootstrap")
		return
	}

	// Credentials + raw keys transit exactly once; keep every cache
	// (browser, proxy) out of the loop.
	w.Header().Set("Cache-Control", "no-store")
	respond.JSON(w, r, http.StatusCreated, bootstrapResponse{
		Tenant:             result.Tenant,
		OwnerEmail:         result.OwnerUser.Email,
		OwnerPassword:      result.OwnerPassword,
		PasswordGenerated:  result.PasswordGenerated,
		SecretKeyTest:      result.TestSecretKey,
		SecretKeyLive:      result.LiveSecretKey,
		PublishableKeyTest: result.TestPublishableKey,
		Message:            "Bootstrap complete. Save these credentials — they will not be shown again.",
	})
}
