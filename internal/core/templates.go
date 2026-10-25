package core

import (
	"fmt"
	"strings"

	"aether/internal/core/oapi"
	"aether/internal/ports"
)

// rendered is a shop email's subject and bodies.
type rendered struct {
	subject, text, html string
}

func htmlPage(shop, body string) string {
	return fmt.Sprintf(`<!doctype html><html><body style="font-family:sans-serif;color:#111"><h2>%s</h2>%s</body></html>`, ports.Escape(shop), body)
}

func orderLines(order oapi.Order) (text, html string) {
	var t, h strings.Builder
	h.WriteString(`<table cellpadding="4">`)
	for _, item := range order.Items {
		name := item.ProductName
		if item.VariantName != nil {
			name = fmt.Sprintf("%s (%s)", item.ProductName, *item.VariantName)
		}
		var labels []string
		for _, o := range item.Options {
			if label, ok := o["label"].(string); ok {
				labels = append(labels, label)
			}
		}
		detail := ""
		if len(labels) > 0 {
			detail = " + " + strings.Join(labels, ", ")
		}
		fmt.Fprintf(&t, "%d x %s%s  %s %s\n", item.Quantity, name, detail, item.Total, order.Currency)
		fmt.Fprintf(&h, `<tr><td>%d &times; %s%s</td><td align="right">%s %s</td></tr>`, item.Quantity, ports.Escape(name), ports.Escape(detail), item.Total, order.Currency)
	}
	h.WriteString("</table>")
	return t.String(), h.String()
}

func orderConfirmation(shop string, order oapi.Order) rendered {
	itemsText, itemsHTML := orderLines(order)
	included := ""
	if order.PricesIncludeTax {
		included = " (included)"
	}
	totals := fmt.Sprintf("Subtotal %s | Discounts -%s | Shipping %s | Tax %s%s | Total %s %s",
		order.Subtotal, order.DiscountTotal, order.ShippingTotal, order.TaxTotal, included, order.GrandTotal, order.Currency)
	return rendered{
		subject: fmt.Sprintf("%s: order #%d received", shop, order.OrderNumber),
		text:    fmt.Sprintf("Thanks for your order #%d from %s.\n\n%s\n%s\n", order.OrderNumber, shop, itemsText, totals),
		html:    htmlPage(shop, fmt.Sprintf("<p>Thanks for your order #%d.</p>%s<p>%s</p>", order.OrderNumber, itemsHTML, ports.Escape(totals))),
	}
}

func orderShipped(shop string, order oapi.Order, shipment oapi.Shipment) rendered {
	tracking := ""
	switch {
	case shipment.Carrier != nil && shipment.TrackingNumber != nil:
		tracking = fmt.Sprintf("Tracking: %s %s", *shipment.Carrier, *shipment.TrackingNumber)
	case shipment.TrackingNumber != nil:
		tracking = "Tracking number: " + *shipment.TrackingNumber
	}
	return rendered{
		subject: fmt.Sprintf("%s: order #%d is on its way", shop, order.OrderNumber),
		text:    fmt.Sprintf("Your order #%d from %s has been sent.\n%s\n", order.OrderNumber, shop, tracking),
		html:    htmlPage(shop, fmt.Sprintf("<p>Your order #%d has been sent.</p><p>%s</p>", order.OrderNumber, ports.Escape(tracking))),
	}
}

func orderRefunded(shop string, order oapi.Order, amount string) rendered {
	line := fmt.Sprintf("We have refunded %s %s for order #%d.", amount, order.Currency, order.OrderNumber)
	return rendered{
		subject: fmt.Sprintf("%s: refund for order #%d", shop, order.OrderNumber),
		text:    line + "\nIt can take a few days to reach your account.\n",
		html:    htmlPage(shop, fmt.Sprintf("<p>%s</p><p>It can take a few days to reach your account.</p>", ports.Escape(line))),
	}
}

func accountCode(shop, purpose, code string) rendered {
	subject, why := fmt.Sprintf("%s: confirm your email", shop), "to confirm your email address"
	if purpose == resetPassword {
		subject, why = fmt.Sprintf("%s: reset your password", shop), "to reset your password"
	}
	line := fmt.Sprintf("Your code %s is %s. It expires in 15 minutes.", why, code)
	return rendered{
		subject: subject,
		text:    line + "\nIf you did not ask for it, ignore this email.\n",
		html:    htmlPage(shop, fmt.Sprintf("<p>%s</p><p>If you did not ask for it, ignore this email.</p>", ports.Escape(line))),
	}
}
