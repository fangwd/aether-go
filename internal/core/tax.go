package core

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"aether/internal/commerce"
	"aether/internal/core/oapi"
	"aether/internal/db"
)

// defaultTaxClass is how products without a tax class are taxed, if the
// site has a class with this code.
const defaultTaxClass = "standard"

// inUse turns deleting something still referenced into a conflict, not a
// bad reference.
func inUse(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
		return conflict("it is still in use")
	}
	return err
}

func (s *Server) ListTaxClasses(ctx context.Context, req oapi.ListTaxClassesRequestObject) (oapi.ListTaxClassesResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	out := oapi.ListTaxClasses200JSONResponse{}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		rows, err := q.TaxClasses(ctx)
		for _, r := range rows {
			out = append(out, oapi.TaxClass{Id: r.ID, Code: r.Code, Name: r.Name})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) CreateTaxClass(ctx context.Context, req oapi.CreateTaxClassRequestObject) (oapi.CreateTaxClassResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var row db.InsertTaxClassRow
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		if _, err := loadSite(ctx, q); err != nil {
			return err
		}
		row, err = q.InsertTaxClass(ctx, db.InsertTaxClassParams{SiteID: req.SiteId, ID: newID(), Code: strings.TrimSpace(req.Body.Code), Name: strings.TrimSpace(req.Body.Name)})
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.CreateTaxClass201JSONResponse{Id: row.ID, Code: row.Code, Name: row.Name}, nil
}

func (s *Server) UpdateTaxClass(ctx context.Context, req oapi.UpdateTaxClassRequestObject) (oapi.UpdateTaxClassResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var row db.UpdateTaxClassRow
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		row, err = q.UpdateTaxClass(ctx, db.UpdateTaxClassParams{ID: req.Id, Code: strings.TrimSpace(req.Body.Code), Name: strings.TrimSpace(req.Body.Name)})
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.UpdateTaxClass200JSONResponse{Id: row.ID, Code: row.Code, Name: row.Name}, nil
}

// DeleteTaxClass deletes a class and its rates; a class products or
// shipping methods still use is a conflict.
func (s *Server) DeleteTaxClass(ctx context.Context, req oapi.DeleteTaxClassRequestObject) (oapi.DeleteTaxClassResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		if err := q.DeleteTaxRatesOfClass(ctx, req.Id); err != nil {
			return err
		}
		n, err := q.DeleteTaxClass(ctx, req.Id)
		if err != nil {
			return inUse(err)
		}
		if n == 0 {
			return errNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return oapi.DeleteTaxClass204Response{}, nil
}

func checkTaxRate(body *oapi.TaxRateInput) (decimal.Decimal, error) {
	rate, err := amountOr("rate", &body.Rate, decimal.Zero)
	if err != nil {
		return rate, err
	}
	if rate.IsNegative() || rate.GreaterThanOrEqual(decimal.NewFromInt(1)) {
		return rate, unprocessable("rate must be a fraction from 0 to under 1 (0.1 = 10%%)")
	}
	if body.RegionCode != nil && body.CountryCode == nil {
		return rate, unprocessable("a region needs a country_code")
	}
	if body.CountryCode != nil && !isUpperCode(*body.CountryCode, 2) {
		return rate, unprocessable("country_code must be an ISO 3166-1 alpha-2 code such as AU")
	}
	if body.ValidFrom != nil && body.ValidUntil != nil && !body.ValidUntil.After(*body.ValidFrom) {
		return rate, unprocessable("valid_until must be after valid_from")
	}
	return rate, nil
}

func (s *Server) ListTaxRates(ctx context.Context, req oapi.ListTaxRatesRequestObject) (oapi.ListTaxRatesResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	out := oapi.ListTaxRates200JSONResponse{}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		rows, err := q.TaxRates(ctx)
		for _, r := range rows {
			out = append(out, oapi.TaxRate{Id: r.ID, TaxClassId: r.TaxClassID, CountryCode: r.CountryCode, RegionCode: r.RegionCode,
				Rate: r.Rate.String(), Priority: r.Priority, ValidFrom: r.ValidFrom, ValidUntil: r.ValidUntil})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) CreateTaxRate(ctx context.Context, req oapi.CreateTaxRateRequestObject) (oapi.CreateTaxRateResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	rate, err := checkTaxRate(req.Body)
	if err != nil {
		return nil, err
	}
	var r db.InsertTaxRateRow
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		r, err = q.InsertTaxRate(ctx, db.InsertTaxRateParams{
			SiteID: req.SiteId, ID: newID(), TaxClassID: req.Body.TaxClassId, CountryCode: req.Body.CountryCode,
			RegionCode: req.Body.RegionCode, Rate: rate, Priority: deref(req.Body.Priority, 0),
			ValidFrom: req.Body.ValidFrom, ValidUntil: req.Body.ValidUntil,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.CreateTaxRate201JSONResponse{Id: r.ID, TaxClassId: r.TaxClassID, CountryCode: r.CountryCode, RegionCode: r.RegionCode,
		Rate: r.Rate.String(), Priority: r.Priority, ValidFrom: r.ValidFrom, ValidUntil: r.ValidUntil}, nil
}

func (s *Server) UpdateTaxRate(ctx context.Context, req oapi.UpdateTaxRateRequestObject) (oapi.UpdateTaxRateResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	rate, err := checkTaxRate(req.Body)
	if err != nil {
		return nil, err
	}
	var r db.UpdateTaxRateRow
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		r, err = q.UpdateTaxRate(ctx, db.UpdateTaxRateParams{
			ID: req.Id, TaxClassID: req.Body.TaxClassId, CountryCode: req.Body.CountryCode, RegionCode: req.Body.RegionCode,
			Rate: rate, Priority: deref(req.Body.Priority, 0), ValidFrom: req.Body.ValidFrom, ValidUntil: req.Body.ValidUntil,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.UpdateTaxRate200JSONResponse{Id: r.ID, TaxClassId: r.TaxClassID, CountryCode: r.CountryCode, RegionCode: r.RegionCode,
		Rate: r.Rate.String(), Priority: r.Priority, ValidFrom: r.ValidFrom, ValidUntil: r.ValidUntil}, nil
}

func (s *Server) DeleteTaxRate(ctx context.Context, req oapi.DeleteTaxRateRequestObject) (oapi.DeleteTaxRateResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		n, err := q.DeleteTaxRate(ctx, req.Id)
		if err == nil && n == 0 {
			return errNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.DeleteTaxRate204Response{}, nil
}

// ratesInForce are the tax rates in force now, by class, plus the default
// class's id.
type ratesInForce struct {
	byClass      map[uuid.UUID][]commerce.TaxRate
	defaultClass *uuid.UUID
}

func loadRatesInForce(ctx context.Context, q *db.Queries) (ratesInForce, error) {
	r := ratesInForce{byClass: map[uuid.UUID][]commerce.TaxRate{}}
	rows, err := q.TaxRatesInForce(ctx)
	if err != nil {
		return r, err
	}
	for _, row := range rows {
		r.byClass[row.TaxClassID] = append(r.byClass[row.TaxClassID], commerce.TaxRate{
			Country: row.CountryCode, Region: row.RegionCode, Rate: row.Rate, Priority: row.Priority,
		})
	}
	id, err := q.TaxClassByCode(ctx, defaultTaxClass)
	if err == nil {
		r.defaultClass = &id
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return r, err
	}
	return r, nil
}

// forProduct is the rates of a product's class, or the default class when
// it has none.
func (r ratesInForce) forProduct(class *uuid.UUID) []commerce.TaxRate {
	if class == nil {
		class = r.defaultClass
	}
	return r.forClass(class)
}

// forClass is the rates of a shipping method's class; no class means
// shipping is not taxed.
func (r ratesInForce) forClass(class *uuid.UUID) []commerce.TaxRate {
	if class == nil {
		return nil
	}
	return r.byClass[*class]
}
