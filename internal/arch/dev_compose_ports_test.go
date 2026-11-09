package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every port the root docker-compose.yml publishes listens on 127.0.0.1 only.
// A port written as "5432:5432" listens on every network interface, so anyone
// on the same network reaches the dev Postgres, Redis and Mailpit, and the
// Postgres password is the well-known "hardcount".
func TestDevCompose_PortsListenOnLoopbackOnly(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Services map[string]struct {
			Ports []any `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	published := 0
	for name, svc := range file.Services {
		for _, port := range svc.Ports {
			published++
			if !loopbackOnly(port) {
				t.Errorf("docker-compose.yml: service %s publishes %v on every network interface; write it as \"127.0.0.1:HOST:CONTAINER\"", name, port)
			}
		}
	}
	// Guard against the parse silently finding nothing.
	if published == 0 {
		t.Fatal("found no published ports in docker-compose.yml; did its format change?")
	}
}

// loopbackOnly reports whether a Compose ports entry binds 127.0.0.1, in the
// short form ("127.0.0.1:5432:5432") or the long form (host_ip: 127.0.0.1).
func loopbackOnly(port any) bool {
	switch p := port.(type) {
	case string:
		return strings.HasPrefix(p, "127.0.0.1:")
	case map[string]any:
		return p["host_ip"] == "127.0.0.1"
	}
	return false
}
