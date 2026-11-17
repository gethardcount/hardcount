package config

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// Server is what the API server reads from the environment. ServerFromEnv is
// its one reader: api.NewServer calls it, and Load checks the same values
// (validateFatal, Validate).
type Server struct {
	// EncryptionKey is HARDCOUNT_ENCRYPTION_KEY: it encrypts customer PII and
	// keys the login throttle.
	EncryptionKey string
	// EmailBlindIndexKey is HARDCOUNT_EMAIL_BIDX_KEY.
	EmailBlindIndexKey string

	// Hardcount's own public addresses (publicAddressSettings).
	DashboardBaseURL       string // DASHBOARD_BASE_URL
	CustomerPortalURL      string // CUSTOMER_PORTAL_URL
	HostedInvoiceBaseURL   string // HOSTED_INVOICE_BASE_URL
	PaymentUpdateURL       string // PAYMENT_UPDATE_URL
	PaymentUpdateReturnURL string // PAYMENT_UPDATE_RETURN_URL
	APIBaseURL             string // HARDCOUNT_API_BASE_URL

	// CookieDomain is HARDCOUNT_COOKIE_DOMAIN, the sign-in cookie's Domain.
	CookieDomain string

	// PostmarkWebhookUser and PostmarkWebhookPass are the basic-auth pair
	// Postmark's webhook sends (POSTMARK_WEBHOOK_USER, POSTMARK_WEBHOOK_PASS).
	PostmarkWebhookUser string
	PostmarkWebhookPass string

	// TrustProxy is TRUST_PROXY, trimmed: the proxies whose forwarded client
	// addresses the router honours. It is required in production. Behind a
	// balancer with it unset, every client address is the balancer's, so the
	// per-IP limits collapse into one bucket. "none" declares that there is
	// no proxy. TrustedProxies parses it.
	TrustProxy string
	// CORSAllowedOrigins is CORS_ALLOWED_ORIGINS as set. CORSOrigins parses it.
	CORSAllowedOrigins string

	// MetricsToken is METRICS_TOKEN, the bearer token /metrics asks for.
	MetricsToken string
}

// corsDefaultOrigins are the dashboard's local origins, allowed when
// CORS_ALLOWED_ORIGINS is unset or empty.
const corsDefaultOrigins = "http://localhost:3000,http://localhost:5173,http://localhost:5174"

// ServerFromEnv reads the server's settings from the environment.
func ServerFromEnv() Server {
	trimmed := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	return Server{
		EncryptionKey:          trimmed("HARDCOUNT_ENCRYPTION_KEY"),
		EmailBlindIndexKey:     trimmed("HARDCOUNT_EMAIL_BIDX_KEY"),
		DashboardBaseURL:       trimmed("DASHBOARD_BASE_URL"),
		CustomerPortalURL:      trimmed("CUSTOMER_PORTAL_URL"),
		HostedInvoiceBaseURL:   trimmed("HOSTED_INVOICE_BASE_URL"),
		PaymentUpdateURL:       trimmed("PAYMENT_UPDATE_URL"),
		PaymentUpdateReturnURL: trimmed("PAYMENT_UPDATE_RETURN_URL"),
		APIBaseURL:             trimmed("HARDCOUNT_API_BASE_URL"),
		CookieDomain:           trimmed("HARDCOUNT_COOKIE_DOMAIN"),
		PostmarkWebhookUser:    trimmed("POSTMARK_WEBHOOK_USER"),
		PostmarkWebhookPass:    trimmed("POSTMARK_WEBHOOK_PASS"),
		TrustProxy:             trimmed("TRUST_PROXY"),
		CORSAllowedOrigins:     os.Getenv("CORS_ALLOWED_ORIGINS"),
		MetricsToken:           trimmed("METRICS_TOKEN"),
	}
}

// Check refuses a set TRUST_PROXY or CORS_ALLOWED_ORIGINS the server could not
// use, with the setting's name: an entry that does not parse, or a value that
// names nothing. Load calls it, and so does api.NewServer, so a server built
// without Load refuses the same values instead of dropping them.
func (s Server) Check() error {
	if _, err := ParseTrustProxy(s.TrustProxy); err != nil {
		return fmt.Errorf("%w — set TRUST_PROXY to the balancer/ingress IP(s) or CIDR(s), comma-separated (e.g. 10.0.0.0/8,127.0.0.1), or TRUST_PROXY=none if replicas accept client connections directly (docs/ops/high-availability.md)", err)
	}
	if _, err := ParseCORSOrigins(s.CORSAllowedOrigins); err != nil {
		return fmt.Errorf("%w (docs/settings.md)", err)
	}
	return nil
}

// TrustedProxies is TrustProxy parsed. Check refuses a value with an entry
// that does not parse.
func (s Server) TrustedProxies() []*net.IPNet {
	nets, _ := ParseTrustProxy(s.TrustProxy)
	return nets
}

// CORSOrigins is CORSAllowedOrigins parsed, or the local default when it is
// unset or empty. Check refuses a value it could not use.
func (s Server) CORSOrigins() []string {
	raw := s.CORSAllowedOrigins
	if raw == "" {
		raw = corsDefaultOrigins
	}
	origins, _ := ParseCORSOrigins(raw)
	return origins
}

// publicAddresses pairs each public-address setting with its value.
func (s Server) publicAddresses() map[string]string {
	return map[string]string{
		"DASHBOARD_BASE_URL":        s.DashboardBaseURL,
		"CUSTOMER_PORTAL_URL":       s.CustomerPortalURL,
		"HOSTED_INVOICE_BASE_URL":   s.HostedInvoiceBaseURL,
		"PAYMENT_UPDATE_URL":        s.PaymentUpdateURL,
		"PAYMENT_UPDATE_RETURN_URL": s.PaymentUpdateReturnURL,
		"HARDCOUNT_API_BASE_URL":    s.APIBaseURL,
	}
}
