package subscription_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/subscription"
)

// TestList_CollectionPausedFilter: GET /v1/subscriptions?collection=paused
// returns the live subscriptions whose collection is paused, whether an
// operator or dunning set the pause, with or without a resume date, and its
// total counts the same set (sweep E6: the Paused tab sent ?status=paused,
// which no row has, so it was always empty). A canceled subscription that
// still carries pause columns is not being collected for any reason the pause
// explains, so it is left out.
//
// "Paused" means in force now, the engine's rule (PauseCollection.InForceAt,
// notPausedForCollection): a timed pause whose resumes_at has passed no
// longer holds collection, though the resumer leaves it on the row while a
// period ending before resumes_at is still open. "Now" is the subscription's
// test clock when it has one.
func TestList_CollectionPausedFilter(t *testing.T) {
	s := newBFSeed(t, "cpaused")
	next := s.now.Add(7 * 24 * time.Hour)
	s.sub(bfRow{id: "sub_cp_running", nextBilling: next})
	s.sub(bfRow{id: "sub_cp_paused", nextBilling: next})
	s.sub(bfRow{id: "sub_cp_timed", nextBilling: next})
	s.sub(bfRow{id: "sub_cp_trial", status: "trialing", nextBilling: next, trialEnd: &next})
	s.sub(bfRow{id: "sub_cp_canceled", nextBilling: next})
	// The resume date has passed, but a period that ended before it is still
	// open, so the resumer has not cleared the pause yet.
	s.sub(bfRow{id: "sub_cp_elapsed", nextBilling: s.now.Add(-2 * time.Hour)})
	// Pinned to a clock frozen ten days ago: a resume date one hour ago on
	// the wall clock is still nine days away on the subscription's clock.
	clockNext := s.frozen.Add(7 * 24 * time.Hour)
	s.sub(bfRow{id: "sub_cp_clock", nextBilling: clockNext, clockID: s.clockID})
	// Pause through the real writer, so the rows carry every column a pause
	// writes. The canceled row is paused while live, then canceled: the shape
	// a cancel that leaves the pause behind produces.
	pause := func(id string, resumesAt *time.Time) {
		t.Helper()
		if _, err := s.store.SetPauseCollection(s.ctx, s.tenantID, id, domain.PauseCollection{
			Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: resumesAt,
		}, nil); err != nil {
			t.Fatalf("pause %s: %v", id, err)
		}
	}
	pause("sub_cp_paused", nil)
	pause("sub_cp_timed", &next)
	pause("sub_cp_trial", nil)
	pause("sub_cp_canceled", nil)
	s.exec(`UPDATE subscriptions SET status = 'canceled', canceled_at = now() WHERE id = 'sub_cp_canceled'`)
	// The service refuses a past resume date, so move it into the past after
	// pausing.
	pause("sub_cp_elapsed", &next)
	pause("sub_cp_clock", &clockNext)
	pastResume := s.now.Add(-time.Hour)
	s.exec(`UPDATE subscriptions SET pause_collection_resumes_at = $1 WHERE id IN ('sub_cp_elapsed', 'sub_cp_clock')`, pastResume)

	h := subscription.NewHandler(subscription.NewService(s.store, nil))
	list := func(query string) (int, []string, int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/"+query, nil)
		req = req.WithContext(auth.WithTenantID(s.ctx, s.tenantID))
		rr := httptest.NewRecorder()
		h.Routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			return rr.Code, nil, 0
		}
		var body struct {
			Data []struct {
				ID              string          `json:"id"`
				PauseCollection json.RawMessage `json:"pause_collection"`
			} `json:"data"`
			Total int `json:"total"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", query, err)
		}
		var got []string
		for _, d := range body.Data {
			got = append(got, d.ID)
		}
		slices.Sort(got)
		return rr.Code, got, body.Total
	}

	code, got, total := list("?collection=paused")
	want := []string{"sub_cp_clock", "sub_cp_paused", "sub_cp_timed", "sub_cp_trial"}
	if code != http.StatusOK || !slices.Equal(got, want) || total != len(want) {
		t.Fatalf("collection=paused: code %d, rows %v, total %d; want 200, %v, %d", code, got, total, want, len(want))
	}

	// It composes with the status filter.
	if code, got, total := list("?collection=paused&status=trialing"); code != http.StatusOK || !slices.Equal(got, []string{"sub_cp_trial"}) || total != 1 {
		t.Fatalf("collection=paused&status=trialing: code %d, rows %v, total %d; want 200, [sub_cp_trial], 1", code, got, total)
	}

	// Control: no filter returns all seven, the elapsed pause among them.
	if code, got, total := list(""); code != http.StatusOK || len(got) != 7 || total != 7 || !slices.Contains(got, "sub_cp_elapsed") {
		t.Fatalf("unfiltered: code %d, rows %v, total %d; want 200 and 7 with sub_cp_elapsed", code, got, total)
	}

	// "paused" is not a status; asking for it is a 400, not an empty list.
	if code, _, _ := list("?status=paused"); code != http.StatusBadRequest {
		t.Fatalf("status=paused: code %d, want 400", code)
	}
}
