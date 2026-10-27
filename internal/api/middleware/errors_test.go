package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the JSON envelope: %q", rec.Body.String())
	}
	return body.Error.Code
}

// A panic answers the JSON 500, not chi's empty one.
func TestRecoverer_PanicAnswersTheEnvelope(t *testing.T) {
	h := Recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError || errorCode(t, rec) != "internal_error" {
		t.Errorf("panic = %d %q, want 500 internal_error", rec.Code, rec.Body.String())
	}
}

// A panic after the handler started its answer leaves that answer alone.
func TestRecoverer_PanicAfterWriteKeepsTheAnswer(t *testing.T) {
	h := Recoverer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"x"}`))
		panic("late")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusCreated || rec.Body.String() != `{"id":"x"}` {
		t.Errorf("late panic = %d %q, want the 201 body untouched", rec.Code, rec.Body.String())
	}
}

// net/http's abort sentinel is passed on, not turned into a 500.
func TestRecoverer_RepanicsAbortHandler(t *testing.T) {
	h := Recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if r := recover(); r != http.ErrAbortHandler { //nolint:errorlint // the sentinel itself
			t.Errorf("recovered %v, want http.ErrAbortHandler re-panicked", r)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

// A handler that runs out of time before writing gets the JSON 504; one
// that answered in time keeps its answer.
func TestTimeout_AnswersTheEnvelopeOnlyWhenNothingWasWritten(t *testing.T) {
	slow := Timeout(10 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	rec := httptest.NewRecorder()
	slow.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusGatewayTimeout || errorCode(t, rec) != "timeout" {
		t.Errorf("slow handler = %d %q, want 504 timeout", rec.Code, rec.Body.String())
	}

	wroteThenSlow := Timeout(10 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		<-r.Context().Done()
	}))
	rec = httptest.NewRecorder()
	wroteThenSlow.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("answered handler = %d %q, want its 200 untouched", rec.Code, rec.Body.String())
	}
}
