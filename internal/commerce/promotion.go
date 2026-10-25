package commerce

import (
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type DiscountKind int

const (
	// PercentOff takes Value percent (10 = 10%) off the eligible lines.
	PercentOff DiscountKind = iota + 1
	// AmountOff takes Value off the eligible lines, at most what they cost.
	AmountOff
	FreeShipping
)

type Discount struct {
	Kind  DiscountKind
	Value decimal.Decimal
}

type Conditions struct {
	// MinSubtotal: the eligible subtotal must be at least this.
	MinSubtotal *decimal.Decimal
	// ProductIDs empty means every product.
	ProductIDs []uuid.UUID
	// CategoryIDs empty means every category.
	CategoryIDs []uuid.UUID
}

type Promotion struct {
	ID         uuid.UUID
	Discount   Discount
	Conditions Conditions
}

type PromotionLine struct {
	ProductID   uuid.UUID
	CategoryIDs []uuid.UUID
	Amount      decimal.Decimal
}

type Applied struct {
	PromotionID uuid.UUID
	// Amount is money off the lines (zero for free shipping).
	Amount       decimal.Decimal
	FreeShipping bool
}

type Outcome struct {
	Applied []Applied
	// LineDiscounts is the discount per line, in the order given.
	LineDiscounts []decimal.Decimal
	FreeShipping  bool
}

func eligible(c Conditions, line PromotionLine) bool {
	byProduct := len(c.ProductIDs) == 0 || contains(c.ProductIDs, line.ProductID)
	byCategory := len(c.CategoryIDs) == 0
	for _, id := range line.CategoryIDs {
		if contains(c.CategoryIDs, id) {
			byCategory = true
		}
	}
	return byProduct && byCategory
}

func contains(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// allocate splits total across weights proportionally, in whole minor
// units; the rounding remainder goes to the last share so shares sum exactly.
func allocate(total decimal.Decimal, weights []decimal.Decimal, currency string) []decimal.Decimal {
	shares := make([]decimal.Decimal, len(weights))
	sum := decimal.Sum(decimal.Zero, weights...)
	if sum.IsZero() {
		return shares
	}
	units := MinorUnits(currency)
	allocated := decimal.Zero
	for i, w := range weights {
		shares[i] = total.Mul(w).Div(sum).Truncate(units)
		allocated = allocated.Add(shares[i])
	}
	for i := len(weights) - 1; i >= 0; i-- {
		if !weights[i].IsZero() {
			shares[i] = shares[i].Add(total.Sub(allocated))
			break
		}
	}
	return shares
}

// ApplyPromotions applies every promotion that qualifies. Each is computed
// on the original line amounts (no compounding) and no line goes below
// zero; promotions whose conditions are unmet are left out of Applied.
func ApplyPromotions(promotions []Promotion, lines []PromotionLine, currency string) Outcome {
	out := Outcome{LineDiscounts: make([]decimal.Decimal, len(lines))}
	for _, p := range promotions {
		weights := make([]decimal.Decimal, len(lines))
		for i, l := range lines {
			if eligible(p.Conditions, l) {
				weights[i] = l.Amount
			}
		}
		eligibleTotal := decimal.Sum(decimal.Zero, weights...)
		if eligibleTotal.IsZero() || (p.Conditions.MinSubtotal != nil && eligibleTotal.LessThan(*p.Conditions.MinSubtotal)) {
			continue
		}
		var wanted decimal.Decimal
		switch p.Discount.Kind {
		case PercentOff:
			wanted = eligibleTotal.Mul(p.Discount.Value).Div(decimal.NewFromInt(100))
		case AmountOff:
			wanted = p.Discount.Value
		case FreeShipping:
			out.FreeShipping = true
			out.Applied = append(out.Applied, Applied{PromotionID: p.ID, FreeShipping: true})
			continue
		}
		room := make([]decimal.Decimal, len(lines))
		for i := range lines {
			room[i] = decimal.Max(decimal.Zero, weights[i].Sub(out.LineDiscounts[i]))
		}
		amount := decimal.Min(Round(wanted, currency), decimal.Sum(decimal.Zero, room...))
		for i, share := range allocate(amount, room, currency) {
			out.LineDiscounts[i] = out.LineDiscounts[i].Add(share)
		}
		out.Applied = append(out.Applied, Applied{PromotionID: p.ID, Amount: amount})
	}
	return out
}
