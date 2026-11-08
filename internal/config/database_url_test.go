package config

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// connTarget is what the database driver reads out of a connection URL.
type connTarget struct {
	Host           string
	Port           uint16
	User           string
	Password       string
	Database       string
	TLS            bool
	ChannelBinding string
}

// TestDatabaseURLForms pins how the driver reads each database URL form the
// docs show. The server opens the URL with sql.Open("pgx", url), and the
// driver parses it with pgx.ParseConfig when it connects, so a driver
// upgrade that reads a form differently fails here, not at an operator's
// start-up.
func TestDatabaseURLForms(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want connTarget
	}{
		{
			name: "local development",
			env:  map[string]string{"DATABASE_URL": "postgres://hardcount:hardcount@localhost:5432/hardcount?sslmode=disable"},
			want: connTarget{Host: "localhost", Port: 5432, User: "hardcount", Password: "hardcount", Database: "hardcount", TLS: false, ChannelBinding: "prefer"},
		},
		{
			name: "Compose stack with a hex password",
			env:  map[string]string{"DATABASE_URL": "postgres://hardcount:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822c@postgres:5432/hardcount?sslmode=disable"},
			want: connTarget{Host: "postgres", Port: 5432, User: "hardcount", Password: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822c", Database: "hardcount", TLS: false, ChannelBinding: "prefer"},
		},
		{
			name: "managed Postgres over TLS",
			env:  map[string]string{"DATABASE_URL": "postgres://hardcount:PASSWORD@db-host:5432/hardcount?sslmode=require"},
			want: connTarget{Host: "db-host", Port: 5432, User: "hardcount", Password: "PASSWORD", Database: "hardcount", TLS: true, ChannelBinding: "prefer"},
		},
		{
			name: "TLS with channel binding required and no port",
			env:  map[string]string{"DATABASE_URL": "postgres://hardcount:PASSWORD@db.example.com/hardcount?sslmode=require&channel_binding=require"},
			want: connTarget{Host: "db.example.com", Port: 5432, User: "hardcount", Password: "PASSWORD", Database: "hardcount", TLS: true, ChannelBinding: "require"},
		},
		{
			name: "percent-encoded special characters in the password",
			env:  map[string]string{"DATABASE_URL": "postgres://hardcount:p%40ss%3Aw%2Frd%3F%23%25%2B%20x@db-host:5432/hardcount?sslmode=require"},
			want: connTarget{Host: "db-host", Port: 5432, User: "hardcount", Password: "p@ss:w/rd?#%+ x", Database: "hardcount", TLS: true, ChannelBinding: "prefer"},
		},
		{
			// The DB_HOST set takes the password as written: loadDatabaseURL
			// percent-encodes it.
			name: "DB_HOST set with special characters in the password",
			env:  map[string]string{"DB_HOST": "db-host", "DB_NAME": "hardcount", "DB_USER": "hardcount", "DB_PASSWORD": "p@ss:w/rd?#%+ x"},
			want: connTarget{Host: "db-host", Port: 5432, User: "hardcount", Password: "p@ss:w/rd?#%+ x", Database: "hardcount", TLS: true, ChannelBinding: "prefer"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"DATABASE_URL", "DB_HOST", "DB_NAME", "DB_USER", "DB_PASSWORD", "DB_PORT", "DB_SSLMODE"} {
				t.Setenv(name, tc.env[name])
			}
			cfg, err := LoadDBOnly()
			if err != nil {
				t.Fatalf("LoadDBOnly() error = %v", err)
			}
			parsed, err := pgx.ParseConfig(cfg.URL)
			if err != nil {
				t.Fatalf("pgx.ParseConfig(%q) error = %v", cfg.URL, err)
			}
			got := connTarget{
				Host:           parsed.Host,
				Port:           parsed.Port,
				User:           parsed.User,
				Password:       parsed.Password,
				Database:       parsed.Database,
				TLS:            parsed.TLSConfig != nil,
				ChannelBinding: parsed.ChannelBinding,
			}
			if got != tc.want {
				t.Errorf("pgx.ParseConfig(%q) = %+v, want %+v", cfg.URL, got, tc.want)
			}
		})
	}
}
