package commerce

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// OptionGroup is a product's add-on group as selection rules.
type OptionGroup struct {
	ID        uuid.UUID
	Name      string
	MinSelect int
	// MaxSelect is nil for no limit.
	MaxSelect *int
	Values    []uuid.UUID
}

// CheckSelection checks a chosen set of option values against the
// product's groups; the error is fit to show to the shopper.
func CheckSelection(groups []OptionGroup, chosen []uuid.UUID) error {
	seen := map[uuid.UUID]bool{}
	for _, id := range chosen {
		if seen[id] {
			return fmt.Errorf("an option was chosen twice")
		}
		seen[id] = true
		if !belongs(groups, id) {
			return fmt.Errorf("an option does not belong to this product")
		}
	}
	for _, g := range groups {
		count := 0
		for _, v := range g.Values {
			if seen[v] {
				count++
			}
		}
		if count < g.MinSelect {
			return fmt.Errorf("choose at least %d from %s", g.MinSelect, g.Name)
		}
		if g.MaxSelect != nil && count > *g.MaxSelect {
			return fmt.Errorf("choose at most %d from %s", *g.MaxSelect, g.Name)
		}
	}
	return nil
}

func belongs(groups []OptionGroup, id uuid.UUID) bool {
	for _, g := range groups {
		for _, v := range g.Values {
			if v == id {
				return true
			}
		}
	}
	return false
}

// UnitPrice is base + variant extra + option extras, never below zero.
func UnitPrice(base, variantExtra decimal.Decimal, optionExtras ...decimal.Decimal) decimal.Decimal {
	return decimal.Max(decimal.Zero, base.Add(variantExtra).Add(decimal.Sum(decimal.Zero, optionExtras...)))
}
