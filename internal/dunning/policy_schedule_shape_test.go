package dunning

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
)

// postPolicy sends body to POST /policies on a handler over the in-memory
// store and returns the status and the decoded response.
func postPolicy(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	h := NewHandler(NewService(newMemStore(), nil, nil))
	req := httptest.NewRequest(http.MethodPost, "/policies", strings.NewReader(body))
	req = req.WithContext(auth.WithTenantID(req.Context(), "t1"))
	rr := httptest.NewRecorder()
	h.createPolicy(rr, req)
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
	return rr.Code, out
}

// errorParam is the field a validation error names, and its message.
func errorParam(resp map[string]any) (param, message string) {
	e, _ := resp["error"].(map[string]any)
	param, _ = e["param"].(string)
	message, _ = e["message"].(string)
	return param, message
}

// TestCreatePolicy_RetryDaysIsTheSchedule: a policy's schedule is one list,
// retry_days. Each entry is the whole days to wait before a retry: the first
// counts from the failed payment, each next one from the retry before it.
// The number of retries is the length of the list, so the policy carries no
// separate count and no separate wait before the first retry.
func TestCreatePolicy_RetryDaysIsTheSchedule(t *testing.T) {
	code, resp := postPolicy(t, `{"name":"Three retries","enabled":true,"retry_days":[3,4,7]}`)
	if code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %v", code, resp)
	}
	days, _ := resp["retry_days"].([]any)
	if len(days) != 3 || days[0] != float64(3) || days[1] != float64(4) || days[2] != float64(7) {
		t.Errorf("retry_days = %v, want [3 4 7]", resp["retry_days"])
	}
	for _, gone := range []string{"retry_schedule", "max_retry_attempts", "grace_period_days"} {
		if _, ok := resp[gone]; ok {
			t.Errorf("response still carries %s = %v", gone, resp[gone])
		}
	}
}

// TestCreatePolicy_OldScheduleFieldsRefused: the three fields the list
// replaced are refused by name with a 422 that points at retry_days.
// Ignoring them would store a schedule the caller did not write: a caller
// still sending retry_schedule ["1h"] would get retries days apart.
func TestCreatePolicy_OldScheduleFieldsRefused(t *testing.T) {
	for field, value := range map[string]string{
		"retry_schedule":     `["72h","120h"]`,
		"max_retry_attempts": `3`,
		"grace_period_days":  `3`,
	} {
		t.Run(field, func(t *testing.T) {
			code, resp := postPolicy(t, `{"name":"Old","enabled":true,"retry_days":[3,4,7],"`+field+`":`+value+`}`)
			param, msg := errorParam(resp)
			if code != http.StatusUnprocessableEntity || param != field || !strings.Contains(msg, "retry_days") {
				t.Errorf("status %d param %q message %q, want 422 naming %s and pointing at retry_days", code, param, msg, field)
			}
		})
	}
}

// TestCreatePolicy_RetryDaysRefused: retry_days is required, holds 1 to 15
// entries, and each entry is a whole number of days from 1 to 30. A wait of
// part of a day (1.5, "36h") cannot be written: the schedule's unit is the
// day.
func TestCreatePolicy_RetryDaysRefused(t *testing.T) {
	for name, days := range map[string]string{
		"missing":       ``,
		"empty":         `,"retry_days":[]`,
		"zero":          `,"retry_days":[3,0]`,
		"negative":      `,"retry_days":[-1]`,
		"over 30 days":  `,"retry_days":[31]`,
		"16 retries":    `,"retry_days":[1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1]`,
		"part of a day": `,"retry_days":[1.5]`,
		"a duration":    `,"retry_days":["36h"]`,
		"null entry":    `,"retry_days":[3,null]`,
		"not a list":    `,"retry_days":3`,
	} {
		t.Run(name, func(t *testing.T) {
			code, resp := postPolicy(t, `{"name":"Bad","enabled":true`+days+`}`)
			if param, msg := errorParam(resp); code != http.StatusUnprocessableEntity || param != "retry_days" {
				t.Errorf("status %d param %q (%s), want 422 on retry_days", code, param, msg)
			}
		})
	}
	// The bounds themselves are allowed.
	for _, days := range []string{`[1]`, `[30]`, `[1,1,1,1,1,1,1,1,1,1,1,1,1,1,30]`} {
		if code, resp := postPolicy(t, `{"name":"Edge","enabled":true,"retry_days":`+days+`}`); code != http.StatusCreated {
			t.Errorf("retry_days %s: status %d, want 201: %v", days, code, resp)
		}
	}
}
