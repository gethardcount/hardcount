package payment

import "time"

// timePasses moves every wall-clock mark on the invoice back by d, which is
// what d of real time passing does to "now minus the mark": the hold
// (written on the database's clock) and the invoice's last write. It
// shifts updated_at RELATIVELY; the author's ageUpdated sets it to now()-age,
// which hides where updated_at was written.
func (r *shRig) timePasses(invoiceID string, d time.Duration) {
	r.t.Helper()
	r.exec(`UPDATE invoices SET
			updated_at = updated_at - make_interval(secs => $2),
			auto_charge_claimed_until = auto_charge_claimed_until - make_interval(secs => $2)
		WHERE id = $1`, invoiceID, d.Seconds())
}
