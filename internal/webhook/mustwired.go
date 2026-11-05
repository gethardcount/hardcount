package webhook

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
)

// outboxEnqueuer is OutboxStore.Enqueue's method, the one each domain
// declares in its own consumer interface (invoice.OutboxEnqueuer and so on).
type outboxEnqueuer interface {
	Enqueue(ctx context.Context, tx *sql.Tx, tenantID, eventType string, payload map[string]any) (string, error)
}

// MustWiredOutbox panics unless every given component holds a webhook outbox.
// A store or service that queues webhook events in its transaction gets the
// outbox from a setter after construction, and queues nothing while it is
// nil, which keeps narrow tests simple. So a forgotten setter would boot and
// send none of that component's events. Call this from the composition root
// after the outbox wiring, with every component that queues events.
//
// A component passes when it has at least one field, exported or not, whose
// type is an interface with outboxEnqueuer's method, and every such field is
// non-nil. It is the shape of audit.MustWired.
func MustWiredOutbox(components ...any) {
	want := reflect.TypeFor[outboxEnqueuer]()
	for _, c := range components {
		v := reflect.ValueOf(c)
		if !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
			panic(fmt.Sprintf("webhook.MustWiredOutbox: component %T is nil", c))
		}
		for v.Kind() == reflect.Pointer {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			panic(fmt.Sprintf("webhook.MustWiredOutbox: component %T is not a struct", c))
		}
		t := v.Type()
		found := false
		for i := range t.NumField() {
			f := t.Field(i)
			if f.Type.Kind() != reflect.Interface || !f.Type.Implements(want) {
				continue
			}
			found = true
			// IsNil works on an unexported field without Interface().
			if v.Field(i).IsNil() {
				panic(fmt.Sprintf("webhook.MustWiredOutbox: %s.%s is nil: the outbox was not wired into %T, so it would queue none of its webhook events",
					t.Name(), f.Name, c))
			}
		}
		if !found {
			panic(fmt.Sprintf("webhook.MustWiredOutbox: %T has no outbox field: remove it from the call or add the field", c))
		}
	}
}
