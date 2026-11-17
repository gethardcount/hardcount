package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeLinkToken stands in for the link token a public link carries in its
// path. It is plainly fake so a failure that prints it is easy to read.
const fakeLinkToken = "FAKE-LINK-TOKEN-DO-NOT-LOG"

// captureLogs sends every log line to the returned buffer until the test ends.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// panicLine returns the "handler panic" line cut before its stack, so the
// line ends with the path field. It fails the test when no such line was
// logged.
func panicLine(t *testing.T, logs *bytes.Buffer) string {
	t.Helper()
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `msg="handler panic"`) {
			if i := strings.Index(line, " stack="); i >= 0 {
				return line[:i]
			}
			return line
		}
	}
	t.Fatalf("no panic line logged:\n%s", logs.String())
	return ""
}

// panickingWriter is a ResponseWriter whose first status write panics, so
// any answer the router gives panics inside the panic middleware.
type panickingWriter struct{ *httptest.ResponseRecorder }

func (panickingWriter) WriteHeader(int) { panic("writing the status") }

// The production router logs a panic on a link-token address with a path
// that holds no token. The 404 is the answer that panics: it is the one a
// router built on an empty database gives on these paths without touching
// the database.
func TestPanicLog_RealRouterLogsNoLinkToken(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"a miss under the hosted invoice mount keeps its first segment",
			"/v1/public/invoices/" + fakeLinkToken + "/no-such-page", "/v1/…"},
		{"the hosted invoice page's address keeps its first segment",
			"/invoice/" + fakeLinkToken, "/invoice/…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer()
			logs := captureLogs(t)
			w := panickingWriter{httptest.NewRecorder()}
			srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))

			line := panicLine(t, logs)
			if strings.Contains(logs.String(), fakeLinkToken) {
				t.Errorf("the log holds the link token:\n%s", line)
			}
			if !strings.HasSuffix(line, " path="+tc.want) {
				t.Errorf("panic line path is not %q:\n%s", tc.want, line)
			}
		})
	}
}
