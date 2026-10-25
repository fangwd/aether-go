package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"aether/internal/core/oapi"
	"aether/internal/db"
	"aether/internal/ports"
)

// catalogView is what a caller may see of the catalog.
type catalogView int

const (
	// adminView shows everything, including cost and stock levels.
	adminView catalogView = iota
	// storefrontView shows active variants only, no cost or stock levels.
	storefrontView
)

func jsonObject(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// loadProducts loads products with everything nested, in the order of ids.
func loadProducts(ctx context.Context, q *db.Queries, ids []uuid.UUID, currency string, view catalogView, images ports.ImageStore) ([]oapi.Product, error) {
	rows, err := q.ProductsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	variants := map[uuid.UUID][]oapi.Variant{}
	variantRows, err := q.VariantsByProducts(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, v := range variantRows {
		if view == storefrontView && v.Status != "active" {
			continue
		}
		var stock *oapi.Stock
		if v.OnHand != nil && v.Reserved != nil && v.Available != nil {
			stock = &oapi.Stock{OnHand: *v.OnHand, Reserved: *v.Reserved, Available: *v.Available}
		}
		out := oapi.Variant{
			Id: v.ID, Sku: v.Sku, Barcode: v.Barcode, Name: v.Name, Attributes: jsonObject(v.Attributes),
			PriceExtra: money(v.PriceExtra, currency), Status: v.Status,
			Available: v.Status == "active" && (stock == nil || stock.Available > 0),
			Weight:    decimalPtr(v.Weight), WeightUnit: v.WeightUnit, Length: decimalPtr(v.Length),
			Width: decimalPtr(v.Width), Height: decimalPtr(v.Height), DimensionUnit: v.DimensionUnit,
		}
		if view == adminView {
			out.Stock = stock
		}
		variants[v.ProductID] = append(variants[v.ProductID], out)
	}

	values := map[uuid.UUID][]oapi.OptionValue{}
	valueRows, err := q.OptionValuesByProducts(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, v := range valueRows {
		values[v.GroupID] = append(values[v.GroupID], oapi.OptionValue{
			Id: v.ID, Code: v.Code, Label: v.Label, PriceExtra: money(v.PriceExtra, currency), IsDefault: v.IsDefault,
		})
	}
	groups := map[uuid.UUID][]oapi.OptionGroup{}
	groupRows, err := q.OptionGroupsByProducts(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, g := range groupRows {
		groups[g.ProductID] = append(groups[g.ProductID], oapi.OptionGroup{
			Id: g.ID, Code: g.Code, Name: g.Name, MinSelect: g.MinSelect, MaxSelect: g.MaxSelect,
			Values: nonNil(values[g.ID]),
		})
	}
	categories := map[uuid.UUID][]oapi.CategoryRef{}
	categoryRows, err := q.CategoriesByProducts(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, c := range categoryRows {
		categories[c.ProductID] = append(categories[c.ProductID], oapi.CategoryRef{Id: c.ID, Slug: c.Slug, Name: c.Name})
	}
	media := map[uuid.UUID][]oapi.ProductMedia{}
	mediaRows, err := q.MediaByProducts(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, m := range mediaRows {
		item := oapi.ProductMedia{MediaId: m.MediaID, Role: m.Role}
		if m.Alt != "" {
			item.Alt = &m.Alt
		}
		if images != nil && m.StorageProvider == images.Provider() {
			url := images.PublicURL(m.StorageKey)
			item.Url = &url
		}
		media[m.ProductID] = append(media[m.ProductID], item)
	}

	byID := map[uuid.UUID]oapi.Product{}
	for _, p := range rows {
		pv := nonNil(variants[p.ID])
		for i := range pv {
			extra, _ := decimal.NewFromString(pv[i].PriceExtra)
			pv[i].Price = money(p.Price.Add(extra), currency)
		}
		product := oapi.Product{
			Id: p.ID, Slug: p.Slug, Name: p.Name, ShortDescription: p.ShortDescription, Description: p.Description,
			Price: money(p.Price, currency), CompareAtPrice: moneyPtr(p.CompareAtPrice, currency),
			Status: p.Status, VariantAxes: nonNil(p.VariantAxes), Attributes: jsonObject(p.Attributes),
			Variants: pv, OptionGroups: nonNil(groups[p.ID]), Categories: nonNil(categories[p.ID]),
			Media: nonNil(media[p.ID]), CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		}
		if view == adminView {
			product.CostAmount = moneyPtr(p.CostAmount, currency)
			product.TaxClassId = p.TaxClassID
		}
		byID[p.ID] = product
	}
	out := make([]oapi.Product, 0, len(ids))
	for _, id := range ids {
		if p, ok := byID[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// nonNil keeps empty lists as [] rather than null in JSON.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func loadProduct(ctx context.Context, q *db.Queries, id uuid.UUID, currency string, view catalogView, images ports.ImageStore) (oapi.Product, error) {
	products, err := loadProducts(ctx, q, []uuid.UUID{id}, currency, view, images)
	if err != nil {
		return oapi.Product{}, err
	}
	if len(products) == 0 {
		return oapi.Product{}, errNotFound
	}
	return products[0], nil
}

// replaceVariants replaces the variant set. Variants matched by id, or else
// by identical attributes, keep their identity (and stock); unmatched ones
// are removed, or deactivated when orders, carts or stock history refer to
// them.
func replaceVariants(ctx context.Context, q *db.Queries, siteID, productID uuid.UUID, axes []string, inputs []variantInput) error {
	existing, err := q.VariantsOfProduct(ctx, productID)
	if err != nil {
		return err
	}
	resolved := make([]uuid.UUID, len(inputs))
	used := map[uuid.UUID]bool{}
	for i, in := range inputs {
		var matched uuid.UUID
		if in.ID != nil {
			for _, e := range existing {
				if e.ID == *in.ID {
					matched = e.ID
				}
			}
			if matched == uuid.Nil {
				return unprocessable("variant %s does not belong to this product", *in.ID)
			}
		} else {
			for _, e := range existing {
				if reflect.DeepEqual(jsonObject(e.Attributes), in.Attributes) {
					matched = e.ID
				}
			}
		}
		if matched != uuid.Nil {
			if used[matched] {
				return unprocessable("two variants refer to the same existing variant")
			}
			used[matched] = true
		}
		resolved[i] = matched
	}
	var removed []uuid.UUID
	for _, e := range existing {
		if !used[e.ID] {
			removed = append(removed, e.ID)
		}
	}
	if err := q.DeleteUnreferencedVariants(ctx, db.DeleteUnreferencedVariantsParams{ProductID: productID, Ids: removed}); err != nil {
		return err
	}
	// Referenced ones stay for history, deactivated; their SKU is freed.
	if err := q.DeactivateVariants(ctx, db.DeactivateVariantsParams{ProductID: productID, Ids: removed}); err != nil {
		return err
	}
	if err := q.SetVariantAxes(ctx, db.SetVariantAxesParams{ID: productID, VariantAxes: nonNil(axes)}); err != nil {
		return err
	}
	// SKU and axis-value uniqueness is checked at commit, so values may
	// move between variants here.
	for i, in := range inputs {
		id := resolved[i]
		if id == uuid.Nil {
			id = newID()
		}
		attributes, _ := json.Marshal(in.Attributes)
		if err := q.UpsertVariant(ctx, db.UpsertVariantParams{
			SiteID: siteID, ID: id, ProductID: productID, Sku: in.SKU, Barcode: in.Barcode, Name: in.Name,
			Attributes: attributes, PriceExtra: in.PriceExtra, Status: in.Status, SortOrder: int32(i),
			Weight: in.Weight, WeightUnit: in.WeightUnit, Length: in.Length, Width: in.Width, Height: in.Height,
			DimensionUnit: in.DimensionUnit,
		}); err != nil {
			return err
		}
	}
	return nil
}

// replaceOptionGroups replaces the add-on groups; groups and values keep
// identity by id or code.
func replaceOptionGroups(ctx context.Context, q *db.Queries, siteID, productID uuid.UUID, groups []optionGroupInput) error {
	existing, err := q.OptionGroupsOfProduct(ctx, productID)
	if err != nil {
		return err
	}
	groupIDs := make([]uuid.UUID, len(groups))
	for i, g := range groups {
		id, err := keepIdentity(g.ID, g.Code, existingIDs(existing, func(e db.OptionGroupsOfProductRow) (uuid.UUID, string) { return e.ID, e.Code }),
			"option group %s does not belong to this product")
		if err != nil {
			return err
		}
		groupIDs[i] = id
	}
	if err := q.DeleteOptionGroupsExcept(ctx, db.DeleteOptionGroupsExceptParams{ProductID: productID, Keep: groupIDs}); err != nil {
		return err
	}
	// Code uniqueness is checked at commit, so codes may move between rows.
	for i, g := range groups {
		if err := q.UpsertOptionGroup(ctx, db.UpsertOptionGroupParams{
			SiteID: siteID, ID: groupIDs[i], ProductID: productID, Code: g.Code, Name: g.Name,
			MinSelect: g.MinSelect, MaxSelect: g.MaxSelect, SortOrder: int32(i),
		}); err != nil {
			return err
		}
		existingValues, err := q.OptionValuesOfGroup(ctx, groupIDs[i])
		if err != nil {
			return err
		}
		valueIDs := make([]uuid.UUID, len(g.Values))
		for j, v := range g.Values {
			id, err := keepIdentity(v.ID, v.Code, existingIDs(existingValues, func(e db.OptionValuesOfGroupRow) (uuid.UUID, string) { return e.ID, e.Code }),
				"option value %s does not belong to group "+g.Code)
			if err != nil {
				return err
			}
			valueIDs[j] = id
		}
		if err := q.DeleteOptionValuesExcept(ctx, db.DeleteOptionValuesExceptParams{GroupID: groupIDs[i], Keep: valueIDs}); err != nil {
			return err
		}
		for j, v := range g.Values {
			if err := q.UpsertOptionValue(ctx, db.UpsertOptionValueParams{
				SiteID: siteID, ID: valueIDs[j], GroupID: groupIDs[i], Code: v.Code, Label: v.Label,
				PriceExtra: v.PriceExtra, IsDefault: v.IsDefault, SortOrder: int32(j),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

type idCode struct {
	id   uuid.UUID
	code string
}

func existingIDs[T any](rows []T, f func(T) (uuid.UUID, string)) []idCode {
	out := make([]idCode, len(rows))
	for i, r := range rows {
		out[i].id, out[i].code = f(r)
	}
	return out
}

// keepIdentity is the existing row's id when id or code matches, a new id
// otherwise; an id that matches nothing is an error.
func keepIdentity(id *uuid.UUID, code string, existing []idCode, foreign string) (uuid.UUID, error) {
	if id != nil {
		for _, e := range existing {
			if e.id == *id {
				return e.id, nil
			}
		}
		return uuid.Nil, unprocessable(foreign, *id)
	}
	for _, e := range existing {
		if e.code == code {
			return e.id, nil
		}
	}
	return newID(), nil
}

// replaceCategories replaces category assignments; the first is primary.
func replaceCategories(ctx context.Context, q *db.Queries, siteID, productID uuid.UUID, categoryIDs []uuid.UUID) error {
	if err := q.DeleteProductCategories(ctx, productID); err != nil {
		return err
	}
	for i, id := range categoryIDs {
		if err := q.InsertProductCategory(ctx, db.InsertProductCategoryParams{
			SiteID: siteID, ProductID: productID, CategoryID: id, IsPrimary: i == 0, Position: int32(i),
		}); err != nil {
			return err
		}
	}
	return nil
}

func replaceMedia(ctx context.Context, q *db.Queries, siteID, productID uuid.UUID, media []oapi.MediaInput) error {
	if err := q.DeleteProductMedia(ctx, productID); err != nil {
		return err
	}
	for i, m := range media {
		if err := q.InsertProductMedia(ctx, db.InsertProductMediaParams{
			SiteID: siteID, ProductID: productID, MediaID: m.MediaId, Role: deref(m.Role, "gallery"), Position: int32(i),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) AdminListProducts(ctx context.Context, req oapi.AdminListProductsRequestObject) (oapi.AdminListProductsResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	limit := pageSize(req.Params.Limit)
	var page oapi.ProductPage
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		var status *string
		if req.Params.Status != nil {
			st := string(*req.Params.Status)
			status = &st
		}
		ids, err := q.AdminProductIDs(ctx, db.AdminProductIDsParams{Status: status, Cursor: req.Params.Cursor, Lim: int32(limit + 1)})
		if err != nil {
			return err
		}
		ids, page.NextCursor = splitPage(ids, limit)
		page.Items, err = loadProducts(ctx, q, ids, site.Currency, adminView, s.Images)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.AdminListProducts200JSONResponse(page), nil
}

// CreateProduct creates a product with its variants, add-ons, categories
// and media.
func (s *Server) CreateProduct(ctx context.Context, req oapi.CreateProductRequestObject) (oapi.CreateProductResponseObject, error) {
	staff, err := s.staff(ctx)
	if err != nil {
		return nil, err
	}
	body := req.Body
	price, err := amountOr("price", &body.Price, decimal.Zero)
	if err != nil {
		return nil, err
	}
	compareAt, err := amount("compare_at_price", body.CompareAtPrice)
	if err != nil {
		return nil, err
	}
	cost, err := amount("cost_amount", body.CostAmount)
	if err != nil {
		return nil, err
	}
	axes := deref(body.VariantAxes, nil)
	variants, err := parseVariants(deref(body.Variants, nil))
	if err != nil {
		return nil, err
	}
	if len(variants) == 0 && len(axes) == 0 {
		variants = []variantInput{{Status: "active", Attributes: map[string]any{}}}
	}
	groups, err := parseOptionGroups(deref(body.OptionGroups, nil))
	if err != nil {
		return nil, err
	}
	status := "draft"
	if body.Status != nil {
		status = string(*body.Status)
	}
	attributes, _ := json.Marshal(deref(body.Attributes, map[string]any{}))

	id := newID()
	var product oapi.Product
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		for _, check := range []error{
			checkSlug(body.Slug), checkName(body.Name), checkPrices(price, compareAt, cost, site.Currency),
			checkVariants(axes, variants, price, site.Currency), checkOptionGroups(groups, site.Currency),
		} {
			if check != nil {
				return check
			}
		}
		if err := q.InsertProduct(ctx, db.InsertProductParams{
			SiteID: req.SiteId, ID: id, TaxClassID: body.TaxClassId, Slug: body.Slug, Name: strings.TrimSpace(body.Name),
			ShortDescription: body.ShortDescription, Description: body.Description, Price: price,
			CompareAtPrice: compareAt, CostAmount: cost, Status: status, VariantAxes: nonNil(axes), Attributes: attributes,
		}); err != nil {
			return err
		}
		if err := replaceVariants(ctx, q, req.SiteId, id, axes, variants); err != nil {
			return err
		}
		if err := replaceOptionGroups(ctx, q, req.SiteId, id, groups); err != nil {
			return err
		}
		if err := replaceCategories(ctx, q, req.SiteId, id, deref(body.CategoryIds, nil)); err != nil {
			return err
		}
		if err := replaceMedia(ctx, q, req.SiteId, id, deref(body.Media, nil)); err != nil {
			return err
		}
		if err := catalogChanged(ctx, q, req.SiteId); err != nil {
			return err
		}
		product, err = loadProduct(ctx, q, id, site.Currency, adminView, s.Images)
		return err
	})
	if err != nil {
		return nil, err
	}
	slog.Info("product created", "product_id", id, "subject", staff.Subject)
	return oapi.CreateProduct201JSONResponse(product), nil
}

func (s *Server) AdminGetProduct(ctx context.Context, req oapi.AdminGetProductRequestObject) (oapi.AdminGetProductResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var product oapi.Product
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		product, err = loadProduct(ctx, q, req.ProductId, site.Currency, adminView, s.Images)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.AdminGetProduct200JSONResponse(product), nil
}

// UpdateProduct updates product fields; absent fields are unchanged and
// null clears a nullable one. Variants and add-ons have their own endpoints.
func (s *Server) UpdateProduct(ctx context.Context, req oapi.UpdateProductRequestObject) (oapi.UpdateProductResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	body, has := req.Body, sent(ctx)
	var product oapi.Product
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		cur, err := q.ProductForUpdate(ctx, req.ProductId)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		next := db.UpdateProductParams{
			ID: req.ProductId, Slug: deref(body.Slug, cur.Slug), Name: cur.Name, ShortDescription: cur.ShortDescription,
			Description: cur.Description, Price: cur.Price, CompareAtPrice: cur.CompareAtPrice, CostAmount: cur.CostAmount,
			Status: cur.Status, TaxClassID: cur.TaxClassID, Attributes: cur.Attributes,
		}
		if body.Name != nil {
			next.Name = strings.TrimSpace(*body.Name)
		}
		if has["short_description"] {
			next.ShortDescription = body.ShortDescription
		}
		if has["description"] {
			next.Description = body.Description
		}
		if body.Price != nil {
			if next.Price, err = amountOr("price", body.Price, cur.Price); err != nil {
				return err
			}
		}
		if has["compare_at_price"] {
			if next.CompareAtPrice, err = amount("compare_at_price", body.CompareAtPrice); err != nil {
				return err
			}
		}
		if has["cost_amount"] {
			if next.CostAmount, err = amount("cost_amount", body.CostAmount); err != nil {
				return err
			}
		}
		if body.Status != nil {
			next.Status = string(*body.Status)
		}
		if has["tax_class_id"] {
			next.TaxClassID = body.TaxClassId
		}
		if body.Attributes != nil {
			next.Attributes, _ = json.Marshal(*body.Attributes)
		}
		for _, check := range []error{checkSlug(next.Slug), checkName(next.Name), checkPrices(next.Price, next.CompareAtPrice, next.CostAmount, site.Currency)} {
			if check != nil {
				return check
			}
		}
		if lowest, err := q.MinActiveVariantExtra(ctx, req.ProductId); err != nil {
			return err
		} else if lowest.HasActive && next.Price.Add(lowest.Extra).IsNegative() {
			return unprocessable("that price would make a variant's price negative")
		}
		if err := q.UpdateProduct(ctx, next); err != nil {
			return err
		}
		if body.CategoryIds != nil {
			if err := replaceCategories(ctx, q, req.SiteId, req.ProductId, *body.CategoryIds); err != nil {
				return err
			}
		}
		if body.Media != nil {
			if err := replaceMedia(ctx, q, req.SiteId, req.ProductId, *body.Media); err != nil {
				return err
			}
		}
		if err := catalogChanged(ctx, q, req.SiteId); err != nil {
			return err
		}
		product, err = loadProduct(ctx, q, req.ProductId, site.Currency, adminView, s.Images)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.UpdateProduct200JSONResponse(product), nil
}

// PutVariants replaces the product's variant axes and variants.
func (s *Server) PutVariants(ctx context.Context, req oapi.PutVariantsRequestObject) (oapi.PutVariantsResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	variants, err := parseVariants(req.Body.Variants)
	if err != nil {
		return nil, err
	}
	var product oapi.Product
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		price, err := q.ProductPriceForUpdate(ctx, req.ProductId)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if err := checkVariants(req.Body.VariantAxes, variants, price, site.Currency); err != nil {
			return err
		}
		if err := replaceVariants(ctx, q, req.SiteId, req.ProductId, req.Body.VariantAxes, variants); err != nil {
			return err
		}
		if err := catalogChanged(ctx, q, req.SiteId); err != nil {
			return err
		}
		product, err = loadProduct(ctx, q, req.ProductId, site.Currency, adminView, s.Images)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.PutVariants200JSONResponse(product), nil
}

// PutOptionGroups replaces the product's add-on option groups and values.
func (s *Server) PutOptionGroups(ctx context.Context, req oapi.PutOptionGroupsRequestObject) (oapi.PutOptionGroupsResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	groups, err := parseOptionGroups(req.Body.OptionGroups)
	if err != nil {
		return nil, err
	}
	var product oapi.Product
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		if _, err := q.ProductPriceForUpdate(ctx, req.ProductId); errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if err := checkOptionGroups(groups, site.Currency); err != nil {
			return err
		}
		if err := replaceOptionGroups(ctx, q, req.SiteId, req.ProductId, groups); err != nil {
			return err
		}
		if err := catalogChanged(ctx, q, req.SiteId); err != nil {
			return err
		}
		product, err = loadProduct(ctx, q, req.ProductId, site.Currency, adminView, s.Images)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.PutOptionGroups200JSONResponse(product), nil
}

// ListProducts is the storefront's active products, newest first.
func (s *Server) ListProducts(ctx context.Context, req oapi.ListProductsRequestObject) (oapi.ListProductsResponseObject, error) {
	limit := pageSize(req.Params.Limit)
	var page oapi.ProductPage
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		ids, err := q.StorefrontProductIDs(ctx, db.StorefrontProductIDsParams{Cursor: req.Params.Cursor, Category: req.Params.Category, Q: req.Params.Q, Lim: int32(limit + 1)})
		if err != nil {
			return err
		}
		ids, page.NextCursor = splitPage(ids, limit)
		page.Items, err = loadProducts(ctx, q, ids, site.Currency, storefrontView, s.Images)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.ListProducts200JSONResponse(page), nil
}

func (s *Server) GetProduct(ctx context.Context, req oapi.GetProductRequestObject) (oapi.GetProductResponseObject, error) {
	var product oapi.Product
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		id, err := q.ActiveProductIDBySlug(ctx, req.Slug)
		if err != nil {
			return err
		}
		product, err = loadProduct(ctx, q, id, site.Currency, storefrontView, s.Images)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.GetProduct200JSONResponse(product), nil
}
