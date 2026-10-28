package dunning

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// panickingRetrier panics on one invoice's retry, standing in for a nil
// dereference on one unusual row, and declines the others.
type panickingRetrier struct{ invoiceID string }

func (p panickingRetrier) RetryPayment(_ context.Context, _, invoiceID, _ string) error {
	if invoiceID == p.invoiceID {
		panic("retry: nil dereference on " + invoiceID)
	}
	return errors.New("card_declined: your card was declined")
}

// TestProcessDueRuns_PanicStillLogsTheOtherRunsErrors: a batch processes its
// other runs past one whose retry panics, then raises the panic again. That
// panic drops the errors the batch collected, which only the scheduler logs
// and counts, so while one run kept panicking another run's error (a database
// error during a connection drop, here a policy read that fails) was never
// logged. Now the batch logs them before it raises the panic.
func TestProcessDueRuns_PanicStillLogsTheOtherRunsErrors(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	store := newMemStore()
	due := time.Now().UTC().Add(-time.Hour)
	store.runs["drun_err"] = domain.InvoiceDunningRun{
		ID: "drun_err", TenantID: "t1", InvoiceID: "inv_err", CustomerID: "cus_1",
		PolicyID: "dpol_unreadable", State: domain.DunningActive, NextActionAt: &due,
	}
	store.runs["drun_panic"] = domain.InvoiceDunningRun{
		ID: "drun_panic", TenantID: "t1", InvoiceID: "inv_panic", CustomerID: "cus_1",
		PolicyID: "dpol_1", State: domain.DunningActive, NextActionAt: &due,
	}
	svc := NewService(store, panickingRetrier{invoiceID: "inv_panic"}, nil)

	recovered := func() (r any) {
		defer func() { r = recover() }()
		svc.ProcessDueRuns(context.Background(), "t1", 20)
		return nil
	}()
	if recovered != "retry: nil dereference on inv_panic" {
		t.Fatalf("recovered %v, want the retry's panic raised again at the end of the batch", recovered)
	}
	if !strings.Contains(logs.String(), "get bound policy dpol_unreadable for run drun_err") {
		t.Fatal("the other run's error was not logged: the panic drops the batch's errors, so the batch must log them first")
	}
}
