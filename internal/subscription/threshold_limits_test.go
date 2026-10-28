package subscription

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/errs"
)

// usage_gte is a request decimal with documented limits: a decimal string of
// at most 100 characters, more than 0, under 10^26, at most 12 decimal places
// (the column is NUMERIC(38,12) with CHECK usage_gte > 0). A value outside
// them is a 422 naming item_thresholds[i].usage_gte, and nothing is written.
//
// Mutation-verify: drop the domain.CheckDecimal call in SetBillingThresholds →
// the size and scale cases go red; drop the zero refusal → the zero cases go
// red.

func thresholdFixture(t *testing.T) (svc *Service, subID, firstItem, secondItem string) {
	t.Helper()
	svc = NewService(newMemStore(), nil)
	sub, err := svc.Create(context.Background(), "t1", CreateInput{
		Code: "sub-limits", DisplayName: "Test", CustomerID: "c",
		Items: []CreateItemInput{{PlanID: "plan_base"}, {PlanID: "plan_addon"}},
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	fresh, err := svc.Get(context.Background(), "t1", sub.ID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	return svc, sub.ID, fresh.Items[0].ID, fresh.Items[1].ID
}

// setUsageGTE sets usage_gte on the second item (index 1, after a valid
// first one), so the 422 must name the index.
func setUsageGTE(t *testing.T, raw string) (string, error) {
	t.Helper()
	svc, subID, firstItem, itemID := thresholdFixture(t)
	updated, err := svc.SetBillingThresholds(context.Background(), "t1", subID, BillingThresholdsInput{
		ItemThresholds: []ItemThresholdInput{
			{SubscriptionItemID: firstItem, UsageGTE: "10"},
			{SubscriptionItemID: itemID, UsageGTE: raw},
		},
	})
	if err != nil {
		return "", err
	}
	for _, it := range updated.BillingThresholds.ItemThresholds {
		if it.SubscriptionItemID == itemID {
			return it.UsageGTE.String(), nil
		}
	}
	t.Fatal("threshold not stored")
	return "", nil
}

func TestUsageGTE_ValidEdgesStoredAsSent(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"0.000000000001", "0.000000000001"},
		{"99999999999999999999999999.999999999999", "99999999999999999999999999.999999999999"},
		{"99999999999999999999999999", "99999999999999999999999999"},
		{"1000", "1000"},
		{"3.14", "3.14"},
		{" 42 ", "42"},
		{"1.500000000000000", "1.5"},
		{"1e3", "1000"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := setUsageGTE(t, tc.raw)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got != tc.want {
				t.Errorf("stored %s, want %s", got, tc.want)
			}
		})
	}
}

func TestUsageGTE_OutOfRangeRefusedNamingTheField(t *testing.T) {
	for _, raw := range []string{
		"0", "0.000", "0e99999", "-0", // the column requires more than 0
		"-1",
		"100000000000000000000000000", // 10^26
		"1e26",
		"0.0000000000001", // 13 places
		"1.0000000000001",
		"abc", "", "1e",
		"1." + strings.Repeat("0", 120), // over 100 characters
		"1e9999999999",                  // exponent out of range
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := setUsageGTE(t, raw)
			var de *errs.DomainError
			if !errors.As(err, &de) || !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("err = %v, want a validation error", err)
			}
			if de.Field != "item_thresholds[1].usage_gte" {
				t.Errorf("field %q, want item_thresholds[1].usage_gte (%s)", de.Field, de.Message)
			}
		})
	}
}

func TestUsageGTE_ExponentOutOfRangeIsNamed(t *testing.T) {
	for _, raw := range []string{"1e9999999999", "1e-9999999999", "0e9999999999"} {
		_, err := setUsageGTE(t, raw)
		if err == nil || !strings.Contains(err.Error(), "exponent is out of range") {
			t.Errorf("%s: err = %v, want the exponent named", raw, err)
		}
	}
}

func TestUsageGTE_ExtremeExponents(t *testing.T) {
	for _, raw := range []string{"1e99999999", "1e-99999999", "0e99999999", "1e2147483647", "1e-2147483648"} {
		if _, err := setUsageGTE(t, raw); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s: err = %v, want a validation error", raw, err)
		}
	}
}
