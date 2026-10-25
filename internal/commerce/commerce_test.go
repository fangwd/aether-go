package commerce

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func eq(t *testing.T, got, want decimal.Decimal, what string) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s: got %s, want %s", what, got, want)
	}
}

func id(n byte) uuid.UUID { return uuid.UUID{15: n} }

func ptr[T any](v T) *T { return &v }

func TestFormatUsesTheCurrencysMinorUnits(t *testing.T) {
	cases := []struct{ amount, currency, want string }{
		{"21", "USD", "21.00"}, {"2.345", "USD", "2.35"}, {"-2.345", "USD", "-2.35"},
		{"1500.5", "JPY", "1501"}, {"1.2345", "KWD", "1.235"},
	}
	for _, c := range cases {
		if got := Format(Round(d(c.amount), c.currency), c.currency); got != c.want {
			t.Errorf("%s %s: got %s, want %s", c.amount, c.currency, got, c.want)
		}
	}
}

func TestPrecisionFollowsTheCurrency(t *testing.T) {
	for _, c := range []struct {
		amount, currency string
		fits             bool
	}{{"2.50", "USD", true}, {"2.5000", "USD", true}, {"2.505", "USD", false}, {"10.5", "JPY", false}} {
		if FitsCurrency(d(c.amount), c.currency) != c.fits {
			t.Errorf("%s %s should fit: %v", c.amount, c.currency, c.fits)
		}
	}
}

// Pad Thai: Spice (exactly one of mild/spicy), Extras (up to two of egg/peanuts/lime).
var mild, spicy, egg, peanuts, lime = id(1), id(2), id(3), id(4), id(5)

func padThaiGroups() []OptionGroup {
	return []OptionGroup{
		{ID: id(10), Name: "Spice", MinSelect: 1, MaxSelect: ptr(1), Values: []uuid.UUID{mild, spicy}},
		{ID: id(20), Name: "Extras", MinSelect: 0, MaxSelect: ptr(2), Values: []uuid.UUID{egg, peanuts, lime}},
	}
}

func TestUnitPrice(t *testing.T) {
	eq(t, UnitPrice(d("15"), d("4"), d("2")), d("21"), "large spicy pad thai")
	eq(t, UnitPrice(d("1"), d("0"), d("-3")), d("0"), "never below zero")
}

func TestSelections(t *testing.T) {
	cases := []struct {
		chosen []uuid.UUID
		want   string
	}{
		{[]uuid.UUID{spicy}, ""},
		{[]uuid.UUID{mild, egg, peanuts}, ""},
		{[]uuid.UUID{egg}, "choose at least 1 from Spice"},
		{[]uuid.UUID{mild, spicy}, "choose at most 1 from Spice"},
		{[]uuid.UUID{mild, egg, peanuts, lime}, "choose at most 2 from Extras"},
		{[]uuid.UUID{mild, id(99)}, "an option does not belong to this product"},
		{[]uuid.UUID{mild, egg, egg}, "an option was chosen twice"},
	}
	for _, c := range cases {
		err := CheckSelection(padThaiGroups(), c.chosen)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%v: got %q, want %q", c.chosen, got, c.want)
		}
	}
	if CheckSelection(nil, nil) != nil || CheckSelection(nil, []uuid.UUID{mild}) == nil {
		t.Error("products without add-ons take no options")
	}
}

var noodles, drinks = id(1), id(2)

func cart() []PromotionLine {
	return []PromotionLine{
		{ProductID: id(10), CategoryIDs: []uuid.UUID{noodles}, Amount: d("42")},
		{ProductID: id(20), CategoryIDs: []uuid.UUID{drinks}, Amount: d("8")},
	}
}

func discounts(t *testing.T, out Outcome, want ...string) {
	t.Helper()
	for i, w := range want {
		eq(t, out.LineDiscounts[i], d(w), "line discount")
	}
}

func TestPercentOffIsSharedAcrossLinesToTheCent(t *testing.T) {
	out := ApplyPromotions([]Promotion{{ID: id(1), Discount: Discount{Kind: PercentOff, Value: d("10")}}}, cart(), "USD")
	eq(t, out.Applied[0].Amount, d("5.00"), "applied")
	discounts(t, out, "4.20", "0.80")
}

func TestConditionsLimitTheEligibleLines(t *testing.T) {
	out := ApplyPromotions([]Promotion{{ID: id(1), Discount: Discount{Kind: AmountOff, Value: d("50")},
		Conditions: Conditions{CategoryIDs: []uuid.UUID{noodles}}}}, cart(), "USD")
	discounts(t, out, "42", "0")
}

func TestMinimumSubtotalMustBeMet(t *testing.T) {
	out := ApplyPromotions([]Promotion{{ID: id(1), Discount: Discount{Kind: PercentOff, Value: d("10")},
		Conditions: Conditions{MinSubtotal: ptr(d("60"))}}}, cart(), "USD")
	if len(out.Applied) != 0 {
		t.Fatal("applied without meeting the minimum")
	}
	discounts(t, out, "0", "0")
}

func TestPromotionsAddUpWithoutGoingBelowZero(t *testing.T) {
	out := ApplyPromotions([]Promotion{
		{ID: id(1), Discount: Discount{Kind: AmountOff, Value: d("30")}},
		{ID: id(2), Discount: Discount{Kind: AmountOff, Value: d("30")}},
		{ID: id(3), Discount: Discount{Kind: FreeShipping}},
	}, cart(), "USD")
	eq(t, decimal.Sum(decimal.Zero, out.LineDiscounts...), d("50"), "total discount")
	eq(t, out.Applied[1].Amount, d("20"), "second promotion")
	if !out.FreeShipping {
		t.Error("free shipping not applied")
	}
}

func TestAllocationNeverLosesACent(t *testing.T) {
	lines := []PromotionLine{
		{ProductID: id(1), Amount: d("10")}, {ProductID: id(2), Amount: d("10")}, {ProductID: id(3), Amount: d("10")},
	}
	out := ApplyPromotions([]Promotion{{ID: id(1), Discount: Discount{Kind: AmountOff, Value: d("10")}}}, lines, "USD")
	discounts(t, out, "3.33", "3.33", "3.34")
}

func tier(spec int, min string, maxAmount *decimal.Decimal, price string) RateTier {
	return RateTier{ZoneSpecificity: spec, MinOrderAmount: d(min), MaxOrderAmount: maxAmount, Price: d(price)}
}

func TestShippingRates(t *testing.T) {
	free := []RateTier{tier(1, "0", nil, "10"), tier(1, "100", nil, "0")}
	if p, _ := PickRate(free, d("99.99")); !p.Equal(d("10")) {
		t.Error("below the free threshold")
	}
	if p, _ := PickRate(free, d("100")); !p.Equal(d("0")) {
		t.Error("at the free threshold")
	}
	if p, _ := PickRate([]RateTier{tier(1, "0", nil, "10"), tier(2, "0", nil, "5")}, d("20")); !p.Equal(d("5")) {
		t.Error("region zone overrides country zone")
	}
	if _, ok := PickRate([]RateTier{tier(1, "0", ptr(d("500")), "10")}, d("500")); ok {
		t.Error("capped tiers can leave an order unshippable")
	}
	if _, ok := PickRate(nil, d("1")); ok {
		t.Error("no tiers, no shipping")
	}
}

func rate(country, region *string, r string, priority int32) TaxRate {
	return TaxRate{Country: country, Region: region, Rate: d(r), Priority: priority}
}

var sydney = Place{Country: "AU", Region: ptr("NSW")}

func TestTaxRates(t *testing.T) {
	rates := []TaxRate{rate(nil, nil, "0.05", 0), rate(ptr("AU"), nil, "0.10", 0)}
	eq(t, EffectiveRate(rates, sydney), d("0.10"), "most specific jurisdiction wins")
	eq(t, EffectiveRate(rates, Place{Country: "NZ"}), d("0.05"), "fallback to anywhere")
	austin := Place{Country: "US", Region: ptr("TX")}
	eq(t, EffectiveRate([]TaxRate{rate(ptr("US"), ptr("TX"), "0.0625", 0), rate(ptr("US"), ptr("TX"), "0.02", 1)}, austin), d("0.0825"), "priorities stack")
	eq(t, EffectiveRate([]TaxRate{rate(ptr("US"), nil, "0.1", 0)}, sydney), d("0"), "no matching rate")
	eq(t, TaxOn(d("110"), d("0.10"), Inclusive).Round(2), d("10.00"), "inclusive tax is extracted")
	eq(t, TaxOn(d("100"), d("0.10"), Exclusive), d("10"), "exclusive tax is added")
}

func TestTotals(t *testing.T) {
	ex := ComputeTotals([]TotalsLine{{Amount: d("42"), TaxRate: d("0.0825")}}, d("10"), d("0.0825"), Exclusive, "USD")
	eq(t, ex.LineTax[0], d("3.47"), "line tax")
	eq(t, ex.ShippingTax, d("0.83"), "shipping tax")
	eq(t, ex.TaxTotal, d("4.30"), "tax total")
	eq(t, ex.GrandTotal, d("56.30"), "exclusive grand total")

	in := ComputeTotals([]TotalsLine{{Amount: d("22"), TaxRate: d("0.10")}}, d("11"), d("0.10"), Inclusive, "AUD")
	eq(t, in.TaxTotal, d("3.00"), "inclusive tax")
	eq(t, in.GrandTotal, d("33.00"), "inclusive tax is not added")

	disc := ComputeTotals([]TotalsLine{{Amount: d("100"), Discount: d("20"), TaxRate: d("0.10")}}, d("0"), d("0"), Exclusive, "USD")
	eq(t, disc.TaxTotal, d("8.00"), "discounts reduce the taxable amount")
	eq(t, disc.GrandTotal, d("88.00"), "discounted grand total")
}
