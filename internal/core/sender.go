package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"aether/internal/core/oapi"
	"aether/internal/db"
	"aether/internal/ports"
)

// A site's own sending domain. Shop emails go out from it once the email
// provider has verified its DNS records; until then they come from
// the provider's fallback address under the shop's name.

func senderJSON(row db.SenderRow) oapi.Sender {
	out := oapi.Sender{Domain: row.Domain, FromLocal: row.FromLocal, FromName: row.FromName, ReplyTo: row.ReplyTo,
		Status: row.Status, VerifiedAt: row.VerifiedAt, DnsRecords: []oapi.DnsRecord{}}
	_ = json.Unmarshal(row.DnsRecords, &out.DnsRecords)
	return out
}

func loadSender(ctx context.Context, q *db.Queries) (db.SenderRow, error) {
	row, err := q.Sender(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, errNotFound
	}
	return row, err
}

func checkSender(body *oapi.SenderInput) (domain, local string, err error) {
	domain = strings.ToLower(strings.TrimSpace(body.Domain))
	ok := len(domain) <= 253 && strings.Contains(domain, ".")
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || strings.IndexFunc(label, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-')
		}) >= 0 {
			ok = false
		}
	}
	if !ok {
		return "", "", unprocessable("domain must be a domain name such as mail.example.com")
	}
	local = strings.TrimSpace(deref(body.FromLocal, "orders"))
	if local == "" || len(local) > 64 || strings.IndexFunc(local, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".-_+", r))
	}) >= 0 {
		return "", "", unprocessable("from_local must be the part before @, e.g. orders")
	}
	if body.ReplyTo != nil && !strings.Contains(*body.ReplyTo, "@") {
		return "", "", unprocessable("reply_to must be an email address")
	}
	return domain, local, nil
}

// refreshSender asks the email provider whether the domain is verified yet
// and records the answer.
func refreshSender(ctx context.Context, q *db.Queries, mailer ports.Mailer, domain string) error {
	status, err := mailer.DomainStatus(ctx, domain)
	if err != nil {
		return err
	}
	return q.SetSenderStatus(ctx, string(status))
}

// GetSender is the sending domain, its status refreshed from the email
// provider while pending.
func (s *Server) GetSender(ctx context.Context, req oapi.GetSenderRequestObject) (oapi.GetSenderResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var sender db.SenderRow
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		row, err := loadSender(ctx, q)
		if err != nil {
			return err
		}
		if row.Status == "pending" {
			if err := refreshSender(ctx, q, s.Mailer, row.Domain); err != nil {
				return err
			}
		}
		sender, err = loadSender(ctx, q)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.GetSender200JSONResponse(senderJSON(sender)), nil
}

// PutSender sets the sending domain (owner only) and registers it with the
// email provider. Changing the domain starts verification again; a domain
// belongs to one site.
func (s *Server) PutSender(ctx context.Context, req oapi.PutSenderRequestObject) (oapi.PutSenderResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	domain, local, err := checkSender(req.Body)
	if err != nil {
		return nil, err
	}
	var sender db.SenderRow
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := loadSite(ctx, q); err != nil {
			return err
		}
		current, err := q.Sender(ctx)
		hasCurrent := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		records := current.DnsRecords
		if !hasCurrent || current.Domain != domain {
			fresh, err := s.Mailer.RegisterDomain(ctx, domain)
			if err != nil {
				return err
			}
			records, _ = json.Marshal(fresh)
		}
		if err := q.UpsertSender(ctx, db.UpsertSenderParams{SiteID: req.SiteId, Domain: domain, FromLocal: local,
			FromName: req.Body.FromName, ReplyTo: req.Body.ReplyTo, DnsRecords: records, Changed: !hasCurrent || current.Domain != domain}); err != nil {
			return err
		}
		sender, err = loadSender(ctx, q)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.PutSender200JSONResponse(senderJSON(sender)), nil
}

// DeleteSender removes the site's own domain; emails fall back to
// the provider's address.
func (s *Server) DeleteSender(ctx context.Context, req oapi.DeleteSenderRequestObject) (oapi.DeleteSenderResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		n, err := q.DeleteSender(ctx)
		if err == nil && n == 0 {
			return errNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.DeleteSender204Response{}, nil
}
