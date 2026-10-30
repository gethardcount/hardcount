package credit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// A credit return hands back credit an invoice drew from the customer's
// balance (ADR-132): a void (ReverseForInvoiceTx), or a credit note on the
// invoice (GrantForCreditNoteTx). The part the invoice drew from credit
// blocks goes back to those blocks, so it keeps their expiry and their kind.
// The whole amount is still one grant row on the ledger, with the source id
// the dedup indexes key on; that row is written with consumed_cents = the
// restored part, so it adds the amount to the balance but only the
// unattributed rest to drainable capacity.

// ReturnEntry is one hand-back of an invoice's drawn credit.
type ReturnEntry struct {
	CustomerID  string
	InvoiceID   string
	Currency    string
	AmountCents int64
	Description string
	// Exactly one source: the voided invoice, or the credit note.
	SourceInvoiceReversalID string
	SourceCreditNoteID      string
	// PaidInvoice is set when the amount is a credit note's credit channel on
	// a paid invoice. Then the customer's balance rises by exactly the amount:
	// a credit note never gives less than it says. The note is split pro rata
	// to how the invoice was paid (paidDrawnShare); the drawn share goes back
	// to the blocks it came from where it stays spendable, and everything
	// else, including a share whose block is past expiry or is a commit whose
	// funding invoice is voided, is new credit with no expiry. When unset (a
	// void, or a credit note on an unpaid invoice) every cent of the amount
	// is drawn credit.
	PaidInvoice bool
	// InvoiceTotalCents is the paid invoice's total, the base of the pro-rata
	// split. Required when PaidInvoice is set.
	InvoiceTotalCents int64
	// At is the instant of the return on the customer's clock. It stamps the
	// row and decides which source grants are already past their expiry.
	// Zero means clock.Now(ctx).
	At time.Time
}

// usageDraw is one usage row of the invoice: what it drew, and from which
// blocks in drain order. Blocks is nil for a row written before drains
// recorded their blocks (2026-07-05).
type usageDraw struct {
	EntryID     string
	AmountCents int64
	Blocks      []drainedBlock
}

// restorePlan is how much of a return goes back to which block.
type restorePlan struct {
	// Legs is one entry per block, in restore order, with what goes back
	// to it.
	Legs []drainedBlock
	// RestoredCents is the sum of Legs: min(amount, OutstandingCents).
	RestoredCents int64
	// OutstandingCents is what the invoice drew from blocks and no earlier
	// return has restored yet.
	OutstandingCents int64
	// LegacyCents is what the invoice's unattributed usage rows drew.
	LegacyCents int64
}

// planRestore decides which blocks a return of `amount` goes back to.
//
// Last-drawn first: the invoice's usage rows newest first (draws sorted by
// id descending), and within a row its blocks in reverse drain order. Every
// earlier return walked the same order, so what they restored (`restored`,
// per block) is the front of this walk: it is skipped block by block before
// anything is taken. The walk runs to the end even after `amount` is placed,
// to add up what is still outstanding and to check the attribution.
//
// It refuses a usage row whose blocks do not add up to what the row drew, and
// an earlier return that restored more to a block than the invoice drew from
// it: either means the ledger is corrupt, and placing credit on a guess would
// hide it.
func planRestore(draws []usageDraw, restored map[string]int64, amount int64) (restorePlan, error) {
	var plan restorePlan
	skip := make(map[string]int64, len(restored))
	for id, c := range restored {
		if c < 0 {
			return restorePlan{}, fmt.Errorf("earlier returns restored %d to block %s", c, id)
		}
		skip[id] = c
	}
	left := amount
	perBlock := map[string]int64{}
	var order []string
	for _, d := range draws {
		if d.Blocks == nil {
			var err error
			if plan.LegacyCents, err = domain.SumCents(plan.LegacyCents, d.AmountCents); err != nil {
				return restorePlan{}, fmt.Errorf("usage row %s: %w", d.EntryID, err)
			}
			continue
		}
		var sum int64
		for _, b := range d.Blocks {
			if b.BlockID == "" || b.TakeCents <= 0 {
				return restorePlan{}, fmt.Errorf("usage row %s names a block %q with take %d", d.EntryID, b.BlockID, b.TakeCents)
			}
			var err error
			if sum, err = domain.SumCents(sum, b.TakeCents); err != nil {
				return restorePlan{}, fmt.Errorf("usage row %s: %w", d.EntryID, err)
			}
		}
		if sum != d.AmountCents {
			return restorePlan{}, fmt.Errorf("usage row %s drew %d but its blocks add up to %d", d.EntryID, d.AmountCents, sum)
		}
		for i := len(d.Blocks) - 1; i >= 0; i-- {
			b := d.Blocks[i]
			avail := b.TakeCents
			s := min(skip[b.BlockID], avail)
			skip[b.BlockID] -= s
			avail -= s
			var err error
			if plan.OutstandingCents, err = domain.SumCents(plan.OutstandingCents, avail); err != nil {
				return restorePlan{}, fmt.Errorf("usage row %s: %w", d.EntryID, err)
			}
			if left <= 0 || avail <= 0 {
				continue
			}
			take := min(avail, left)
			if _, seen := perBlock[b.BlockID]; !seen {
				order = append(order, b.BlockID)
			}
			perBlock[b.BlockID] += take
			left -= take
		}
	}
	for id, c := range skip {
		if c > 0 {
			return restorePlan{}, fmt.Errorf("earlier returns restored %d more to block %s than the invoice drew from it", c, id)
		}
	}
	for _, id := range order {
		plan.Legs = append(plan.Legs, drainedBlock{BlockID: id, TakeCents: perBlock[id]})
	}
	plan.RestoredCents = amount - left
	return plan, nil
}

// paidDrawnShare is the part of a credit note on a paid invoice that stands
// for credit the invoice drew, rather than cash the customer paid: the note
// split pro rata to how the invoice was paid, drawn / total of it.
//
// It is cumulative over the invoice's earlier paid-branch notes (`earlier`
// is the sum of their amounts): this note's share is
// floor((earlier+amount)×drawn/total) − floor(earlier×drawn/total). The
// rounding therefore telescopes, and the notes' shares together never pass
// the drawn credit's share of what was credited. The result is within
// [0, amount].
func paidDrawnShare(earlier, amount, drawn, total int64) (int64, error) {
	switch {
	case total <= 0:
		return 0, fmt.Errorf("invoice total %d is not positive", total)
	case earlier < 0 || amount <= 0 || drawn < 0:
		return 0, fmt.Errorf("paid share inputs out of range (earlier %d, amount %d, drawn %d)", earlier, amount, drawn)
	}
	f := func(x int64) *big.Int {
		v := new(big.Int).Mul(big.NewInt(x), big.NewInt(drawn))
		return v.Quo(v, big.NewInt(total)) // all non-negative: Quo is floor
	}
	share := new(big.Int).Sub(f(earlier+amount), f(earlier))
	if share.Sign() < 0 {
		return 0, nil
	}
	if share.Cmp(big.NewInt(amount)) > 0 {
		return amount, nil // drawn > total: a corrupt invoice, never more than the note
	}
	return share.Int64(), nil
}

// blockState is a source block as the return reads it under the locks.
type blockState struct {
	ID              string
	EntryType       domain.CreditEntryType
	Currency        string
	AmountCents     int64
	ConsumedCents   int64
	CNRetiredCents  int64
	ExpiresAt       *time.Time
	GrantKind       string
	SourceInvoiceID string
	// Held: a draw that can still use this grant is owed, so the expiry
	// sweep does not retire it (domain.CreditDrawOwedSQL, ADR-071 §2).
	Held bool
	// Swept: the sweep already wrote this grant's expiry entry.
	Swept bool
	// FundingVoided: a commit grant whose funding invoice is voided.
	FundingVoided bool
}

// restoreAction is what happens to a share once it is back on its block.
type restoreAction int

const (
	// restoreLive: the share is spendable again on its block.
	restoreLive restoreAction = iota
	// restoreThenExpire: the block is a grant past its expiry, so the share
	// expires at once, as the sweep would have expired it.
	restoreThenExpire
	// restoreThenRetire: the block is a commit grant whose funding invoice is
	// voided, so the share is retired again, as the void retired the rest.
	restoreThenRetire
)

// classifyRestore applies the expiry sweep's rule to a restored share, at the
// return's instant on the customer's clock. A grant is expired when
// expires_at < at (the drain's liveness rule is its complement). The sweep
// skips a grant an owed draw still needs (Held), and once it has retired a
// grant, retirement wins (ADR-071 §2): a swept grant never gets live credit
// back, held or not. A commit whose funding invoice is voided has no money
// behind it, so that comes first.
func classifyRestore(b blockState, at time.Time) restoreAction {
	if b.FundingVoided {
		return restoreThenRetire
	}
	if b.EntryType == domain.CreditGrant && b.ExpiresAt != nil && b.ExpiresAt.Before(at) && (b.Swept || !b.Held) {
		return restoreThenExpire
	}
	return restoreLive
}

// spendableAt reports whether a share restored to b would be spendable at
// `at`: not past its expiry (expires_at >= at, the drain's rule) and not a
// commit whose funding invoice is voided. On a paid invoice only such shares
// go back to their block (ADR-132): a credit note never gives less than it
// says, so a share that would expire or be retired is new credit instead.
func spendableAt(b blockState, at time.Time) bool {
	if b.FundingVoided {
		return false
	}
	return b.ExpiresAt == nil || !b.ExpiresAt.Before(at)
}

// ReturnCreditTx writes one credit return on the caller's tx (ADR-132).
//
// Locks, in the house order: the caller already holds the invoice row (the
// void, the credit note's issue); this takes the customer advisory lock, then
// the customer's ledger rows FOR UPDATE, before it reads anything. Every
// write to a source block is a CAS on the consumed_cents read under those
// locks, so a change the locks failed to keep out aborts the return instead
// of restoring on a stale read.
//
// Order of writes: for a grant past expiry with a remainder the sweep has not
// written off yet, that remainder's expiry entry (the sweep's own write,
// stamped at expires_at); the restores; the return row; then the at-once
// expiry entries (stamped at the return's instant) and commit re-retires.
// The balance crossings (ADR-078) are emitted once, for the net change.
func (s *PostgresStore) ReturnCreditTx(ctx context.Context, tx *sql.Tx, tenantID string, in ReturnEntry) (domain.CreditLedgerEntry, domain.CreditReturn, error) {
	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	switch {
	case currency == "":
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("credit return for invoice %s has no currency", in.InvoiceID)
	case in.CustomerID == "" || in.InvoiceID == "":
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("credit return needs a customer and an invoice (customer %q, invoice %q)", in.CustomerID, in.InvoiceID)
	case in.AmountCents <= 0:
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("credit return for invoice %s: amount %d is not positive", in.InvoiceID, in.AmountCents)
	case (in.SourceInvoiceReversalID == "") == (in.SourceCreditNoteID == ""):
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("credit return for invoice %s needs exactly one source", in.InvoiceID)
	case in.PaidInvoice && (in.SourceCreditNoteID == "" || in.InvoiceTotalCents <= 0):
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("credit return on paid invoice %s needs a credit note and the invoice total (%d)", in.InvoiceID, in.InvoiceTotalCents)
	}
	at := in.At
	if at.IsZero() {
		at = clock.Now(ctx)
	}

	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		tenantID+":"+in.CustomerID,
	); err != nil {
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("acquire credit ledger lock: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT 1 FROM customer_credit_ledger WHERE tenant_id = $1 AND customer_id = $2 FOR UPDATE`,
		tenantID, in.CustomerID,
	); err != nil {
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("lock credit ledger: %w", err)
	}

	row := domain.CreditLedgerEntry{
		CustomerID:              in.CustomerID,
		EntryType:               domain.CreditGrant,
		Currency:                currency,
		AmountCents:             in.AmountCents,
		Description:             in.Description,
		InvoiceID:               in.InvoiceID,
		SourceInvoiceReversalID: in.SourceInvoiceReversalID,
		SourceCreditNoteID:      in.SourceCreditNoteID,
		CreatedAt:               at,
	}
	// One return per source (the 0093 and 0106 indexes). It is checked
	// before the restores: a second return would otherwise read the first
	// one's restores as earlier returns and fail on the arithmetic, not on
	// the source.
	if err := refuseTakenSource(ctx, tx, tenantID, row); err != nil {
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, err
	}

	before, err := currencyBalanceTx(ctx, tx, tenantID, in.CustomerID, currency)
	if err != nil {
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, err
	}
	draws, err := invoiceDrawsTx(ctx, tx, tenantID, in.CustomerID, in.InvoiceID, currency)
	if err != nil {
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, err
	}
	restored, err := invoiceRestoredTx(ctx, tx, tenantID, in.CustomerID, in.InvoiceID)
	if err != nil {
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, err
	}
	// What goes back to blocks: on a void or an unpaid note, all of it; on a
	// paid invoice, the note's drawn share (paidDrawnShare).
	toPlace := in.AmountCents
	var drawnShare int64
	if in.PaidInvoice {
		earlier, err := paidReturnedTx(ctx, tx, tenantID, in.CustomerID, in.InvoiceID)
		if err != nil {
			return domain.CreditLedgerEntry{}, domain.CreditReturn{}, err
		}
		if drawnShare, err = paidDrawnShare(earlier, in.AmountCents, attributedCents(draws), in.InvoiceTotalCents); err != nil {
			return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("credit return for invoice %s: %w", in.InvoiceID, err)
		}
		toPlace = drawnShare
	}
	plan, err := planRestore(draws, restored, toPlace)
	if err != nil {
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("credit return for invoice %s: %w", in.InvoiceID, err)
	}
	if !in.PaidInvoice && in.AmountCents-plan.RestoredCents > plan.LegacyCents {
		// Every cent of this return is credit the invoice drew. More of it
		// than the unattributed rows drew has no block to go back to.
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf(
			"credit return for invoice %s: %d to return, %d outstanding on recorded blocks, %d drawn by rows without a record; refusing to mint the difference",
			in.InvoiceID, in.AmountCents, plan.OutstandingCents, plan.LegacyCents)
	}

	ids := make([]string, len(plan.Legs))
	for i, l := range plan.Legs {
		ids[i] = l.BlockID
	}
	states, err := blockStatesTx(ctx, tx, tenantID, in.CustomerID, in.InvoiceID, ids)
	if err != nil {
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, err
	}
	for _, leg := range plan.Legs {
		b, ok := states[leg.BlockID]
		switch {
		case !ok:
			return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("credit return for invoice %s: block %s the invoice drew from is missing", in.InvoiceID, leg.BlockID)
		case b.Currency != currency:
			return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("credit return for invoice %s: block %s is in %s, the return in %s", in.InvoiceID, b.ID, b.Currency, currency)
		case b.AmountCents <= 0 || b.ConsumedCents < leg.TakeCents:
			return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("credit return for invoice %s: block %s has %d of %d consumed, less than the %d to restore", in.InvoiceID, b.ID, b.ConsumedCents, b.AmountCents, leg.TakeCents)
		}
	}
	// On a paid invoice a share goes back to its block only where it stays
	// spendable; the rest is new credit, so the balance rises by the note's
	// whole amount.
	legs := plan.Legs
	restoredCents := plan.RestoredCents
	if in.PaidInvoice {
		legs = nil
		for _, leg := range plan.Legs {
			if spendableAt(states[leg.BlockID], at) {
				legs = append(legs, leg)
			} else {
				restoredCents -= leg.TakeCents
			}
		}
	}
	unattributed := in.AmountCents - restoredCents
	if unattributed > 0 && plan.LegacyCents > 0 {
		// No silent fallback: name the invoice whose credit could not be
		// traced to its blocks.
		slog.WarnContext(ctx, "credit return: part of the credit goes back as new credit with no expiry, because the invoice drew it before drains recorded their blocks (2026-07-05)",
			"invoice_id", in.InvoiceID, "customer_id", in.CustomerID, "currency", currency,
			"returned_cents", in.AmountCents, "restored_cents", restoredCents,
			"unattributed_cents", unattributed, "paid_invoice", in.PaidInvoice)
	}
	if s.beforeRestore != nil {
		s.beforeRestore(ctx, tx)
	}

	result := domain.CreditReturn{ReturnedCents: in.AmountCents, RestoredCents: restoredCents, UnattributedCents: unattributed}
	type followUp struct {
		block    blockState
		take     int64
		consumed int64 // the block's consumed_cents after the restore
		action   restoreAction
	}
	followUps := make([]followUp, 0, len(legs))
	for _, leg := range legs {
		b := states[leg.BlockID]
		action := classifyRestore(b, at)
		if in.PaidInvoice {
			action = restoreLive // spendableAt held above
		}
		consumed := b.ConsumedCents
		if action == restoreThenExpire && b.AmountCents > consumed {
			// The sweep has not written off this grant's remainder yet. It
			// would at its next run; write it off now, as the sweep does, so
			// the grant is never left with an expiry entry and live headroom.
			ok, _, _, err := s.expireGrantRemainderTx(ctx, tx, tenantID, in.CustomerID, b.ID, b.Currency, b.AmountCents-consumed, *b.ExpiresAt)
			if err != nil {
				return domain.CreditLedgerEntry{}, domain.CreditReturn{}, err
			}
			if !ok {
				return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("credit return for invoice %s: grant %s changed under the ledger locks", in.InvoiceID, b.ID)
			}
			consumed = b.AmountCents
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE customer_credit_ledger
			SET consumed_cents = consumed_cents - $1
			WHERE tenant_id = $2 AND id = $3 AND consumed_cents = $4
		`, leg.TakeCents, tenantID, b.ID, consumed)
		if err != nil {
			return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("restore %d to block %s: %w", leg.TakeCents, b.ID, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("restore %d to block %s: rows affected: %w", leg.TakeCents, b.ID, err)
		} else if n != 1 {
			return domain.CreditLedgerEntry{}, domain.CreditReturn{}, errs.InvalidState(fmt.Sprintf(
				"credit return for invoice %s: block %s changed between the read and the restore; retry", in.InvoiceID, b.ID)).
				WithCode("credit_block_changed")
		}
		followUps = append(followUps, followUp{block: b, take: leg.TakeCents, consumed: consumed - leg.TakeCents, action: action})
	}

	row.ConsumedCents = restoredCents
	switch {
	case in.PaidInvoice:
		// paid_invoice marks the row for paidReturnedTx: the base of the
		// next paid note's cumulative share.
		row.Metadata = map[string]any{"paid_invoice": true, "drawn_share_cents": drawnShare}
		if len(legs) > 0 {
			row.Metadata["restored_blocks"] = legs
			row.Metadata["restored_cents"] = restoredCents
		}
	case len(legs) > 0:
		row.Metadata = map[string]any{"restored_blocks": legs, "restored_cents": restoredCents}
	}
	out, _, err := s.insertEntryInTx(ctx, tx, tenantID, row)
	if err != nil {
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, err
	}

	for _, f := range followUps {
		switch f.action {
		case restoreThenRetire:
			g := commitGrantState{
				GrantID: f.block.ID, CustomerID: in.CustomerID, Currency: f.block.Currency,
				AmountCents: f.block.AmountCents, ConsumedCents: f.consumed, CNRetiredCents: f.block.CNRetiredCents,
			}
			if err := s.retireCommitSliceTx(ctx, tx, tenantID, g, f.take, false, false,
				fmt.Sprintf("Commit retired — funding invoice voided (grant %s)", f.block.ID),
				map[string]any{
					"reason": "commit_void_retire", "grant_id": f.block.ID,
					"funding_invoice_id": f.block.SourceInvoiceID, "returned_for_invoice_id": in.InvoiceID,
				},
			); err != nil {
				return domain.CreditLedgerEntry{}, domain.CreditReturn{}, err
			}
			result.CommitRetiredOnReturnCents += f.take
		case restoreThenExpire:
			ok, _, _, err := s.expireGrantRemainderTx(ctx, tx, tenantID, in.CustomerID, f.block.ID, f.block.Currency, f.take, at)
			if err != nil {
				return domain.CreditLedgerEntry{}, domain.CreditReturn{}, err
			}
			if !ok {
				return domain.CreditLedgerEntry{}, domain.CreditReturn{}, fmt.Errorf("credit return for invoice %s: grant %s changed under the ledger locks", in.InvoiceID, f.block.ID)
			}
			result.ExpiredOnReturnCents += f.take
		}
	}

	after, err := currencyBalanceTx(ctx, tx, tenantID, in.CustomerID, currency)
	if err != nil {
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, err
	}
	if err := s.emitBalanceCrossings(ctx, tx, tenantID, in.CustomerID, currency, before, after); err != nil {
		return domain.CreditLedgerEntry{}, domain.CreditReturn{}, err
	}
	return out, result, nil
}

// refuseTakenSource returns the dedup error the insert would hit when a row
// for this source already exists, and the amount-mismatch error first when
// that row's amount or currency differs (checkSameSourceAmount).
func refuseTakenSource(ctx context.Context, tx *sql.Tx, tenantID string, row domain.CreditLedgerEntry) error {
	if err := checkSameSourceAmount(ctx, tx, tenantID, row); err != nil {
		return err
	}
	var exists bool
	if row.SourceCreditNoteID != "" {
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM customer_credit_ledger WHERE tenant_id = $1 AND source_credit_note_id = $2)`,
			tenantID, row.SourceCreditNoteID).Scan(&exists); err != nil {
			return fmt.Errorf("check credit note %s for a return: %w", row.SourceCreditNoteID, err)
		}
		if exists {
			return errs.AlreadyExists("credit_note_source",
				"credit ledger entry already exists for this credit note").WithCode("credit_note_source_taken")
		}
		return nil
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM customer_credit_ledger WHERE tenant_id = $1 AND source_invoice_reversal_id = $2)`,
		tenantID, row.SourceInvoiceReversalID).Scan(&exists); err != nil {
		return fmt.Errorf("check invoice %s for a reversal: %w", row.SourceInvoiceReversalID, err)
	}
	if exists {
		return errs.AlreadyExists("invoice_reversal_source",
			"credit ledger reversal already exists for this invoice").WithCode("credit_reversal_source_taken")
	}
	return nil
}

func currencyBalanceTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, currency string) (int64, error) {
	var b int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_cents), 0) FROM customer_credit_ledger WHERE tenant_id = $1 AND customer_id = $2 AND currency = $3`,
		tenantID, customerID, currency,
	).Scan(&b); err != nil {
		return 0, fmt.Errorf("read the %s credit balance: %w", currency, err)
	}
	return b, nil
}

// invoiceDrawsTx reads every usage row of the invoice, newest first (ids are
// xids, which sort by creation time), with no LIMIT. A row in another
// currency than the return's is refused: the drain draws only in the
// invoice's currency.
func invoiceDrawsTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, invoiceID, currency string) ([]usageDraw, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, currency, -amount_cents,
			CASE WHEN jsonb_typeof(metadata->'drained_blocks') = 'array' THEN metadata->'drained_blocks' END
		FROM customer_credit_ledger
		WHERE tenant_id = $1 AND customer_id = $2 AND invoice_id = $3 AND entry_type = $4
		ORDER BY id DESC
	`, tenantID, customerID, invoiceID, domain.CreditUsage)
	if err != nil {
		return nil, fmt.Errorf("read the draws on invoice %s: %w", invoiceID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []usageDraw
	for rows.Next() {
		var (
			d   usageDraw
			cur string
			raw []byte
		)
		if err := rows.Scan(&d.EntryID, &cur, &d.AmountCents, &raw); err != nil {
			return nil, fmt.Errorf("read the draws on invoice %s: %w", invoiceID, err)
		}
		if cur != currency {
			return nil, fmt.Errorf("invoice %s drew credit in %s, the return is in %s", invoiceID, cur, currency)
		}
		if raw != nil {
			d.Blocks = []drainedBlock{}
			if err := json.Unmarshal(raw, &d.Blocks); err != nil {
				return nil, fmt.Errorf("usage row %s: read drained_blocks: %w", d.EntryID, err)
			}
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the draws on invoice %s: %w", invoiceID, err)
	}
	return out, nil
}

// attributedCents is what the invoice's usage rows with a block record drew.
// planRestore checked each such row's blocks against its amount.
func attributedCents(draws []usageDraw) int64 {
	var sum int64
	for _, d := range draws {
		if d.Blocks != nil {
			sum += d.AmountCents
		}
	}
	return sum
}

// paidReturnedTx adds up the earlier paid-branch credit-note returns on the
// invoice (metadata paid_invoice), the base of paidDrawnShare.
func paidReturnedTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, invoiceID string) (int64, error) {
	var sum int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount_cents), 0)::bigint FROM customer_credit_ledger
		WHERE tenant_id = $1 AND customer_id = $2 AND invoice_id = $3 AND entry_type = $4
		  AND source_credit_note_id IS NOT NULL AND metadata->>'paid_invoice' = 'true'
	`, tenantID, customerID, invoiceID, domain.CreditGrant).Scan(&sum); err != nil {
		return 0, fmt.Errorf("read earlier credit notes on paid invoice %s: %w", invoiceID, err)
	}
	return sum, nil
}

// invoiceRestoredTx adds up, per block, what earlier returns on the invoice
// restored (their restored_blocks).
func invoiceRestoredTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, invoiceID string) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT r.value->>'block_id', SUM((r.value->>'take_cents')::bigint)::bigint
		FROM customer_credit_ledger e
		CROSS JOIN LATERAL jsonb_array_elements(
			CASE WHEN jsonb_typeof(e.metadata->'restored_blocks') = 'array'
			     THEN e.metadata->'restored_blocks' ELSE '[]'::jsonb END) AS r(value)
		WHERE e.tenant_id = $1 AND e.customer_id = $2 AND e.invoice_id = $3 AND e.entry_type = $4
		  AND (e.source_credit_note_id IS NOT NULL OR e.source_invoice_reversal_id IS NOT NULL)
		GROUP BY 1
	`, tenantID, customerID, invoiceID, domain.CreditGrant)
	if err != nil {
		return nil, fmt.Errorf("read earlier returns on invoice %s: %w", invoiceID, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var c int64
		if err := rows.Scan(&id, &c); err != nil {
			return nil, fmt.Errorf("read earlier returns on invoice %s: %w", invoiceID, err)
		}
		out[id] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read earlier returns on invoice %s: %w", invoiceID, err)
	}
	return out, nil
}

// blockStatesTx reads the source blocks under the ledger locks, with what the
// expiry sweep's rule needs: whether an owed draw holds the grant, whether the
// sweep already retired it, and for a commit whether its funding invoice is
// voided.
//
// The hold leaves out invoiceID, the invoice the credit is being returned
// for: it is the draw being undone (an unpaid one may still be queued for
// collection), never a draw the returned credit is owed to.
func blockStatesTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, invoiceID string, ids []string) (map[string]blockState, error) {
	out := make(map[string]blockState, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT g.id, g.entry_type, g.currency, g.amount_cents, g.consumed_cents, g.cn_retired_cents,
			g.expires_at, COALESCE(g.grant_kind, ''), COALESCE(g.source_invoice_id, ''),
			(g.expires_at IS NOT NULL AND `+domain.CreditDrawOwedExceptSQL("g", "$4")+`),
			EXISTS (
				SELECT 1 FROM customer_credit_ledger e
				WHERE e.tenant_id = g.tenant_id AND e.customer_id = g.customer_id
				  AND e.entry_type = 'expiry' AND e.description = 'Expired grant ' || g.id),
			(COALESCE(g.grant_kind, '') = 'commit' AND EXISTS (
				SELECT 1 FROM invoices fi
				WHERE fi.tenant_id = g.tenant_id AND fi.id = g.source_invoice_id AND fi.status = 'voided'))
		FROM customer_credit_ledger g
		WHERE g.tenant_id = $1 AND g.customer_id = $2 AND g.id = ANY($3::text[])
	`, tenantID, customerID, ids, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("read the blocks to restore: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b blockState
		if err := rows.Scan(&b.ID, (*string)(&b.EntryType), &b.Currency, &b.AmountCents, &b.ConsumedCents, &b.CNRetiredCents,
			&b.ExpiresAt, &b.GrantKind, &b.SourceInvoiceID, &b.Held, &b.Swept, &b.FundingVoided); err != nil {
			return nil, fmt.Errorf("read the blocks to restore: %w", err)
		}
		out[b.ID] = b
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the blocks to restore: %w", err)
	}
	return out, nil
}
