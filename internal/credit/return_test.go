package credit

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

func blocks(pairs ...any) []drainedBlock {
	out := []drainedBlock{}
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, drainedBlock{BlockID: pairs[i].(string), TakeCents: int64(pairs[i+1].(int))})
	}
	return out
}

// planRestore places a return last drawn first: the newest usage row first,
// and within a row its blocks in reverse drain order. Earlier returns took
// the front of that same walk, so they are skipped block by block.
//
// Mutation-verify: walk a row's blocks forwards instead of backwards → the
// "one row" and "partial" cases go red; drop the skip of earlier restores →
// "after an earlier return" and "same block on two rows" go red.
func TestPlanRestore_Order(t *testing.T) {
	cases := []struct {
		name     string
		draws    []usageDraw
		restored map[string]int64
		amount   int64
		want     restorePlan
	}{
		{
			name:   "one row, whole return: reverse drain order",
			draws:  []usageDraw{{EntryID: "u1", AmountCents: 100, Blocks: blocks("A", 30, "B", 70)}},
			amount: 100,
			want:   restorePlan{Legs: blocks("B", 70, "A", 30), RestoredCents: 100, OutstandingCents: 100},
		},
		{
			name:   "partial: the last-drawn block first",
			draws:  []usageDraw{{EntryID: "u1", AmountCents: 100, Blocks: blocks("A", 30, "B", 70)}},
			amount: 50,
			want:   restorePlan{Legs: blocks("B", 50), RestoredCents: 50, OutstandingCents: 100},
		},
		{
			name: "two rows: the newer row first",
			draws: []usageDraw{
				{EntryID: "u2", AmountCents: 20, Blocks: blocks("C", 20)},
				{EntryID: "u1", AmountCents: 100, Blocks: blocks("A", 30, "B", 70)},
			},
			amount: 40,
			want:   restorePlan{Legs: blocks("C", 20, "B", 20), RestoredCents: 40, OutstandingCents: 120},
		},
		{
			name: "after an earlier return: what it restored is skipped",
			draws: []usageDraw{
				{EntryID: "u2", AmountCents: 20, Blocks: blocks("C", 20)},
				{EntryID: "u1", AmountCents: 100, Blocks: blocks("A", 30, "B", 70)},
			},
			restored: map[string]int64{"C": 20, "B": 10},
			amount:   50,
			want:     restorePlan{Legs: blocks("B", 50), RestoredCents: 50, OutstandingCents: 90},
		},
		{
			name: "same block on two rows: the skip runs over both",
			draws: []usageDraw{
				{EntryID: "u2", AmountCents: 5, Blocks: blocks("A", 5)},
				{EntryID: "u1", AmountCents: 10, Blocks: blocks("A", 10)},
			},
			restored: map[string]int64{"A": 7},
			amount:   8,
			want:     restorePlan{Legs: blocks("A", 8), RestoredCents: 8, OutstandingCents: 8},
		},
		{
			name: "a row with no record is legacy, and recorded blocks come first",
			draws: []usageDraw{
				{EntryID: "u2", AmountCents: 40},
				{EntryID: "u1", AmountCents: 60, Blocks: blocks("A", 60)},
			},
			amount: 100,
			want:   restorePlan{Legs: blocks("A", 60), RestoredCents: 60, OutstandingCents: 60, LegacyCents: 40},
		},
		{
			name:   "more to return than is outstanding: the rest is unplaced",
			draws:  []usageDraw{{EntryID: "u1", AmountCents: 30, Blocks: blocks("A", 30)}},
			amount: 100,
			want:   restorePlan{Legs: blocks("A", 30), RestoredCents: 30, OutstandingCents: 30},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planRestore(tc.draws, tc.restored, tc.amount)
			if err != nil {
				t.Fatalf("planRestore: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("plan = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// planRestore refuses an attribution that does not add up rather than place
// credit on a guess.
func TestPlanRestore_RefusesCorruptAttribution(t *testing.T) {
	cases := []struct {
		name     string
		draws    []usageDraw
		restored map[string]int64
		wantMsg  string
	}{
		{"blocks do not add up to the row", []usageDraw{{EntryID: "u1", AmountCents: 100, Blocks: blocks("A", 30)}}, nil, "add up to"},
		{"a block with no take", []usageDraw{{EntryID: "u1", AmountCents: 0, Blocks: blocks("A", 0)}}, nil, "with take"},
		{"an empty record", []usageDraw{{EntryID: "u1", AmountCents: 10, Blocks: []drainedBlock{}}}, nil, "add up to"},
		{"earlier returns restored more than was drawn", []usageDraw{{EntryID: "u1", AmountCents: 10, Blocks: blocks("A", 10)}}, map[string]int64{"A": 11}, "more to block A"},
		{"earlier returns restored to a block never drawn", []usageDraw{{EntryID: "u1", AmountCents: 10, Blocks: blocks("A", 10)}}, map[string]int64{"Z": 1}, "more to block Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planRestore(tc.draws, tc.restored, 5)
			if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantMsg)
			}
		})
	}
}

// classifyRestore applies the expiry sweep's rule: expired when
// expires_at < at (the boundary instant is live), not while an owed draw holds
// the grant, and retirement wins once the sweep wrote the grant off. A voided
// commit funding invoice comes first.
//
// Mutation-verify: `!b.ExpiresAt.After(at)` (boundary expired) → "at the
// boundary" red; drop `b.Swept ||` → "swept and held" red; drop the Held
// test → "held, not swept" red; drop the funding check → both "funding
// voided" cases red.
func TestClassifyRestore_FollowsTheSweepRule(t *testing.T) {
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	past, boundary, future := at.Add(-time.Hour), at, at.Add(time.Hour)
	grant := func(exp *time.Time, held, swept bool) blockState {
		return blockState{EntryType: domain.CreditGrant, ExpiresAt: exp, Held: held, Swept: swept}
	}
	cases := []struct {
		name string
		b    blockState
		want restoreAction
	}{
		{"no expiry", grant(nil, false, false), restoreLive},
		{"not yet expired", grant(&future, false, false), restoreLive},
		{"at the boundary: still live", grant(&boundary, false, false), restoreLive},
		{"past expiry", grant(&past, false, false), restoreThenExpire},
		{"past expiry, held, not swept: the owed draw may use it", grant(&past, true, false), restoreLive},
		{"past expiry, swept and held: retirement wins", grant(&past, true, true), restoreThenExpire},
		{"a positive adjustment never expires", blockState{EntryType: domain.CreditAdjustment, ExpiresAt: &past}, restoreLive},
		{"funding voided", blockState{EntryType: domain.CreditGrant, GrantKind: "commit", FundingVoided: true}, restoreThenRetire},
		{"funding voided and expired: retired", blockState{EntryType: domain.CreditGrant, GrantKind: "commit", FundingVoided: true, ExpiresAt: &past}, restoreThenRetire},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRestore(tc.b, at); got != tc.want {
				t.Errorf("classifyRestore = %d, want %d", got, tc.want)
			}
		})
	}
}

// The void reversal asks the store to return what is still applied (drawn
// minus what credit notes already returned), for its own source, with every
// cent drawn credit.
func TestReverseForInvoiceTx_ReturnsWhatIsStillApplied(t *testing.T) {
	store := newMemStore()
	svc := NewService(store)
	ctx := context.Background()
	for _, e := range []domain.CreditLedgerEntry{
		{CustomerID: "cus_1", EntryType: domain.CreditUsage, Currency: "EUR", AmountCents: -10000, InvoiceID: "inv_1"},
		{CustomerID: "cus_1", EntryType: domain.CreditGrant, Currency: "EUR", AmountCents: 3000, InvoiceID: "inv_1", SourceCreditNoteID: "cn_1"},
	} {
		if _, err := store.AppendEntry(ctx, "t1", e); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	res, err := svc.ReverseForInvoiceTx(ctx, nil, "t1", "cus_1", "inv_1", "INV-1")
	if err != nil {
		t.Fatalf("ReverseForInvoiceTx: %v", err)
	}
	if res.ReturnedCents != 7000 {
		t.Errorf("returned %d, want 7000", res.ReturnedCents)
	}
	if len(store.returns) != 1 {
		t.Fatalf("returns = %d, want 1", len(store.returns))
	}
	got := store.returns[0]
	if got.AmountCents != 7000 || got.Currency != "EUR" || got.SourceInvoiceReversalID != "inv_1" || got.SourceCreditNoteID != "" ||
		got.PaidInvoice || got.InvoiceID != "inv_1" || got.CustomerID != "cus_1" || got.At.IsZero() {
		t.Errorf("return entry = %+v", got)
	}
	if got.Description != "Reversed — invoice INV-1 voided" {
		t.Errorf("description = %q", got.Description)
	}
}

// A return judges expiry at the customer's instant: on a test clock that is
// the clock's time on ctx, not the wall clock. The void and the credit-note
// grant both pass it to the store as At.
//
// Mutation-verify: pass time.Now() as At in either method → red.
func TestReturns_UseTheCustomersInstant(t *testing.T) {
	frozen := time.Date(2027, 1, 15, 9, 0, 0, 0, time.UTC)
	ctx := clock.WithSim(context.Background(), clock.Sim{At: frozen, TestClockID: "hc_tclk_1"})
	store := newMemStore()
	svc := NewService(store)
	if _, err := store.AppendEntry(ctx, "t1", domain.CreditLedgerEntry{
		CustomerID: "cus_1", EntryType: domain.CreditUsage, Currency: "USD", AmountCents: -500, InvoiceID: "inv_1",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := svc.ReverseForInvoiceTx(ctx, nil, "t1", "cus_1", "inv_1", ""); err != nil {
		t.Fatalf("ReverseForInvoiceTx: %v", err)
	}
	if _, err := svc.GrantForCreditNoteTx(ctx, nil, "t1", "cn_1", CreditNoteReturnInput{
		CustomerID: "cus_1", Currency: "USD", AmountCents: 100, Description: "note", InvoiceID: "inv_2",
	}); err != nil {
		t.Fatalf("GrantForCreditNoteTx: %v", err)
	}
	if len(store.returns) != 2 {
		t.Fatalf("returns = %d, want 2", len(store.returns))
	}
	for i, r := range store.returns {
		if !r.At.Equal(frozen) {
			t.Errorf("return %d At = %v, want the clock's %v", i, r.At, frozen)
		}
	}
}

// The credit-note grant is a return for the note, carrying whether the
// invoice was paid, after GrantTx's own checks.
func TestGrantForCreditNoteTx_IsAReturnForTheNote(t *testing.T) {
	ctx := context.Background()
	valid := CreditNoteReturnInput{CustomerID: "cus_1", Currency: "usd", AmountCents: 500, Description: " Credit note CN-1 ", InvoiceID: "inv_1", InvoicePaid: true}

	store := newMemStore()
	svc := NewService(store)
	if _, err := svc.GrantForCreditNoteTx(ctx, nil, "t1", "cn_1", valid); err != nil {
		t.Fatalf("GrantForCreditNoteTx: %v", err)
	}
	if len(store.returns) != 1 {
		t.Fatalf("returns = %d, want 1", len(store.returns))
	}
	got := store.returns[0]
	if got.SourceCreditNoteID != "cn_1" || got.SourceInvoiceReversalID != "" || !got.PaidInvoice ||
		got.Currency != "USD" || got.Description != "Credit note CN-1" || got.AmountCents != 500 || got.At.IsZero() {
		t.Errorf("return entry = %+v", got)
	}

	refused := []struct {
		name  string
		cnID  string
		edit  func(*CreditNoteReturnInput)
		field string
	}{
		{"no credit note", "", func(*CreditNoteReturnInput) {}, "credit_note_id"},
		{"no customer", "cn_2", func(i *CreditNoteReturnInput) { i.CustomerID = "" }, "customer_id"},
		{"no invoice", "cn_2", func(i *CreditNoteReturnInput) { i.InvoiceID = "" }, "invoice_id"},
		{"no currency", "cn_2", func(i *CreditNoteReturnInput) { i.Currency = "" }, "currency"},
		{"zero amount", "cn_2", func(i *CreditNoteReturnInput) { i.AmountCents = 0 }, "amount_cents"},
		{"over the cap", "cn_2", func(i *CreditNoteReturnInput) { i.AmountCents = 100_000_001 }, "amount_cents"},
		{"no description", "cn_2", func(i *CreditNoteReturnInput) { i.Description = "  " }, "description"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			in := valid
			tc.edit(&in)
			store := newMemStore()
			_, err := NewService(store).GrantForCreditNoteTx(ctx, nil, "t1", tc.cnID, in)
			if errs.Field(err) != tc.field {
				t.Errorf("err = %v (field %q), want field %q", err, errs.Field(err), tc.field)
			}
			if len(store.returns) != 0 {
				t.Error("a refused input reached the store")
			}
		})
	}
}
