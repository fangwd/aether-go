// Package smtp is aether's mailer over unencrypted SMTP, for a local catcher
// such as Mailpit. Sending domains verify at once: there is nothing to verify.
package smtp

import (
	"context"

	"github.com/wneessen/go-mail"

	"aether/internal/ports"
)

type Mailer struct {
	host string
	port int
}

var _ ports.Mailer = (*Mailer)(nil)

func New(host string, port int) *Mailer {
	return &Mailer{host: host, port: port}
}

func (m *Mailer) Send(ctx context.Context, email ports.Email) error {
	msg := mail.NewMsg()
	if err := msg.From(email.From); err != nil {
		return ports.Reject("from: %v", err)
	}
	if err := msg.To(email.To); err != nil {
		return ports.Reject("to: %v", err)
	}
	if email.ReplyTo != "" {
		if err := msg.ReplyTo(email.ReplyTo); err != nil {
			return ports.Reject("reply-to: %v", err)
		}
	}
	msg.Subject(email.Subject)
	msg.SetBodyString(mail.TypeTextPlain, email.Text)
	msg.AddAlternativeString(mail.TypeTextHTML, email.HTML)

	client, err := mail.NewClient(m.host, mail.WithPort(m.port), mail.WithTLSPolicy(mail.NoTLS))
	if err != nil {
		return ports.Unavailablef("smtp: %v", err)
	}
	if err := client.DialAndSendWithContext(ctx, msg); err != nil {
		return ports.Unavailablef("smtp: %v", err)
	}
	return nil
}

func (*Mailer) RegisterDomain(_ context.Context, domain string) ([]ports.DNSRecord, error) {
	return []ports.DNSRecord{{Type: "CNAME", Name: "local._domainkey." + domain, Value: "local.dkim.invalid"}}, nil
}

func (*Mailer) DomainStatus(context.Context, string) (ports.DomainStatus, error) {
	return ports.DomainVerified, nil
}
