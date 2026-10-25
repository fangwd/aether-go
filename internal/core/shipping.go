package core

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"aether/internal/commerce"
	"aether/internal/core/oapi"
	"aether/internal/db"
)

func checkCodeName(code, name string) error {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(name) == "" {
		return unprocessable("code and name must not be empty")
	}
	return nil
}

func checkRegions(regions []oapi.ZoneRegion) error {
	if len(regions) == 0 {
		return unprocessable("a zone needs at least one region")
	}
	for _, r := range regions {
		if !isUpperCode(r.CountryCode, 2) {
			return unprocessable("country_code must be an ISO 3166-1 alpha-2 code such as AU")
		}
	}
	return nil
}

func loadZone(ctx context.Context, q *db.Queries, id uuid.UUID) (oapi.Zone, error) {
	z, err := q.ZoneByID(ctx, id)
	if err != nil {
		return oapi.Zone{}, err
	}
	regions, err := q.ZoneRegions(ctx, id)
	out := oapi.Zone{Id: z.ID, Code: z.Code, Name: z.Name, Regions: []oapi.ZoneRegion{}}
	for _, r := range regions {
		out.Regions = append(out.Regions, oapi.ZoneRegion{CountryCode: r.CountryCode, RegionCode: ptr(r.RegionCode)})
	}
	return out, err
}

// replaceRegions sets a zone's regions; a region "*" is the whole country.
func replaceRegions(ctx context.Context, q *db.Queries, siteID, zoneID uuid.UUID, regions []oapi.ZoneRegion) error {
	if err := q.DeleteZoneRegions(ctx, zoneID); err != nil {
		return err
	}
	for _, r := range regions {
		if err := q.InsertZoneRegion(ctx, db.InsertZoneRegionParams{SiteID: siteID, ZoneID: zoneID, CountryCode: r.CountryCode, RegionCode: deref(r.RegionCode, "*")}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) ListZones(ctx context.Context, req oapi.ListZonesRequestObject) (oapi.ListZonesResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	out := oapi.ListZones200JSONResponse{}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		ids, err := q.ZoneIDs(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			z, err := loadZone(ctx, q, id)
			if err != nil {
				return err
			}
			out = append(out, z)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) CreateZone(ctx context.Context, req oapi.CreateZoneRequestObject) (oapi.CreateZoneResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	if err := checkCodeName(req.Body.Code, req.Body.Name); err != nil {
		return nil, err
	}
	if err := checkRegions(req.Body.Regions); err != nil {
		return nil, err
	}
	var zone oapi.Zone
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := loadSite(ctx, q); err != nil {
			return err
		}
		id := newID()
		if err := q.InsertZone(ctx, db.InsertZoneParams{SiteID: req.SiteId, ID: id, Code: strings.TrimSpace(req.Body.Code), Name: strings.TrimSpace(req.Body.Name)}); err != nil {
			return err
		}
		if err := replaceRegions(ctx, q, req.SiteId, id, req.Body.Regions); err != nil {
			return err
		}
		var err error
		zone, err = loadZone(ctx, q, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.CreateZone201JSONResponse(zone), nil
}

func (s *Server) UpdateZone(ctx context.Context, req oapi.UpdateZoneRequestObject) (oapi.UpdateZoneResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	if err := checkCodeName(req.Body.Code, req.Body.Name); err != nil {
		return nil, err
	}
	if err := checkRegions(req.Body.Regions); err != nil {
		return nil, err
	}
	var zone oapi.Zone
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		n, err := q.UpdateZone(ctx, db.UpdateZoneParams{ID: req.Id, Code: strings.TrimSpace(req.Body.Code), Name: strings.TrimSpace(req.Body.Name)})
		if err != nil {
			return err
		}
		if n == 0 {
			return errNotFound
		}
		if err := replaceRegions(ctx, q, req.SiteId, req.Id, req.Body.Regions); err != nil {
			return err
		}
		zone, err = loadZone(ctx, q, req.Id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.UpdateZone200JSONResponse(zone), nil
}

// DeleteZone deletes a zone with its regions and rates.
func (s *Server) DeleteZone(ctx context.Context, req oapi.DeleteZoneRequestObject) (oapi.DeleteZoneResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		n, err := q.DeleteZone(ctx, req.Id)
		if err == nil && n == 0 {
			return errNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.DeleteZone204Response{}, nil
}

func (s *Server) ListMethods(ctx context.Context, req oapi.ListMethodsRequestObject) (oapi.ListMethodsResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	out := oapi.ListMethods200JSONResponse{}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		rows, err := q.ShippingMethods(ctx)
		for _, m := range rows {
			out = append(out, oapi.Method{Id: m.ID, Code: m.Code, Name: m.Name, Carrier: m.Carrier, Kind: oapi.MethodKind(m.Kind), Status: m.Status, TaxClassId: m.TaxClassID})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func checkMethod(body *oapi.MethodInput) (status string, err error) {
	if err := checkCodeName(body.Code, body.Name); err != nil {
		return "", err
	}
	status = deref(body.Status, "active")
	if status != "active" && status != "inactive" {
		return "", unprocessable("status must be active or inactive")
	}
	return status, nil
}

func methodFrom(id uuid.UUID, body *oapi.MethodInput, status string) oapi.Method {
	return oapi.Method{Id: id, Code: strings.TrimSpace(body.Code), Name: strings.TrimSpace(body.Name), Carrier: body.Carrier,
		Kind: body.Kind, Status: status, TaxClassId: body.TaxClassId}
}

func (s *Server) CreateMethod(ctx context.Context, req oapi.CreateMethodRequestObject) (oapi.CreateMethodResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	status, err := checkMethod(req.Body)
	if err != nil {
		return nil, err
	}
	id := newID()
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := loadSite(ctx, q); err != nil {
			return err
		}
		return q.InsertShippingMethod(ctx, db.InsertShippingMethodParams{
			SiteID: req.SiteId, ID: id, Code: strings.TrimSpace(req.Body.Code), Name: strings.TrimSpace(req.Body.Name),
			Carrier: req.Body.Carrier, Kind: string(req.Body.Kind), Status: status, TaxClassID: req.Body.TaxClassId,
		})
	})
	if err != nil {
		return nil, err
	}
	return oapi.CreateMethod201JSONResponse(methodFrom(id, req.Body, status)), nil
}

func (s *Server) UpdateMethod(ctx context.Context, req oapi.UpdateMethodRequestObject) (oapi.UpdateMethodResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	status, err := checkMethod(req.Body)
	if err != nil {
		return nil, err
	}
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		n, err := q.UpdateShippingMethod(ctx, db.UpdateShippingMethodParams{
			ID: req.Id, Code: strings.TrimSpace(req.Body.Code), Name: strings.TrimSpace(req.Body.Name),
			Carrier: req.Body.Carrier, Kind: string(req.Body.Kind), Status: status, TaxClassID: req.Body.TaxClassId,
		})
		if err == nil && n == 0 {
			return errNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.UpdateMethod200JSONResponse(methodFrom(req.Id, req.Body, status)), nil
}

// DeleteMethod deletes a method and its rates. Past orders keep its code.
func (s *Server) DeleteMethod(ctx context.Context, req oapi.DeleteMethodRequestObject) (oapi.DeleteMethodResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		n, err := q.DeleteShippingMethod(ctx, req.Id)
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
	return oapi.DeleteMethod204Response{}, nil
}

func rateJSON(id, methodID, zoneID uuid.UUID, minAmount decimal.Decimal, maxAmount *decimal.Decimal, price decimal.Decimal, currency string) oapi.Rate {
	return oapi.Rate{Id: id, MethodId: methodID, ZoneId: zoneID, MinOrderAmount: money(minAmount, currency),
		MaxOrderAmount: moneyPtr(maxAmount, currency), Price: money(price, currency)}
}

func (s *Server) ListShippingRates(ctx context.Context, req oapi.ListShippingRatesRequestObject) (oapi.ListShippingRatesResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	out := oapi.ListShippingRates200JSONResponse{}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		rows, err := q.ShippingRates(ctx)
		for _, r := range rows {
			out = append(out, rateJSON(r.ID, r.MethodID, r.ZoneID, r.MinOrderAmount, r.MaxOrderAmount, r.Price, site.Currency))
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type rateInput struct {
	minAmount, price decimal.Decimal
	maxAmount        *decimal.Decimal
}

// checkRate: a price tier for orders whose subtotal is at least
// min_order_amount (and below max_order_amount); pickup methods take none.
func checkRate(ctx context.Context, q *db.Queries, body *oapi.RateInput, currency string) (rateInput, error) {
	var r rateInput
	var err error
	if r.minAmount, err = amountOr("min_order_amount", body.MinOrderAmount, decimal.Zero); err != nil {
		return r, err
	}
	if r.price, err = amountOr("price", &body.Price, decimal.Zero); err != nil {
		return r, err
	}
	if r.maxAmount, err = amount("max_order_amount", body.MaxOrderAmount); err != nil {
		return r, err
	}
	for _, f := range []struct {
		name string
		d    decimal.Decimal
	}{{"min_order_amount", r.minAmount}, {"price", r.price}} {
		if f.d.IsNegative() {
			return r, unprocessable("%s must not be negative", f.name)
		}
		if err := checkAmount(f.name, f.d, currency); err != nil {
			return r, err
		}
	}
	if r.maxAmount != nil && r.maxAmount.LessThanOrEqual(r.minAmount) {
		return r, unprocessable("max_order_amount must be above min_order_amount")
	}
	kind, err := q.ShippingMethodKind(ctx, body.MethodId)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, unprocessable("method_id does not exist")
	}
	if err != nil {
		return r, err
	}
	if kind == "pickup" {
		return r, unprocessable("pickup methods are free and take no rates")
	}
	return r, nil
}

func (s *Server) CreateShippingRate(ctx context.Context, req oapi.CreateShippingRateRequestObject) (oapi.CreateShippingRateResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var out oapi.Rate
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		in, err := checkRate(ctx, q, req.Body, site.Currency)
		if err != nil {
			return err
		}
		r, err := q.InsertShippingRate(ctx, db.InsertShippingRateParams{
			SiteID: req.SiteId, ID: newID(), MethodID: req.Body.MethodId, ZoneID: req.Body.ZoneId,
			MinOrderAmount: in.minAmount, MaxOrderAmount: in.maxAmount, Price: in.price,
		})
		out = rateJSON(r.ID, r.MethodID, r.ZoneID, r.MinOrderAmount, r.MaxOrderAmount, r.Price, site.Currency)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.CreateShippingRate201JSONResponse(out), nil
}

func (s *Server) UpdateShippingRate(ctx context.Context, req oapi.UpdateShippingRateRequestObject) (oapi.UpdateShippingRateResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var out oapi.Rate
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		in, err := checkRate(ctx, q, req.Body, site.Currency)
		if err != nil {
			return err
		}
		r, err := q.UpdateShippingRate(ctx, db.UpdateShippingRateParams{
			ID: req.Id, MethodID: req.Body.MethodId, ZoneID: req.Body.ZoneId,
			MinOrderAmount: in.minAmount, MaxOrderAmount: in.maxAmount, Price: in.price,
		})
		out = rateJSON(r.ID, r.MethodID, r.ZoneID, r.MinOrderAmount, r.MaxOrderAmount, r.Price, site.Currency)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.UpdateShippingRate200JSONResponse(out), nil
}

func (s *Server) DeleteShippingRate(ctx context.Context, req oapi.DeleteShippingRateRequestObject) (oapi.DeleteShippingRateResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		n, err := q.DeleteShippingRate(ctx, req.Id)
		if err == nil && n == 0 {
			return errNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.DeleteShippingRate204Response{}, nil
}

// shippingOption is a way an order can be shipped, with its price.
type shippingOption struct {
	Code, Name string
	Carrier    *string
	Kind       string
	Price      decimal.Decimal
	TaxClassID *uuid.UUID
}

func (o shippingOption) json(currency string) oapi.ShippingOption {
	return oapi.ShippingOption{Code: o.Code, Name: o.Name, Carrier: o.Carrier, Kind: oapi.MethodKind(o.Kind), Price: money(o.Price, currency)}
}

// shippingOptions are the options for an order of subtotal going to
// address: pickup always; delivery methods only when they have a rate for
// the address and amount.
func shippingOptions(ctx context.Context, q *db.Queries, address *oapi.Address, subtotal decimal.Decimal, currency string) ([]shippingOption, error) {
	methods, err := q.ActiveShippingMethods(ctx)
	if err != nil {
		return nil, err
	}
	tiers := map[uuid.UUID][]commerce.RateTier{}
	if address != nil {
		rows, err := q.RateTiersFor(ctx, db.RateTiersForParams{CountryCode: address.CountryCode, RegionCode: deref(address.State, "")})
		if err != nil {
			return nil, err
		}
		for _, t := range rows {
			tiers[t.MethodID] = append(tiers[t.MethodID], commerce.RateTier{
				ZoneSpecificity: int(t.Specificity), MinOrderAmount: t.MinOrderAmount, MaxOrderAmount: t.MaxOrderAmount, Price: t.Price,
			})
		}
	}
	var out []shippingOption
	for _, m := range methods {
		price := decimal.Zero
		if m.Kind != "pickup" {
			p, ok := commerce.PickRate(tiers[m.ID], subtotal)
			if !ok {
				continue
			}
			price = p
		}
		out = append(out, shippingOption{Code: m.Code, Name: m.Name, Carrier: m.Carrier, Kind: m.Kind,
			Price: commerce.Round(price, currency), TaxClassID: m.TaxClassID})
	}
	return out, nil
}
