package respond

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParsePage(t *testing.T) {
	cases := []struct {
		query     string
		want      Page
		wantParam string // "" means the page is accepted
	}{
		{"", Page{Limit: 50, Offset: 0}, ""},
		{"limit=1", Page{Limit: 1}, ""},
		{"limit=100&offset=250", Page{Limit: 100, Offset: 250}, ""},
		{"offset=0", Page{Limit: 50}, ""},
		{"limit=0", Page{}, "limit"},
		{"limit=101", Page{}, "limit"},
		{"limit=-5", Page{}, "limit"},
		{"limit=abc", Page{}, "limit"},
		{"limit=", Page{}, "limit"},
		{"limit=2.5", Page{}, "limit"},
		{"limit=10&limit=20", Page{}, "limit"},
		{"offset=-1", Page{}, "offset"},
		{"offset=x", Page{}, "offset"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		got, ok := ParsePage(rec, httptest.NewRequest(http.MethodGet, "/?"+c.query, nil))
		if c.wantParam == "" {
			if !ok || got != c.want {
				t.Errorf("?%s = %+v, %v; want %+v accepted", c.query, got, ok, c.want)
			}
			continue
		}
		var body ErrorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if ok || rec.Code != http.StatusUnprocessableEntity || body.Error.Param != c.wantParam || body.Error.Code != "validation_error" {
			t.Errorf("?%s = %d %s, want 422 validation_error naming %s", c.query, rec.Code, rec.Body.String(), c.wantParam)
		}
	}
}

func TestWindow(t *testing.T) {
	all := []int{1, 2, 3, 4, 5}
	for _, c := range []struct {
		p    Page
		want []int
	}{
		{Page{Limit: 2, Offset: 0}, []int{1, 2}},
		{Page{Limit: 2, Offset: 4}, []int{5}},
		{Page{Limit: 50, Offset: 0}, []int{1, 2, 3, 4, 5}},
		{Page{Limit: 2, Offset: 5}, []int{}},
		{Page{Limit: 2, Offset: 99}, []int{}},
	} {
		got := Window(all, c.p)
		if len(got) != len(c.want) || got == nil {
			t.Errorf("Window(%+v) = %v, want %v (never nil)", c.p, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("Window(%+v) = %v, want %v", c.p, got, c.want)
			}
		}
	}
}

// A nil slice is written as [], so data is always an array.
func TestList_NilIsEmptyArray(t *testing.T) {
	rec := httptest.NewRecorder()
	var none []string
	List(rec, httptest.NewRequest(http.MethodGet, "/", nil), none, 0)
	if got := rec.Body.String(); got != "{\"data\":[],\"total\":0}\n" {
		t.Errorf("body = %q, want data [] and total 0", got)
	}
}
