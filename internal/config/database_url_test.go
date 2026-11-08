package config

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// connTarget is what the database driver reads out of a connection URL.
type connTarget struct {
	Host            string
	Port            uint16
	User            string
	Password        string
	Database        string
	TLS             bool
	ChannelBinding  string
	ApplicationName string
}

// TestDatabaseURLForms pins how the driver reads each database URL form the
// docs show, and each URL rule that docs/settings.md and the CHANGELOG state.
// The server opens the URL with sql.Open("pgx", url), and the driver parses
// it with pgx.ParseConfig when it connects, so a driver upgrade that reads a
// form differently fails here, not at an operator's start-up.
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
		{
			// The second sslmode wins, so this URL connects without TLS.
			name: "a repeated parameter takes its last value",
			env:  map[string]string{"DATABASE_URL": "postgres://hardcount:PASSWORD@db-host:5432/hardcount?sslmode=require&sslmode=disable"},
			want: connTarget{Host: "db-host", Port: 5432, User: "hardcount", Password: "PASSWORD", Database: "hardcount", TLS: false, ChannelBinding: "prefer"},
		},
		{
			name: "a + in a parameter stays a +",
			env:  map[string]string{"DATABASE_URL": "postgres://hardcount:PASSWORD@db-host:5432/hardcount?sslmode=disable&application_name=a+b"},
			want: connTarget{Host: "db-host", Port: 5432, User: "hardcount", Password: "PASSWORD", Database: "hardcount", TLS: false, ChannelBinding: "prefer", ApplicationName: "a+b"},
		},
		{
			name: "an IPv6 host in brackets",
			env:  map[string]string{"DATABASE_URL": "postgres://hardcount:PASSWORD@[::1]:5432/hardcount?sslmode=disable"},
			want: connTarget{Host: "::1", Port: 5432, User: "hardcount", Password: "PASSWORD", Database: "hardcount", TLS: false, ChannelBinding: "prefer"},
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
				Host:            parsed.Host,
				Port:            parsed.Port,
				User:            parsed.User,
				Password:        parsed.Password,
				Database:        parsed.Database,
				TLS:             parsed.TLSConfig != nil,
				ChannelBinding:  parsed.ChannelBinding,
				ApplicationName: parsed.RuntimeParams["application_name"],
			}
			if got != tc.want {
				t.Errorf("pgx.ParseConfig(%q) = %+v, want %+v", cfg.URL, got, tc.want)
			}
		})
	}
}

// TestLoad_RefusesUnencodedAtInCredentials checks that start-up refuses a
// database URL with an unencoded @ in its user name or password, and that
// the error does not hold the password. The driver ends the password at the
// first @, as psql does, so the rest of the password becomes the host name.
// Opening that URL fails with a host-name lookup error that prints it, and
// the server would log that error on every start.
func TestLoad_RefusesUnencodedAtInCredentials(t *testing.T) {
	const (
		head = "S3cr"
		tail = "tPartOfPassword"
	)
	badURL := "postgres://hardcount:" + head + "@" + tail + "@db.invalid:5432/hardcount?sslmode=disable"

	// The reason for the refusal: the driver reads the password's tail as
	// part of the host name.
	parsed, err := pgx.ParseConfig(badURL)
	if err != nil {
		t.Fatalf("pgx.ParseConfig(%q) error = %v", badURL, err)
	}
	if want := tail + "@db.invalid"; parsed.Host != want || parsed.Password != head {
		t.Fatalf("pgx.ParseConfig(%q): host %q, password %q; want host %q, password %q", badURL, parsed.Host, parsed.Password, want, head)
	}

	cases := []struct {
		setting string
		env     map[string]string
	}{
		{
			setting: "DATABASE_URL",
			env:     map[string]string{"DATABASE_URL": badURL},
		},
		{
			setting: "APP_DATABASE_URL",
			env: map[string]string{
				"DATABASE_URL":     "postgres://hardcount:hardcount@db.invalid:5432/hardcount?sslmode=disable",
				"APP_DATABASE_URL": strings.Replace(badURL, "//hardcount:", "//hardcount_app:", 1),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.setting, func(t *testing.T) {
			for _, name := range []string{"APP_ENV", "DATABASE_URL", "APP_DATABASE_URL", "DB_HOST", "DB_NAME", "DB_USER", "DB_PASSWORD", "DB_PORT", "DB_SSLMODE"} {
				t.Setenv(name, tc.env[name])
			}

			// What the server does at start-up: load the settings, then open
			// the pool for the URL under test.
			cfg, err := Load()
			if err == nil {
				dbCfg := cfg.DB
				if tc.setting == "APP_DATABASE_URL" {
					dbCfg.URL = cfg.DB.AppURL
				}
				dbCfg.PingTimeout = 2 * time.Second
				db, openErr := OpenPostgres(dbCfg)
				if openErr == nil {
					_ = db.Close() // the test fails below; a close error adds nothing
				}
				err = openErr
			}

			if err == nil {
				t.Fatal("start-up error = nil; want a refusal")
			}
			msg := err.Error()
			if strings.Contains(msg, tail) || strings.Contains(msg, head) {
				t.Fatalf("start-up error holds the password: %v", err)
			}
			if !strings.HasPrefix(msg, tc.setting+" ") || !strings.Contains(msg, "%40") {
				t.Fatalf("start-up error = %v; want it to name %s and say to encode @ as %%40", err, tc.setting)
			}
		})
	}
}

// TestCheckDatabaseURL covers the URL shapes CheckDatabaseURL tells apart.
// It refuses exactly the URLs in which the driver reads an @ as part of the
// host or port: each row also checks the driver's own reading.
func TestCheckDatabaseURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		refused bool
	}{
		{name: "plain password", url: "postgres://u:secret@db-host:5432/db?sslmode=require"},
		{name: "percent-encoded @", url: "postgres://u:p%40ss@db-host:5432/db"},
		{name: "postgresql scheme", url: "postgresql://u:p%40ss@db-host/db"},
		{name: "no user name or password", url: "postgres://db-host:5432/db"},
		{name: "@ in a parameter of a URL without a path", url: "postgres://u:p@db-host?application_name=a@b"},
		{name: "@ in a parameter after the path", url: "postgres://u:p@db-host/db?application_name=a@b"},
		{name: "keyword/value string", url: "host=db-host user=u password=p@ss@x dbname=db"},
		{name: "unencoded @ in the password", url: "postgres://u:p@ss@db-host:5432/db", refused: true},
		{name: "unencoded @ in the password, postgresql scheme", url: "postgresql://u:p@ss@db-host/db", refused: true},
		{name: "unencoded @ in the user name", url: "postgres://u@server:p@db-host:5432/db", refused: true},
		{name: "unencoded @ and : in the password", url: "postgres://u:p@ss:x@db-host:5432/db", refused: true},
		{name: "unencoded ? and @ in the password", url: "postgres://u:p?x@ss@db-host/db", refused: true},
		{name: "unencoded @ in a URL without a path", url: "postgres://u:p@ss@db-host?sslmode=require", refused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckDatabaseURL("DATABASE_URL", tc.url)
			if refused := err != nil; refused != tc.refused {
				t.Fatalf("CheckDatabaseURL(%q) error = %v; want refused = %v", tc.url, err, tc.refused)
			}
			// The driver puts the @ into the host, or into a port it then
			// cannot parse.
			host := ""
			parsed, perr := pgx.ParseConfig(tc.url)
			if perr == nil {
				host = parsed.Host
			}
			if misread := perr != nil || strings.Contains(host, "@"); misread != tc.refused {
				t.Fatalf("pgx.ParseConfig(%q) error = %v, host %q; want a misread = %v", tc.url, perr, host, tc.refused)
			}
		})
	}
}
