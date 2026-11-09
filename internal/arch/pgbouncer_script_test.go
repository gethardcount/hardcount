package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/pgbouncer-test.sh picks the Docker network that reaches the dev
// Postgres, which the root docker-compose.yml publishes on 127.0.0.1 only.
// Docker Desktop forwards host.docker.internal to this computer's 127.0.0.1,
// so the bridge network works there. Docker Engine on Linux points that name
// at the Docker bridge, which reaches no port bound to 127.0.0.1, so the
// script must use the host network there. On the host network PgBouncer
// itself must listen on 127.0.0.1 only: its test credentials are in the repo.
//
// The test runs the real script with fake uname, docker, psql and go
// commands first on PATH, and reads the PgBouncer container it would start.
func TestPgBouncerScript_PicksTheNetworkThatReachesLoopbackPostgres(t *testing.T) {
	for _, tool := range []string{"bash", "perl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found; scripts/pgbouncer-test.sh needs it", tool)
		}
	}
	script := filepath.Join(repoRoot(t), "scripts", "pgbouncer-test.sh")

	cases := []struct {
		name        string
		uname       string
		dockerOS    string
		pgbMode     string // "" leaves PGB_MODE unset
		wantHostNet bool
	}{
		{name: "Docker Engine on Linux", uname: "Linux", dockerOS: "Ubuntu 24.04.1 LTS", wantHostNet: true},
		{name: "Docker Desktop on Linux", uname: "Linux", dockerOS: "Docker Desktop", wantHostNet: false},
		{name: "Docker Desktop on macOS", uname: "Darwin", dockerOS: "Docker Desktop", wantHostNet: false},
		{name: "PGB_MODE=local wins on Docker Engine", uname: "Linux", dockerOS: "Ubuntu 24.04.1 LTS", pgbMode: "local", wantHostNet: false},
		{name: "PGB_MODE=ci wins on Docker Desktop", uname: "Darwin", dockerOS: "Docker Desktop", pgbMode: "ci", wantHostNet: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, out := t.TempDir(), t.TempDir()
			writeFake(t, bin, "uname", `echo "$FAKE_UNAME"`)
			writeFake(t, bin, "psql", `exit 0`)
			writeFake(t, bin, "go", `exit 0`)
			// docker info prints the fake OS. The one-shot run that resolves
			// host.docker.internal prints an address. The detached run that
			// starts PgBouncer records its flags and the config it mounts.
			writeFake(t, bin, "docker", `
case "$1" in
  info) echo "$FAKE_DOCKER_OS" ;;
  run)
    if [ "$2" = "-d" ]; then
      echo "$*" > "$FAKE_OUT/run-args"
      for a in "$@"; do
        case "$a" in *:/etc/pgbouncer) cp "${a%:/etc/pgbouncer}/pgbouncer.ini" "$FAKE_OUT/" ;; esac
      done
    else
      echo "192.168.65.254  STREAM host.docker.internal"
    fi ;;
esac
exit 0`)

			cmd := exec.Command("bash", script)
			cmd.Env = append(envWithout("PGB_"),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FAKE_UNAME="+tc.uname, "FAKE_DOCKER_OS="+tc.dockerOS, "FAKE_OUT="+out)
			if tc.pgbMode != "" {
				cmd.Env = append(cmd.Env, "PGB_MODE="+tc.pgbMode)
			}
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("scripts/pgbouncer-test.sh failed: %v\n%s", err, b)
			}

			args := readFile(t, filepath.Join(out, "run-args"))
			ini := readFile(t, filepath.Join(out, "pgbouncer.ini"))
			backend, listen := iniValue(ini, "hardcount_test"), iniValue(ini, "listen_addr")
			if tc.wantHostNet {
				if !strings.Contains(args, "--network host") {
					t.Errorf("PgBouncer must run on the host network, which reaches 127.0.0.1:5432; got docker %s", args)
				}
				if want := "host=127.0.0.1 port=5432 dbname=hardcount_test"; backend != want {
					t.Errorf("backend = %q, want %q", backend, want)
				}
				if listen != "127.0.0.1" {
					t.Errorf("listen_addr = %q, want 127.0.0.1: on the host network 0.0.0.0 opens PgBouncer to the network", listen)
				}
			} else {
				if !strings.Contains(args, "-p 127.0.0.1:6432:6432") || strings.Contains(args, "--network host") {
					t.Errorf("PgBouncer must run on the bridge network with 6432 published on 127.0.0.1; got docker %s", args)
				}
				if want := "host=192.168.65.254 port=5432 dbname=hardcount_test"; backend != want {
					t.Errorf("backend = %q, want %q (host.docker.internal's address)", backend, want)
				}
				if listen != "0.0.0.0" {
					t.Errorf("listen_addr = %q, want 0.0.0.0: Docker forwards a published port to the container's own address", listen)
				}
			}
		})
	}
}

// iniValue returns the value of the first "key = value" line in a PgBouncer
// config, or "" when there is none.
func iniValue(ini, key string) string {
	for _, line := range strings.Split(ini, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func writeFake(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// envWithout returns this process's environment minus every variable whose
// name starts with prefix, so a PGB_MODE set in the shell cannot steer a case.
func envWithout(prefix string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, prefix) {
			env = append(env, kv)
		}
	}
	return env
}
