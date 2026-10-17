package migrate

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// The boot error is the only thing an operator sees when a migration
// failed midway, so it must name the migration, both force commands and
// the runbook page.
func TestDirtyError_NamesMigrationAndRepair(t *testing.T) {
	versions, err := embeddedUpVersions()
	if err != nil {
		t.Fatal(err)
	}
	latest := versions[len(versions)-1]
	prev := versions[len(versions)-2]

	msg := (&DirtyError{Version: latest}).Error()
	for _, want := range []string{
		fmt.Sprintf("dirty at version %d", latest),
		migrationFileName(latest),
		fmt.Sprintf("`velox migrate force %d` and then `velox migrate`", prev),
		fmt.Sprintf("`velox migrate force %d`", latest),
		RunbookPath,
		"runs in one transaction",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestDirtyError_NoTxMigrationSaysPartial(t *testing.T) {
	noTx, err := versionsWithNoTxHeader("up")
	if err != nil {
		t.Fatal(err)
	}
	for v := range noTx {
		if msg := (&DirtyError{Version: v}).Error(); !strings.Contains(msg, "outside a transaction") {
			t.Errorf("no-tx version %d: message does not warn of a partial apply:\n%s", v, msg)
		}
	}
}

func TestDirtyError_UnknownVersion(t *testing.T) {
	msg := (&DirtyError{Version: 99999}).Error()
	if !strings.Contains(msg, "does not ship migration 99999") || !strings.Contains(msg, RunbookPath) {
		t.Fatalf("unexpected message: %s", msg)
	}
}

func TestAsDirtyError_ConvertsLibraryError(t *testing.T) {
	var de *DirtyError
	if err := asDirtyError(fmt.Errorf("step: %w", migrate.ErrDirty{Version: 42})); !errors.As(err, &de) || de.Version != 42 {
		t.Fatalf("got %v", err)
	}
	other := errors.New("boom")
	if err := asDirtyError(other); err != other {
		t.Fatalf("non-dirty error changed: %v", err)
	}
}

func TestForceTargets(t *testing.T) {
	versions, err := embeddedUpVersions()
	if err != nil {
		t.Fatal(err)
	}
	n := len(versions)

	got, err := ForceTargets(versions[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint{0, versions[0], versions[1]}; !reflect.DeepEqual(got, want) {
		t.Errorf("first: got %v want %v", got, want)
	}
	got, err = ForceTargets(versions[n-1])
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint{versions[n-2], versions[n-1]}; !reflect.DeepEqual(got, want) {
		t.Errorf("latest: got %v want %v", got, want)
	}
	if _, err := ForceTargets(99999); err == nil {
		t.Error("unknown version: want error")
	}
}
