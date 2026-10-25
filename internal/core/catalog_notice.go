package core

import (
	"context"
	"time"

	"github.com/google/uuid"

	"aether/internal/db"
)

// catalogNoticeDelay is how long a catalog change waits for others before the
// site's one notice goes out, so a bulk edit makes one call.
const catalogNoticeDelay = 10 * time.Second

// catalogChanged queues the site's catalog.changed notice unless one is
// already waiting. Stock moved by orders is left out: storefronts read live
// stock, so only what staff change needs pages rebuilt.
func catalogChanged(ctx context.Context, q *db.Queries, siteID uuid.UUID) error {
	return q.NoteCatalogChanged(ctx, db.NoteCatalogChangedParams{
		SiteID: siteID, ID: newID(), DelaySecs: int32(catalogNoticeDelay / time.Second),
	})
}
