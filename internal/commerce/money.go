// Package commerce holds the commerce rules with no IO: money, pricing,
// promotions, tax, shipping and totals.
package commerce

import "github.com/shopspring/decimal"

// MinorUnits is how many decimals a currency's amounts carry (ISO 4217).
func MinorUnits(currency string) int32 {
	switch currency {
	case "BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF", "UGX",
		"UYI", "VND", "VUV", "XAF", "XOF", "XPF":
		return 0
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		return 3
	}
	return 2
}

// Round rounds half away from zero to the currency's minor units.
func Round(amount decimal.Decimal, currency string) decimal.Decimal {
	return amount.Round(MinorUnits(currency))
}

// Format is an amount as the API shows it: rounded, with exactly the
// currency's decimals ("21" in USD is "21.00").
func Format(amount decimal.Decimal, currency string) string {
	return amount.StringFixed(MinorUnits(currency))
}

// FitsCurrency reports whether an entered amount is a whole number of minor
// units (no 1.005 USD).
func FitsCurrency(amount decimal.Decimal, currency string) bool {
	return amount.Equal(amount.Truncate(MinorUnits(currency)))
}
