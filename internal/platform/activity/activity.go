// Package activity holds the shape of an activity line and the words that
// more than one activity surface shares (ADR-128).
//
// An activity line is one row on an activity card: the invoice and
// subscription Activity cards, the Dunning page's recovery timeline and the
// customer's Sent emails card. The backend builds every word of a line; the
// dashboard formats money and times, turns a link into a route, maps the tone
// to a colour and lays the row out.
//
// Each domain keeps its own describe functions and returns a Line. This
// package holds what they share: the Line type, the tones, the actor mapping,
// the email names and outcome words, the credit-note reason labels and refund
// parts, and the decline-code words.
//
// A describe function takes no clock input. Times are attached afterwards by
// Stamp, so a test-clock line can differ from a real-clock line only by its
// Recorded time.
package activity

import (
	"strings"
	"time"
)

// Tone is the meaning a line's dot repeats. The words carry the meaning; the
// dot only repeats it.
type Tone string

const (
	// ToneNeutral: something happened.
	ToneNeutral Tone = "neutral"
	// ToneSuccess: money arrived, or was returned as intended.
	ToneSuccess Tone = "success"
	// ToneWarning: needs a look, or money will not be collected.
	ToneWarning Tone = "warning"
	// ToneFailure: an attempt failed.
	ToneFailure Tone = "failure"
)

// Money is an amount in a currency's minor units.
type Money struct {
	Cents    int64  `json:"cents"`
	Currency string `json:"currency"`
}

// LinkKind names the kind of object a link part points at.
type LinkKind string

const (
	LinkInvoice      LinkKind = "invoice"
	LinkCreditNote   LinkKind = "credit_note"
	LinkSubscription LinkKind = "subscription"
)

// Link points at an object the row's own data names, and that still exists.
type Link struct {
	Kind  LinkKind `json:"kind"`
	ID    string   `json:"id"`
	Label string   `json:"label"`
}

// Part is one piece of a meta segment. Exactly one field is set.
type Part struct {
	Text  string     `json:"text,omitempty"`
	Money *Money     `json:"money,omitempty"`
	Time  *time.Time `json:"time,omitempty"`
	Link  *Link      `json:"link,omitempty"`
}

// Segment is one item of the meta line: its parts read as one phrase
// ("$15.00 refunded to card"), and segments are joined by " · ".
type Segment []Part

// ActorKind is who did what a line tells, from a fixed set.
type ActorKind string

const (
	ActorPerson    ActorKind = "person"
	ActorAPIKey    ActorKind = "api_key"
	ActorCustomer  ActorKind = "customer"
	ActorAutomatic ActorKind = "automatic"
)

// Via says where the customer acted.
type Via string

const (
	ViaPaymentPage Via = "payment_page"
	ViaPortal      Via = "portal"
)

// Actor is who did it. It comes from a fact written with the row; a row with
// no such fact has no actor.
type Actor struct {
	Kind ActorKind `json:"kind"`
	Name string    `json:"name,omitempty"`
	Via  Via       `json:"via,omitempty"`
}

// Line is one activity row.
type Line struct {
	// ID is the row's stable key.
	ID string `json:"id"`
	// Kind is the machine name of the event, for example "invoice.voided".
	Kind string `json:"kind"`
	// Title is plain words, object plus past participle, with no values.
	Title string `json:"title"`
	// Amount is set only when the event moves money, asks for it, or cancels
	// money owed.
	Amount *Money `json:"amount,omitempty"`
	// Problem says what went wrong or what it means, on failure and warning
	// rows whose title does not say it all.
	Problem string `json:"problem,omitempty"`
	// Detail is the meta line before the actor.
	Detail []Segment `json:"detail"`
	// Actor is the meta line's last part. Nil when no fact names one.
	Actor *Actor `json:"actor,omitempty"`
	Tone  Tone   `json:"tone"`
	// OccurredAt is the event's instant on the entity's calendar.
	OccurredAt time.Time `json:"occurred_at"`
	// RecordedAt is the real-world instant, set only for an entity on a test
	// clock that has one.
	RecordedAt *time.Time `json:"recorded_at,omitempty"`
}

// Text is a one-part segment of plain words.
func Text(s string) Segment { return Segment{{Text: s}} }

// T is a text part.
func T(s string) Part { return Part{Text: s} }

// M is a money part.
func M(cents int64, currency string) Part {
	return Part{Money: &Money{Cents: cents, Currency: currency}}
}

// At is a time part. The dashboard formats it in the account's timezone.
func At(t time.Time) Part {
	u := t.UTC()
	return Part{Time: &u}
}

// L is a link part.
func L(kind LinkKind, id, label string) Part {
	return Part{Link: &Link{Kind: kind, ID: id, Label: label}}
}

// Amount returns the amount a title shows, or nil when there is none to show.
// A zero amount moves nothing, so it is not shown.
func Amount(cents int64, currency string) *Money {
	if cents == 0 {
		return nil
	}
	return &Money{Cents: cents, Currency: currency}
}

// Stamp attaches a line's times. occurred is the instant on the entity's
// calendar. recorded is the real-world instant; it is kept only when the
// entity is on a test clock (pinned) and the row has one.
func Stamp(l Line, occurred, recorded time.Time, pinned bool) Line {
	l.OccurredAt = occurred.UTC()
	l.RecordedAt = nil
	if pinned && !recorded.IsZero() {
		r := recorded.UTC()
		l.RecordedAt = &r
	}
	if l.Detail == nil {
		l.Detail = []Segment{}
	}
	return l
}

// Automatic is the actor for anything Hardcount did on its own.
func Automatic() *Actor { return &Actor{Kind: ActorAutomatic} }

// ActorFromAudit maps an audit row's actor (audit.ResolveActor's kinds:
// user, api_key, customer, system) to a line's actor. name is the resolved
// actor name the audit list carries: the user's email, the key's name or the
// customer's display name. An unknown kind has no actor.
func ActorFromAudit(actorType, name string) *Actor {
	switch actorType {
	case "user":
		return &Actor{Kind: ActorPerson, Name: name}
	case "api_key":
		return &Actor{Kind: ActorAPIKey, Name: name}
	case "customer":
		return &Actor{Kind: ActorCustomer, Name: name}
	case "system":
		return Automatic()
	}
	return nil
}

// ActorText is the actor as the meta line words it.
func ActorText(a *Actor) string {
	if a == nil {
		return ""
	}
	switch a.Kind {
	case ActorPerson:
		if a.Name == "" {
			return ""
		}
		return "by " + a.Name
	case ActorAPIKey:
		if a.Name == "" {
			return "by API key"
		}
		return "by API key " + a.Name
	case ActorCustomer:
		switch a.Via {
		case ViaPaymentPage:
			return "Customer, on the payment page"
		case ViaPortal:
			return "Customer, in the portal"
		}
		return "Customer"
	case ActorAutomatic:
		return "Automatic"
	}
	return ""
}

// MetaText renders the meta line as plain text, without the Recorded part:
// the detail segments, then the actor, joined by " · ". It is what the
// invoice timeline's legacy `detail` field carries until the dashboard
// renders lines itself.
func MetaText(l Line) string {
	var segs []string
	for _, s := range l.Detail {
		if txt := SegmentText(s); txt != "" {
			segs = append(segs, txt)
		}
	}
	if a := ActorText(l.Actor); a != "" {
		segs = append(segs, a)
	}
	return strings.Join(segs, " · ")
}

// SegmentText renders one segment as plain text. Money uses FormatMoney, a
// time is printed in UTC with its zone, and a link prints its label.
func SegmentText(s Segment) string {
	var b strings.Builder
	for _, p := range s {
		switch {
		case p.Money != nil:
			b.WriteString(FormatMoney(*p.Money))
		case p.Time != nil:
			b.WriteString(p.Time.UTC().Format("Jan 2, 2006 15:04 MST")) //tz:ok — plain-text fallback for an instant, in UTC with its zone
		case p.Link != nil:
			b.WriteString(p.Link.Label)
		default:
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
