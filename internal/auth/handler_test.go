package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// retireFailStore fails the write that retires the old key, after the
// replacement has been created.
type retireFailStore struct {
	*memStore
	err error
}

func (s *retireFailStore) Revoke(context.Context, string, string) (domain.APIKey, error) {
	return domain.APIKey{}, s.err
}

// A store failure while retiring the old key is a server fault: the handler
// must answer 500 with the generic body, not a 4xx carrying the error text.
func TestRotateHandler_RetireStoreFailureIs500Generic(t *testing.T) {
	store := &retireFailStore{memStore: newMemStore(), err: errors.New("pq: connection refused")}
	svc := NewService(store)
	ctx := WithTenantID(context.Background(), "t1")

	original, err := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "rotate-me"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/"+original.Key.ID+"/rotate", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	NewHandler(svc).Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Type != "api_error" {
		t.Errorf("error type: got %q, want api_error", body.Error.Type)
	}
	if body.Error.Message != "an internal error occurred" {
		t.Errorf("message: got %q, want the generic internal-error message", body.Error.Message)
	}
	raw := rec.Body.String()
	for _, leak := range []string{"retire old key", "replacement", "connection refused", "vlx_"} {
		if strings.Contains(raw, leak) {
			t.Errorf("response body carries internal text %q: %s", leak, raw)
		}
	}
}
