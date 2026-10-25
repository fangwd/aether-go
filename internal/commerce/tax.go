package commerce

import "github.com/shopspring/decimal"

// TaxRate is one rate of a tax class, already filtered to those valid now.
type TaxRate struct {
	// Country is nil for anywhere.
	Country *string
	// Region is nil for the whole country.
	Region   *string
	Rate     decimal.Decimal
	Priority int32
}

// Place is where the goods are taxed.
type Place struct {
	Country string
	Region  *string
}

type TaxMode int

const (
	// Exclusive: prices exclude tax; tax is added on top (US style).
	Exclusive TaxMode = iota
	// Inclusive: prices include tax; tax is the part of the price that is
	// tax (AU/EU).
	Inclusive
)

func specificity(r TaxRate, p Place) (int, bool) {
	switch {
	case r.Country == nil && r.Region == nil:
		return 0, true
	case r.Country != nil && *r.Country == p.Country && r.Region == nil:
		return 1, true
	case r.Country != nil && *r.Country == p.Country && r.Region != nil && p.Region != nil && *r.Region == *p.Region:
		return 2, true
	}
	return 0, false
}

// EffectiveRate is the combined rate at a place: only the most specific
// matching jurisdiction counts; within it, rates of different priorities
// stack (state + county) and the highest rate wins a tie in priority.
func EffectiveRate(rates []TaxRate, p Place) decimal.Decimal {
	best, found := -1, false
	for _, r := range rates {
		if s, ok := specificity(r, p); ok && s > best {
			best, found = s, true
		}
	}
	if !found {
		return decimal.Zero
	}
	byPriority := map[int32]decimal.Decimal{}
	for _, r := range rates {
		if s, ok := specificity(r, p); ok && s == best {
			if existing, ok := byPriority[r.Priority]; !ok || r.Rate.GreaterThan(existing) {
				byPriority[r.Priority] = r.Rate
			}
		}
	}
	total := decimal.Zero
	for _, r := range byPriority {
		total = total.Add(r)
	}
	return total
}

// TaxOn is the tax on amount at rate, unrounded.
func TaxOn(amount, rate decimal.Decimal, mode TaxMode) decimal.Decimal {
	if mode == Inclusive {
		return amount.Sub(amount.Div(decimal.NewFromInt(1).Add(rate)))
	}
	return amount.Mul(rate)
}
