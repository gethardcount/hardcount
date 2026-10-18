package user

import "github.com/prometheus/client_golang/prometheus"

// loginAttempts counts dashboard sign-in attempts by outcome:
//
//   - ok: the right password; a session was issued
//   - bad_credentials: a wrong password, or an email with no account
//   - throttled: refused by the sign-in throttle before the password check
//   - refused: the right password, but the user cannot sign in, such as a
//     removed member with no tenant (a 4xx answer)
//   - error: the server failed, such as the throttle or the account store
//     (a 5xx answer)
//
// A rising bad_credentials or throttled rate is someone guessing passwords.
var loginAttempts *prometheus.CounterVec

func init() {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "velox_auth_login_total",
		Help: "Dashboard sign-in attempts by outcome: ok, bad_credentials, throttled (refused by the sign-in throttle before the password check), refused (the right password for a user who cannot sign in) or error (a server failure).",
	}, []string{"outcome"})
	if err := prometheus.DefaultRegisterer.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			loginAttempts = are.ExistingCollector.(*prometheus.CounterVec)
		} else {
			panic(err)
		}
	} else {
		loginAttempts = c
	}
	// Every outcome is exported from the start at 0, so a rate alert has a
	// series to read before the first attempt of that kind.
	for _, o := range []string{"ok", "bad_credentials", "throttled", "refused", "error"} {
		loginAttempts.WithLabelValues(o)
	}
}

func recordLogin(outcome string) { loginAttempts.WithLabelValues(outcome).Inc() }
