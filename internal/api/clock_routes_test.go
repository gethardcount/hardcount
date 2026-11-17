package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Every route declares how its time is decided, and every declaration is a
// route: the router and clockRoutes are compared in both directions.
func TestClockRoutes_MatchLiveRoutes(t *testing.T) {
	srv := newTestServer()
	walked := map[routeKey]bool{}
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		walked[routeKey{Method: method, Pattern: canonicalRoute(route)}] = true
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	var undeclared, stale []string
	for k := range walked {
		if _, ok := clockRoutes[k]; !ok {
			undeclared = append(undeclared, k.Method+" "+k.Pattern)
		}
	}
	for k := range clockRoutes {
		if !walked[k] {
			stale = append(stale, k.Method+" "+k.Pattern)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(stale)
	if len(undeclared) > 0 {
		t.Errorf(`%d route(s) do not declare how their time is decided:

  %s

Declare each in internal/api/clock_routes.go (clockRoutes): realTime() when the
request names no record on a clock; ofCustomer, ofSubscription, ofInvoice,
ofCreditNote or ofDunningRun when a path parameter names the record; or
decidedIn("package.Type.Method") naming the function that decides after it
reads the record the body or a token names.`, len(undeclared), strings.Join(undeclared, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("%d clockRoutes entr(y/ies) match no route; delete or rename them (keys are canonical chi patterns):\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// Each declaration says what its source needs, and nothing else: a record's
// parameter is in its route's pattern, and a deciding function exists.
func TestClockRoutes_DeclarationsAreWhole(t *testing.T) {
	for k, d := range clockRoutes {
		name := k.Method + " " + k.Pattern
		switch d.Source {
		case clockRealTime:
			if d.Param != "" || d.In != "" {
				t.Errorf("%s: real time takes no parameter or function, has %+v", name, d)
			}
		case clockOfCustomer, clockOfSubscription, clockOfInvoice, clockOfCreditNote, clockOfDunningRun:
			if d.Param == "" || d.In != "" || !strings.Contains(k.Pattern, "{"+d.Param+"}") {
				t.Errorf("%s: the record's parameter %q must be in the pattern, and no function named; has %+v", name, d.Param, d)
			}
		case clockDecidedIn:
			if d.In == "" || d.Param != "" {
				t.Errorf("%s: decidedIn names a function and no parameter; has %+v", name, d)
			} else if err := functionExists(d.In); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		default:
			t.Errorf("%s: no source", name)
		}
	}
}

// functionExists finds the method package.Type.Method in internal/<package>,
// outside its tests.
func functionExists(name string) error {
	parts := strings.Split(name, ".")
	if len(parts) != 3 {
		return fmt.Errorf("%q is not package.Type.Method", name)
	}
	decl := regexp.MustCompile(`(?m)^func \(\w+ \*?` + parts[1] + `\) ` + parts[2] + `\(`)
	files, err := filepath.Glob(filepath.Join("..", parts[0], "*.go"))
	if err != nil || len(files) == 0 {
		return fmt.Errorf("%s: no package internal/%s", name, parts[0])
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if decl.Match(src) {
			return nil
		}
	}
	return fmt.Errorf("%s: no such method in internal/%s", name, parts[0])
}
