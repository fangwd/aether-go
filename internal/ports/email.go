package ports

import (
	"context"
	"log/slog"
	"strings"
)

type Email struct {
	// From is "Name <local@domain>".
	From    string
	ReplyTo string
	To      string
	Subject string
	Text    string
	HTML    string
}

// DNSRecord is a record the merchant publishes to verify their domain.
type DNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type DomainStatus string

const (
	DomainPending  DomainStatus = "pending"
	DomainVerified DomainStatus = "verified"
	DomainFailed   DomainStatus = "failed"
)

// Mailer sends shop emails and verifies merchants' sending domains.
type Mailer interface {
	Send(ctx context.Context, email Email) error
	// RegisterDomain starts verifying a sending domain and returns the
	// records to publish.
	RegisterDomain(ctx context.Context, domain string) ([]DNSRecord, error)
	DomainStatus(ctx context.Context, domain string) (DomainStatus, error)
}

// LogMailer logs messages instead of sending them; domains verify at once.
// For development.
type LogMailer struct{}

func (LogMailer) Send(_ context.Context, email Email) error {
	slog.Info("email (log mailer)", "to", email.To, "subject", email.Subject)
	return nil
}

func (LogMailer) RegisterDomain(_ context.Context, domain string) ([]DNSRecord, error) {
	return []DNSRecord{{Type: "CNAME", Name: "dev._domainkey." + domain, Value: "dev.dkim.example.com"}}, nil
}

func (LogMailer) DomainStatus(context.Context, string) (DomainStatus, error) {
	return DomainVerified, nil
}

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

// Escape escapes text for HTML email bodies.
func Escape(text string) string { return htmlEscaper.Replace(text) }
