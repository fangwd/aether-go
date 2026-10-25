package core

import (
	"encoding/json"

	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"aether/internal/commerce"
	"aether/internal/core/oapi"
)

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

// amount parses a decimal field; nil stays nil.
func amount(field string, s *string) (*decimal.Decimal, error) {
	if s == nil {
		return nil, nil
	}
	d, err := decimal.NewFromString(strings.TrimSpace(*s))
	if err != nil {
		return nil, unprocessable("%s must be a decimal number", field)
	}
	return &d, nil
}

// amountOr parses a decimal field, def when absent.
func amountOr(field string, s *string, def decimal.Decimal) (decimal.Decimal, error) {
	d, err := amount(field, s)
	if err != nil || d == nil {
		return def, err
	}
	return *d, nil
}

func money(d decimal.Decimal, currency string) string { return commerce.Format(d, currency) }

func moneyPtr(d *decimal.Decimal, currency string) *string {
	if d == nil {
		return nil
	}
	s := money(*d, currency)
	return &s
}

func decimalPtr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.String()
	return &s
}

func checkSlug(slug string) error {
	ok := slug != "" && len(slug) <= 200 && !strings.HasPrefix(slug, "-") && !strings.HasSuffix(slug, "-")
	for _, c := range slug {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			ok = false
		}
	}
	if !ok {
		return unprocessable("slug must be lowercase letters, digits and inner hyphens")
	}
	return nil
}

func checkName(name string) error {
	if strings.TrimSpace(name) == "" {
		return unprocessable("name must not be empty")
	}
	return nil
}

func checkAmount(field string, d decimal.Decimal, currency string) error {
	if !commerce.FitsCurrency(d, currency) {
		return unprocessable("%s has more decimals than %s allows", field, currency)
	}
	return nil
}

func checkPrices(price decimal.Decimal, compareAt, cost *decimal.Decimal, currency string) error {
	if price.IsNegative() {
		return unprocessable("price must not be negative")
	}
	if err := checkAmount("price", price, currency); err != nil {
		return err
	}
	if compareAt != nil {
		if err := checkAmount("compare_at_price", *compareAt, currency); err != nil {
			return err
		}
		if compareAt.LessThanOrEqual(price) {
			return unprocessable("compare_at_price must be greater than price")
		}
	}
	if cost != nil {
		if err := checkAmount("cost_amount", *cost, currency); err != nil {
			return err
		}
		if cost.IsNegative() {
			return unprocessable("cost_amount must not be negative")
		}
	}
	return nil
}

// variantInput is a parsed oapi.VariantInput.
type variantInput struct {
	ID                            *uuid.UUID
	SKU, Barcode, Name            *string
	Attributes                    map[string]any
	PriceExtra                    decimal.Decimal
	Status                        string
	Weight, Length, Width, Height *decimal.Decimal
	WeightUnit, DimensionUnit     *string
}

func parseVariants(in []oapi.VariantInput) ([]variantInput, error) {
	out := make([]variantInput, len(in))
	for i, v := range in {
		p := variantInput{ID: v.Id, SKU: v.Sku, Barcode: v.Barcode, Name: v.Name, Status: "active",
			WeightUnit: v.WeightUnit, DimensionUnit: v.DimensionUnit, Attributes: map[string]any{}}
		if v.Attributes != nil {
			p.Attributes = *v.Attributes
		}
		if v.Status != nil {
			p.Status = string(*v.Status)
		}
		var err error
		if p.PriceExtra, err = amountOr("price_extra", v.PriceExtra, decimal.Zero); err != nil {
			return nil, err
		}
		for _, f := range []struct {
			name string
			in   *string
			out  **decimal.Decimal
		}{{"weight", v.Weight, &p.Weight}, {"length", v.Length, &p.Length}, {"width", v.Width, &p.Width}, {"height", v.Height, &p.Height}} {
			if *f.out, err = amount(f.name, f.in); err != nil {
				return nil, err
			}
		}
		out[i] = p
	}
	return out, nil
}

func checkVariants(axes []string, variants []variantInput, price decimal.Decimal, currency string) error {
	if len(variants) == 0 {
		return unprocessable("a product needs at least one variant")
	}
	axisSet := map[string]bool{}
	for _, a := range axes {
		if strings.TrimSpace(a) == "" || axisSet[a] {
			return unprocessable("variant_axes must be distinct, non-empty names")
		}
		axisSet[a] = true
	}
	combinations := map[string]bool{}
	for _, v := range variants {
		if len(v.Attributes) != len(axisSet) {
			return unprocessable("every variant needs exactly the axes %q", axes)
		}
		for k, val := range v.Attributes {
			if !axisSet[k] {
				return unprocessable("every variant needs exactly the axes %q", axes)
			}
			if s, ok := val.(string); !ok || strings.TrimSpace(s) == "" {
				return unprocessable("variant axis values must be non-empty strings")
			}
		}
		key, _ := json.Marshal(v.Attributes) // map keys marshal sorted
		if combinations[string(key)] {
			return unprocessable("two variants have the same axis values")
		}
		combinations[string(key)] = true
		if err := checkAmount("price_extra", v.PriceExtra, currency); err != nil {
			return err
		}
		if price.Add(v.PriceExtra).IsNegative() {
			return unprocessable("a variant's price (price + price_extra) must not be negative")
		}
		for name, d := range map[string]*decimal.Decimal{"weight": v.Weight, "length": v.Length, "width": v.Width, "height": v.Height} {
			if d != nil && d.IsNegative() {
				return unprocessable("%s must not be negative", name)
			}
		}
	}
	return nil
}

// optionGroupInput is a parsed oapi.OptionGroupInput.
type optionGroupInput struct {
	ID        *uuid.UUID
	Code      string
	Name      string
	MinSelect int32
	MaxSelect *int32
	Values    []optionValueInput
}

type optionValueInput struct {
	ID         *uuid.UUID
	Code       string
	Label      string
	PriceExtra decimal.Decimal
	IsDefault  bool
}

func parseOptionGroups(in []oapi.OptionGroupInput) ([]optionGroupInput, error) {
	out := make([]optionGroupInput, len(in))
	for i, g := range in {
		p := optionGroupInput{ID: g.Id, Code: g.Code, Name: g.Name, MinSelect: deref(g.MinSelect, 0), MaxSelect: g.MaxSelect}
		for _, v := range g.Values {
			extra, err := amountOr("price_extra", v.PriceExtra, decimal.Zero)
			if err != nil {
				return nil, err
			}
			p.Values = append(p.Values, optionValueInput{ID: v.Id, Code: v.Code, Label: v.Label, PriceExtra: extra, IsDefault: deref(v.IsDefault, false)})
		}
		out[i] = p
	}
	return out, nil
}

func checkOptionGroups(groups []optionGroupInput, currency string) error {
	codes := map[string]bool{}
	for _, g := range groups {
		if strings.TrimSpace(g.Code) == "" || codes[g.Code] {
			return unprocessable("option group codes must be distinct and non-empty")
		}
		codes[g.Code] = true
		if err := checkName(g.Name); err != nil {
			return err
		}
		if len(g.Values) == 0 {
			return unprocessable("option group %s has no values", g.Code)
		}
		count := int32(len(g.Values))
		maxSel := deref(g.MaxSelect, count)
		if g.MinSelect < 0 || maxSel < 1 || maxSel < g.MinSelect || g.MinSelect > count {
			return unprocessable("option group %s: need 0 <= min_select <= max_select and min_select <= number of values", g.Code)
		}
		defaults := int32(0)
		valueCodes := map[string]bool{}
		for _, v := range g.Values {
			if v.IsDefault {
				defaults++
			}
			if strings.TrimSpace(v.Code) == "" || valueCodes[v.Code] {
				return unprocessable("option group %s: value codes must be distinct and non-empty", g.Code)
			}
			valueCodes[v.Code] = true
			if strings.TrimSpace(v.Label) == "" {
				return unprocessable("option value labels must not be empty")
			}
			if err := checkAmount("price_extra", v.PriceExtra, currency); err != nil {
				return err
			}
		}
		if defaults > maxSel {
			return unprocessable("option group %s has more defaults than max_select", g.Code)
		}
	}
	return nil
}

const (
	defaultPage = 50
	maxPage     = 100
)

// pageSize is the requested page size, 1 to 100 (default 50).
func pageSize(limit *int64) int64 {
	return min(max(deref(limit, defaultPage), 1), maxPage)
}

// splitPage takes up to limit+1 ids (newest first) and returns the page and
// the next cursor.
func splitPage(ids []uuid.UUID, limit int64) ([]uuid.UUID, *uuid.UUID) {
	if int64(len(ids)) > limit {
		ids = ids[:limit]
		last := ids[len(ids)-1]
		return ids, &last
	}
	return ids, nil
}
