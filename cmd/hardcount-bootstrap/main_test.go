package main

import "testing"

func TestSignInURL(t *testing.T) {
	for _, c := range []struct{ name, base, want string }{
		{"unset: the dashboard's default local address", "", "http://localhost:5173/login"},
		{"set", "https://billing.example.com", "https://billing.example.com/login"},
		{"set with a trailing slash", "http://localhost:5190/", "http://localhost:5190/login"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("DASHBOARD_BASE_URL", c.base)
			if got := signInURL(); got != c.want {
				t.Errorf("signInURL() = %q, want %q", got, c.want)
			}
		})
	}
}
