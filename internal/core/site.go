package core

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	_ "time/tzdata" // timezone names don't depend on the host

	"github.com/jackc/pgx/v5"

	"aether/internal/core/oapi"
	"aether/internal/db"
)

func siteJSON(s db.GetSiteRow) oapi.Site {
	return oapi.Site{
		Id: s.ID, Name: s.Name, Currency: s.Currency, Timezone: s.Timezone,
		PricesIncludeTax: s.PricesIncludeTax, CountryCode: s.CountryCode, RegionCode: s.RegionCode,
		PaymentAccountId: s.PaymentAccountID, PaymentsEnabled: s.PaymentsEnabled,
	}
}

// loadSite is the site's commerce settings; 404 when commerce is not on.
func loadSite(ctx context.Context, q *db.Queries) (db.GetSiteRow, error) {
	s, err := q.GetSite(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, errNotFound
	}
	return s, err
}

func (s *Server) GetSite(ctx context.Context, req oapi.GetSiteRequestObject) (oapi.GetSiteResponseObject, error) {
	var site db.GetSiteRow
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		site, err = loadSite(ctx, q)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.GetSite200JSONResponse(siteJSON(site)), nil
}

func isUpperCode(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// PutSite turns commerce on for a site, or updates its settings (owner
// only). The currency is fixed once the site has orders.
func (s *Server) PutSite(ctx context.Context, req oapi.PutSiteRequestObject) (oapi.PutSiteResponseObject, error) {
	staff, err := s.staff(ctx)
	if err != nil {
		return nil, err
	}
	body := req.Body
	name := strings.TrimSpace(body.Name)
	if name == "" {
		return nil, unprocessable("name must not be empty")
	}
	if !isUpperCode(body.Currency, 3) {
		return nil, unprocessable("currency must be an ISO 4217 code such as USD")
	}
	if body.CountryCode != nil && !isUpperCode(*body.CountryCode, 2) {
		return nil, unprocessable("country_code must be an ISO 3166-1 alpha-2 code such as AU")
	}
	timezone := deref(body.Timezone, "UTC")
	if _, err := time.LoadLocation(timezone); err != nil || timezone == "Local" {
		return nil, unprocessable("timezone must be an IANA name such as Australia/Sydney")
	}

	var saved db.UpsertSiteRow
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		if current, err := q.GetSite(ctx); err == nil && current.Currency != body.Currency {
			if hasOrders, err := q.AnyOrders(ctx); err != nil || hasOrders {
				if err != nil {
					return err
				}
				return conflict("the currency cannot change once the site has orders")
			}
		}
		saved, err = q.UpsertSite(ctx, db.UpsertSiteParams{
			ID: req.SiteId, Name: name, Currency: body.Currency, Timezone: timezone,
			PricesIncludeTax: deref(body.PricesIncludeTax, false), CountryCode: body.CountryCode, RegionCode: body.RegionCode,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	slog.Info("site settings saved", "site_id", req.SiteId, "subject", staff.Subject)
	return oapi.PutSite200JSONResponse(siteJSON(db.GetSiteRow(saved))), nil
}

func deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
