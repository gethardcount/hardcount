package domain

// The words a subscription's audit rows are written with (ADR-090 frozen
// vocabulary; ADR-128). The subscription activity timeline reads these rows
// and words each one as an activity line, so a writer and the timeline must
// name a row the same way. Every writer uses these constants, and the
// timeline's golden and no-fallback tests range over the lists below: a new
// row kind that has no words fails a test instead of reaching the screen as
// "Subscription changed".
//
// The wire strings are frozen: audit rows already written carry them.

// Audit actions of subscription rows, besides the generic AuditActionCreate,
// AuditActionActivate, AuditActionCancel and AuditActionUpdate.
const (
	// AuditActionSubscriptionItemUpdated: an item's plan or quantity changed,
	// or a plan change was scheduled. Its metadata "action" says which
	// (SubAuditItemPlanChanged, SubAuditItemQuantityChanged).
	AuditActionSubscriptionItemUpdated = "subscription.item_updated"
	// AuditActionSubscriptionPendingChangeApplied: a scheduled plan change
	// took effect at the period boundary.
	AuditActionSubscriptionPendingChangeApplied = "subscription.pending_change_applied"
	// AuditActionSubscriptionThresholdCrossed: spend crossed a threshold and
	// an invoice was issued early.
	AuditActionSubscriptionThresholdCrossed = "subscription.threshold_crossed"
	// AuditActionSubscriptionThresholdDeferred: spend crossed a threshold,
	// but the price bills on peak usage, so it is billed at cycle close.
	AuditActionSubscriptionThresholdDeferred = "subscription.threshold_deferred"
	// AuditActionSubscriptionProrationFailed: an item change was made, but
	// its prorated charge or credit was not.
	AuditActionSubscriptionProrationFailed = "subscription.proration_failed"
	// AuditActionSubscriptionNetNegativeUsage: usage below zero after
	// corrections was billed as zero (#1193).
	AuditActionSubscriptionNetNegativeUsage = "subscription.net_negative_usage"
)

// SubscriptionAuditActions lists every action a subscription audit row is
// written with.
func SubscriptionAuditActions() []string {
	return []string{
		AuditActionCreate, AuditActionActivate, AuditActionCancel, AuditActionUpdate,
		AuditActionSubscriptionItemUpdated, AuditActionSubscriptionPendingChangeApplied,
		AuditActionSubscriptionThresholdCrossed, AuditActionSubscriptionThresholdDeferred,
		AuditActionSubscriptionProrationFailed, AuditActionSubscriptionNetNegativeUsage,
	}
}

// Values of a subscription audit row's metadata "action", which says what an
// AuditActionUpdate, AuditActionCancel or AuditActionSubscriptionItemUpdated
// row did.
const (
	SubAuditCancelScheduled           = "cancel_scheduled"
	SubAuditCancelCleared             = "cancel_cleared"
	SubAuditCollectionPaused          = "collection_paused"
	SubAuditCollectionResumed         = "collection_resumed"
	SubAuditTrialEnded                = "trial_ended"
	SubAuditTrialExtended             = "trial_extended"
	SubAuditBillingRetried            = "billing_retried"
	SubAuditThresholdsSet             = "billing_thresholds_set"
	SubAuditThresholdsCleared         = "billing_thresholds_cleared"
	SubAuditItemAdded                 = "item_added"
	SubAuditItemRemoved               = "item_removed"
	SubAuditPendingPlanChangeCanceled = "cancel_pending_item_plan_change"
	// SubAuditTrialEndCancel rides an AuditActionCancel row: the trial ended
	// with a cancellation due, so the subscription was canceled free.
	SubAuditTrialEndCancel = "trial_end_cancel"
	// SubAuditItemPlanChanged and SubAuditItemQuantityChanged ride an
	// AuditActionSubscriptionItemUpdated row.
	SubAuditItemPlanChanged     = "item_plan_changed"
	SubAuditItemQuantityChanged = "item_quantity_changed"
)

// SubscriptionAuditSubActions lists every metadata "action" a subscription
// audit row is written with.
func SubscriptionAuditSubActions() []string {
	return []string{
		SubAuditCancelScheduled, SubAuditCancelCleared, SubAuditCollectionPaused, SubAuditCollectionResumed,
		SubAuditTrialEnded, SubAuditTrialExtended, SubAuditBillingRetried, SubAuditThresholdsSet,
		SubAuditThresholdsCleared, SubAuditItemAdded, SubAuditItemRemoved, SubAuditPendingPlanChangeCanceled,
		SubAuditTrialEndCancel, SubAuditItemPlanChanged, SubAuditItemQuantityChanged,
	}
}
