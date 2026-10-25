package commerce

import "github.com/shopspring/decimal"

// RateTier is a rate tier of one shipping method in a zone that covers the
// address.
type RateTier struct {
	// ZoneSpecificity is 2 when the zone lists the address's region, 1 for
	// its whole country.
	ZoneSpecificity int
	MinOrderAmount  decimal.Decimal
	MaxOrderAmount  *decimal.Decimal
	Price           decimal.Decimal
}

// PickRate is the price for an order of subtotal: tiers of the most
// specific zone only, and within those the tier with the highest minimum
// the order meets. ok is false when the method does not ship this order.
func PickRate(tiers []RateTier, subtotal decimal.Decimal) (price decimal.Decimal, ok bool) {
	best := 0
	for _, t := range tiers {
		best = max(best, t.ZoneSpecificity)
	}
	var pick *RateTier
	for i, t := range tiers {
		if t.ZoneSpecificity != best || t.MinOrderAmount.GreaterThan(subtotal) {
			continue
		}
		if t.MaxOrderAmount != nil && !subtotal.LessThan(*t.MaxOrderAmount) {
			continue
		}
		if pick == nil || t.MinOrderAmount.GreaterThan(pick.MinOrderAmount) {
			pick = &tiers[i]
		}
	}
	if pick == nil {
		return decimal.Zero, false
	}
	return pick.Price, true
}
