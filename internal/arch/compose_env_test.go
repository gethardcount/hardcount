package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var envExampleSetting = regexp.MustCompile(`^#? ?([A-Z][A-Z0-9_]*)=`)

// Every setting deploy/compose/.env.example offers must reach a container
// through deploy/compose/docker-compose.yml. Compose only passes the names its
// file lists, so a setting missing there is silently dropped: the operator sets
// it in .env and Velox never sees it (#1150: POSTMARK_WEBHOOK_USER/PASS and
// VELOX_API_BASE_URL).
func TestComposeEnvExample_EverySettingReachesCompose(t *testing.T) {
	root := repoRoot(t)
	example, err := os.ReadFile(filepath.Join(root, "deploy", "compose", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(filepath.Join(root, "deploy", "compose", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(example), "\n") {
		m := envExampleSetting.FindStringSubmatch(line)
		if m == nil || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		used := regexp.MustCompile(`\$\{` + m[1] + `[:}]`)
		if !used.Match(compose) {
			t.Errorf("%s is in deploy/compose/.env.example but docker-compose.yml never reads ${%s}; add it to the service's environment", m[1], m[1])
		}
	}
	// Guard against the pattern silently matching nothing.
	if len(seen) < 20 {
		t.Fatalf("found only %d settings in .env.example; did its format change?", len(seen))
	}
}
