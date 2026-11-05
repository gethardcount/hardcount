package webhook_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/webhook"
)

// enqueuer is a domain's consumer interface for the outbox, as
// invoice.OutboxEnqueuer is.
type enqueuer interface {
	Enqueue(ctx context.Context, tx *sql.Tx, tenantID, eventType string, payload map[string]any) (string, error)
}

// widerEnqueuer holds the outbox method among others; it still counts.
type widerEnqueuer interface {
	enqueuer
	Name() string
}

type namedOutbox struct{ *webhook.OutboxStore }

func (namedOutbox) Name() string { return "outbox" }

type wiredStore struct {
	outbox enqueuer
}

type twoOutboxes struct {
	events  enqueuer
	refunds widerEnqueuer
}

// noOutbox holds an interface, but not one with the outbox method.
type noOutbox struct {
	audit fmt.Stringer
}

func TestMustWiredOutbox(t *testing.T) {
	outbox := webhook.NewOutboxStore(nil)
	for _, tc := range []struct {
		name      string
		component any
		wantPanic string // "" means it must pass
	}{
		{"wired", &wiredStore{outbox: outbox}, ""},
		{"wired by value", wiredStore{outbox: outbox}, ""},
		{"every outbox field wired", &twoOutboxes{events: outbox, refunds: namedOutbox{outbox}}, ""},
		{"outbox not wired", &wiredStore{}, "wiredStore.outbox is nil"},
		{"one of two not wired", &twoOutboxes{events: outbox}, "twoOutboxes.refunds is nil"},
		{"no outbox field", &noOutbox{audit: &strings.Builder{}}, "has no outbox field"},
		{"nil component", (*wiredStore)(nil), "is nil"},
		{"not a struct", outbox.Enqueue, "is not a struct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := func() (msg string) {
				defer func() {
					if r := recover(); r != nil {
						msg = fmt.Sprint(r)
					}
				}()
				webhook.MustWiredOutbox(tc.component)
				return ""
			}()
			switch {
			case tc.wantPanic == "" && got != "":
				t.Fatalf("MustWiredOutbox(%T) panicked: %s", tc.component, got)
			case tc.wantPanic != "" && got == "":
				t.Fatalf("MustWiredOutbox(%T) passed; want a panic containing %q", tc.component, tc.wantPanic)
			case !strings.Contains(got, tc.wantPanic):
				t.Fatalf("MustWiredOutbox(%T) panic = %q; want it to contain %q", tc.component, got, tc.wantPanic)
			}
		})
	}
}
