package commerce

import "github.com/shopspring/decimal"

type TotalsLine struct {
	// Amount is unit price x quantity.
	Amount decimal.Decimal
	// Discount is this line's share of order discounts.
	Discount decimal.Decimal
	TaxRate  decimal.Decimal
}

type Totals struct {
	Subtotal      decimal.Decimal
	DiscountTotal decimal.Decimal
	ShippingTotal decimal.Decimal
	ShippingTax   decimal.Decimal
	// LineTax is per line, in the order given.
	LineTax []decimal.Decimal
	// TaxTotal is line tax plus shipping tax.
	TaxTotal   decimal.Decimal
	GrandTotal decimal.Decimal
}

// ComputeTotals rounds per line. Inclusive prices already contain their
// tax, so tax is reported but not added to the grand total.
func ComputeTotals(lines []TotalsLine, shipping, shippingTaxRate decimal.Decimal, mode TaxMode, currency string) Totals {
	t := Totals{LineTax: make([]decimal.Decimal, len(lines))}
	subtotal, discount := decimal.Zero, decimal.Zero
	for i, l := range lines {
		subtotal = subtotal.Add(l.Amount)
		discount = discount.Add(l.Discount)
		t.LineTax[i] = Round(TaxOn(decimal.Max(decimal.Zero, l.Amount.Sub(l.Discount)), l.TaxRate, mode), currency)
		t.TaxTotal = t.TaxTotal.Add(t.LineTax[i])
	}
	t.Subtotal = Round(subtotal, currency)
	t.DiscountTotal = Round(discount, currency)
	t.ShippingTotal = Round(shipping, currency)
	t.ShippingTax = Round(TaxOn(t.ShippingTotal, shippingTaxRate, mode), currency)
	t.TaxTotal = t.TaxTotal.Add(t.ShippingTax)
	added := t.TaxTotal
	if mode == Inclusive {
		added = decimal.Zero
	}
	t.GrandTotal = decimal.Max(decimal.Zero, t.Subtotal.Sub(t.DiscountTotal).Add(t.ShippingTotal).Add(added))
	return t
}
