package ports

import (
	"context"

	"github.com/google/uuid"
)

// CatalogHook tells the partner that renders a site's pages that its catalog
// changed (products, variants, add-ons, categories or staff stock changes),
// so it can rebuild them.
type CatalogHook interface {
	CatalogChanged(ctx context.Context, siteID uuid.UUID) error
}
