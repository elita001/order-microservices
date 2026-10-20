package application

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"strings"
)

//go:embed templates/email.html
var templatesFS embed.FS

// html/template escapes every value, so event data cannot inject markup into emails.
var emailTemplate = template.Must(template.ParseFS(templatesFS, "templates/email.html"))

// content is the email body independent of the format: it is rendered as plain text
// and as HTML, and mail clients choose which one to show.
type content struct {
	Subject string
	Heading string
	Lines   []string
}

func (c content) text() string {
	var b strings.Builder
	b.WriteString(c.Heading)
	b.WriteString("\n\n")
	for _, line := range c.Lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n--\nOrderFlow. This is an automated message, please do not reply.\n")
	return b.String()
}

func (c content) html() (string, error) {
	var buf bytes.Buffer
	if err := emailTemplate.Execute(&buf, c); err != nil {
		return "", fmt.Errorf("render email template: %w", err)
	}
	return buf.String(), nil
}

func orderCreatedContent(in OrderCreatedInput) content {
	number := orderNumber(in.OrderID.String())
	lines := []string{fmt.Sprintf("We have received your order #%s and are waiting for the payment.", number)}
	for _, item := range in.Items {
		lines = append(lines, fmt.Sprintf("%d × %s — %s",
			item.Quantity, item.ProductID, formatMoney(item.Price*int64(item.Quantity), in.Currency)))
	}
	lines = append(lines, "Total: "+formatMoney(in.TotalAmount, in.Currency))
	return content{
		Subject: fmt.Sprintf("Order #%s received", number),
		Heading: "Thank you for your order!",
		Lines:   lines,
	}
}

func paymentSucceededContent(in PaymentSucceededInput) content {
	number := orderNumber(in.OrderID.String())
	return content{
		Subject: fmt.Sprintf("Payment for order #%s received", number),
		Heading: "Payment received",
		Lines: []string{
			fmt.Sprintf("We have received your payment of %s for order #%s.", formatMoney(in.Amount, in.Currency), number),
			"Transaction: " + in.TransactionID,
			"We are preparing your order for shipping and will send you the tracking number soon.",
		},
	}
}

func paymentFailedContent(in PaymentFailedInput) content {
	number := orderNumber(in.OrderID.String())
	return content{
		Subject: fmt.Sprintf("Payment for order #%s failed", number),
		Heading: "Payment failed",
		Lines: []string{
			fmt.Sprintf("Unfortunately, the payment of %s for order #%s was declined.", formatMoney(in.Amount, in.Currency), number),
			"Reason: " + strings.ReplaceAll(in.Reason, "_", " "),
			"Please place the order again or use another payment method.",
		},
	}
}

func orderShippedContent(in OrderShippedInput) content {
	number := orderNumber(in.OrderID.String())
	return content{
		Subject: fmt.Sprintf("Order #%s has been shipped", number),
		Heading: "Your order is on its way",
		Lines: []string{
			fmt.Sprintf("Your order #%s has been handed over to %s.", number, in.Carrier),
			"Tracking number: " + in.TrackingNumber,
			"Estimated delivery: " + in.EstimatedDeliveryAt.Format("January 2, 2006"),
		},
	}
}

// orderNumber is a short, human-friendly form of the order ID.
func orderNumber(orderID string) string {
	return strings.ToUpper(orderID[:8])
}

// formatMoney formats an amount in minor units; all supported currencies have 2 decimals.
func formatMoney(amount int64, currency string) string {
	sign := ""
	if amount < 0 {
		sign, amount = "-", -amount
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, amount/100, amount%100, currency)
}
