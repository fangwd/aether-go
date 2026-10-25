package core

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"aether/internal/core/oapi"
	"aether/internal/db"
)

func categoryJSON(c db.CategoriesRow) oapi.Category {
	return oapi.Category{Id: c.ID, ParentId: c.ParentID, Slug: c.Slug, Name: c.Name, Description: c.Description, Status: c.Status, SortOrder: c.SortOrder}
}

func categoriesJSON(rows []db.CategoriesRow) []oapi.Category {
	out := make([]oapi.Category, len(rows))
	for i, c := range rows {
		out[i] = categoryJSON(c)
	}
	return out
}

func checkCategory(body *oapi.CategoryInput) (status string, err error) {
	if err := checkSlug(body.Slug); err != nil {
		return "", err
	}
	if err := checkName(body.Name); err != nil {
		return "", err
	}
	status = "active"
	if body.Status != nil {
		status = string(*body.Status)
	}
	return status, nil
}

// ListCategories is every category, flat and ordered for display; build
// the tree from parent_id.
func (s *Server) ListCategories(ctx context.Context, req oapi.ListCategoriesRequestObject) (oapi.ListCategoriesResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var rows []db.CategoriesRow
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		rows, err = q.Categories(ctx, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.ListCategories200JSONResponse(categoriesJSON(rows)), nil
}

// StorefrontCategories is the active categories, flat.
func (s *Server) StorefrontCategories(ctx context.Context, req oapi.StorefrontCategoriesRequestObject) (oapi.StorefrontCategoriesResponseObject, error) {
	var rows []db.CategoriesRow
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		if _, err := loadSite(ctx, q); err != nil {
			return err
		}
		rows, err = q.Categories(ctx, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.StorefrontCategories200JSONResponse(categoriesJSON(rows)), nil
}

func (s *Server) CreateCategory(ctx context.Context, req oapi.CreateCategoryRequestObject) (oapi.CreateCategoryResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	status, err := checkCategory(req.Body)
	if err != nil {
		return nil, err
	}
	var c db.CategoriesRow
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := loadSite(ctx, q); err != nil {
			return err
		}
		row, err := q.InsertCategory(ctx, db.InsertCategoryParams{
			SiteID: req.SiteId, ID: newID(), ParentID: req.Body.ParentId, Slug: req.Body.Slug,
			Name: strings.TrimSpace(req.Body.Name), Description: req.Body.Description, Status: status,
			SortOrder: deref(req.Body.SortOrder, 0),
		})
		if err != nil {
			return err
		}
		c = db.CategoriesRow(row)
		return catalogChanged(ctx, q, req.SiteId)
	})
	if err != nil {
		return nil, err
	}
	return oapi.CreateCategory201JSONResponse(categoryJSON(c)), nil
}

// UpdateCategory replaces a category's fields. A category cannot move
// under its own subtree.
func (s *Server) UpdateCategory(ctx context.Context, req oapi.UpdateCategoryRequestObject) (oapi.UpdateCategoryResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	status, err := checkCategory(req.Body)
	if err != nil {
		return nil, err
	}
	var c db.CategoriesRow
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		if parent := req.Body.ParentId; parent != nil {
			cycle, err := q.CategoryIsAncestor(ctx, db.CategoryIsAncestorParams{ParentID: *parent, CategoryID: req.CategoryId})
			if err != nil {
				return err
			}
			if cycle {
				return unprocessable("a category cannot be its own ancestor")
			}
		}
		row, err := q.UpdateCategory(ctx, db.UpdateCategoryParams{
			ID: req.CategoryId, ParentID: req.Body.ParentId, Slug: req.Body.Slug, Name: strings.TrimSpace(req.Body.Name),
			Description: req.Body.Description, Status: status, SortOrder: deref(req.Body.SortOrder, 0),
		})
		if err != nil {
			return err
		}
		c = db.CategoriesRow(row)
		return catalogChanged(ctx, q, req.SiteId)
	})
	if err != nil {
		return nil, err
	}
	return oapi.UpdateCategory200JSONResponse(categoryJSON(c)), nil
}

// DeleteCategory deletes a category and its subcategories; products stay,
// unassigned.
func (s *Server) DeleteCategory(ctx context.Context, req oapi.DeleteCategoryRequestObject) (oapi.DeleteCategoryResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		n, err := q.DeleteCategory(ctx, req.CategoryId)
		if err != nil {
			return err
		}
		if n == 0 {
			return errNotFound
		}
		return catalogChanged(ctx, q, req.SiteId)
	})
	if err != nil {
		return nil, err
	}
	return oapi.DeleteCategory204Response{}, nil
}
