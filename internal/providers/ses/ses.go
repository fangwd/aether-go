// Package ses is aether's mailer on Amazon SES (v2): sending, and
// per-merchant sending domains verified with Easy DKIM.
package ses

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/smithy-go"

	"aether/internal/ports"
)

type Mailer struct {
	client *sesv2.Client
}

var _ ports.Mailer = (*Mailer)(nil)

// New takes region and credentials from the client's AWS configuration.
func New(client *sesv2.Client) *Mailer {
	return &Mailer{client: client}
}

func unavailable(what string, err error) error {
	return ports.Unavailablef("SES %s: %s", what, errorMessage(err, "failed"))
}

func errorMessage(err error, fallback string) string {
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok && apiErr.ErrorMessage() != "" {
		return apiErr.ErrorMessage()
	}
	return fallback
}

func errorCode(err error) string {
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		return apiErr.ErrorCode()
	}
	return ""
}

func content(data string) *types.Content {
	return &types.Content{Data: aws.String(data), Charset: aws.String("UTF-8")}
}

func (m *Mailer) Send(ctx context.Context, email ports.Email) error {
	input := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(email.From),
		Destination:      &types.Destination{ToAddresses: []string{email.To}},
		Content: &types.EmailContent{Simple: &types.Message{
			Subject: content(email.Subject),
			Body:    &types.Body{Text: content(email.Text), Html: content(email.HTML)},
		}},
	}
	if email.ReplyTo != "" {
		input.ReplyToAddresses = []string{email.ReplyTo}
	}
	_, err := m.client.SendEmail(ctx, input)
	if err == nil {
		return nil
	}
	switch errorCode(err) {
	// The message itself was refused (e.g. a bad address); sending it again won't help.
	case "MessageRejected", "BadRequestException":
		return ports.Reject("SES: %s", errorMessage(err, "rejected"))
	}
	return unavailable("send", err)
}

func (m *Mailer) RegisterDomain(ctx context.Context, domain string) ([]ports.DNSRecord, error) {
	var dkim *types.DkimAttributes
	created, err := m.client.CreateEmailIdentity(ctx, &sesv2.CreateEmailIdentityInput{EmailIdentity: aws.String(domain)})
	switch {
	case err == nil:
		dkim = created.DkimAttributes
	case errorCode(err) == "AlreadyExistsException":
		existing, err := m.client.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: aws.String(domain)})
		if err != nil {
			return nil, unavailable("lookup", err)
		}
		dkim = existing.DkimAttributes
	default:
		return nil, unavailable("register", err)
	}
	records := []ports.DNSRecord{}
	if dkim != nil {
		for _, token := range dkim.Tokens {
			records = append(records, ports.DNSRecord{
				Type:  "CNAME",
				Name:  token + "._domainkey." + domain,
				Value: token + ".dkim.amazonses.com",
			})
		}
	}
	return records, nil
}

func (m *Mailer) DomainStatus(ctx context.Context, domain string) (ports.DomainStatus, error) {
	identity, err := m.client.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: aws.String(domain)})
	if err != nil {
		return "", unavailable("lookup", err)
	}
	switch {
	case identity.VerifiedForSendingStatus:
		return ports.DomainVerified, nil
	case identity.DkimAttributes != nil && identity.DkimAttributes.Status == types.DkimStatusFailed:
		return ports.DomainFailed, nil
	}
	return ports.DomainPending, nil
}
