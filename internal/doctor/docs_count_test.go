package doctor_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/sagarsuperuser/velox/internal/doctor"
)

// The docs that state how many checks velox-doctor has must state the real
// number. The count was left behind twice (28 → 30, 30 → 32) when checks were
// added; this fails the PR that adds the next one until the docs say so.
func TestDocsStateTheCheckCount(t *testing.T) {
	for file, pattern := range map[string]string{
		"ENGINEERING.md":                    `velox-doctor, (\d+) checks`,
		"benchmarks/failure-correctness.md": "`velox-doctor` now has (\\d+) checks",
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", "docs", file))
		if err != nil {
			t.Fatalf("read docs/%s: %v", file, err)
		}
		m := regexp.MustCompile(pattern).FindSubmatch(b)
		if m == nil {
			t.Errorf("docs/%s: no %q sentence; update this test with the doc", file, pattern)
			continue
		}
		if n, _ := strconv.Atoi(string(m[1])); n != len(doctor.Checks) {
			t.Errorf("docs/%s says %d checks; velox-doctor has %d", file, n, len(doctor.Checks))
		}
	}
}
