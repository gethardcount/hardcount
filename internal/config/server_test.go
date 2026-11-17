package config

import (
	"os"
	"slices"
	"testing"
)

// ServerFromEnv reads each of the server's settings, trimmed, except
// CORS_ALLOWED_ORIGINS, which it keeps as set.
func TestServerFromEnv(t *testing.T) {
	cases := []struct {
		env, value, want string
		got              func(Server) string
	}{
		{"HARDCOUNT_ENCRYPTION_KEY", " key ", "key", func(s Server) string { return s.EncryptionKey }},
		{"HARDCOUNT_EMAIL_BIDX_KEY", " bidx ", "bidx", func(s Server) string { return s.EmailBlindIndexKey }},
		{"DASHBOARD_BASE_URL", " https://app.example ", "https://app.example", func(s Server) string { return s.DashboardBaseURL }},
		{"CUSTOMER_PORTAL_URL", " https://portal.example ", "https://portal.example", func(s Server) string { return s.CustomerPortalURL }},
		{"HOSTED_INVOICE_BASE_URL", " https://pay.example ", "https://pay.example", func(s Server) string { return s.HostedInvoiceBaseURL }},
		{"PAYMENT_UPDATE_URL", " https://update.example ", "https://update.example", func(s Server) string { return s.PaymentUpdateURL }},
		{"PAYMENT_UPDATE_RETURN_URL", " https://return.example ", "https://return.example", func(s Server) string { return s.PaymentUpdateReturnURL }},
		{"HARDCOUNT_API_BASE_URL", " https://api.example ", "https://api.example", func(s Server) string { return s.APIBaseURL }},
		{"POSTMARK_WEBHOOK_USER", " pm-user ", "pm-user", func(s Server) string { return s.PostmarkWebhookUser }},
		{"POSTMARK_WEBHOOK_PASS", " pm-pass ", "pm-pass", func(s Server) string { return s.PostmarkWebhookPass }},
		{"TRUST_PROXY", " 10.0.0.0/8 ", "10.0.0.0/8", func(s Server) string { return s.TrustProxy }},
		{"CORS_ALLOWED_ORIGINS", " https://a.example ", " https://a.example ", func(s Server) string { return s.CORSAllowedOrigins }},
		{"METRICS_TOKEN", " scrape ", "scrape", func(s Server) string { return s.MetricsToken }},
	}
	for _, c := range cases {
		t.Setenv(c.env, c.value)
	}
	s := ServerFromEnv()
	for _, c := range cases {
		if got := c.got(s); got != c.want {
			t.Errorf("%s=%q: read %q, want %q", c.env, c.value, got, c.want)
		}
	}
}

// With CORS_ALLOWED_ORIGINS unset or empty, the dashboard's local origins are
// allowed.
func TestServer_CORSOriginsDefault(t *testing.T) {
	want := []string{"http://localhost:3000", "http://localhost:5173", "http://localhost:5174"}
	for _, c := range []struct {
		name  string
		setup func()
	}{
		{"unset", func() { t.Setenv("CORS_ALLOWED_ORIGINS", ""); _ = os.Unsetenv("CORS_ALLOWED_ORIGINS") }},
		{"empty", func() { t.Setenv("CORS_ALLOWED_ORIGINS", "") }},
	} {
		c.setup()
		if got := ServerFromEnv().CORSOrigins(); !slices.Equal(got, want) {
			t.Errorf("CORS_ALLOWED_ORIGINS %s: origins %v, want the local default %v", c.name, got, want)
		}
	}
}
