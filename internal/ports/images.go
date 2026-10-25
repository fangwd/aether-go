package ports

import (
	"context"

	"github.com/google/uuid"
)

// Upload is a one-time slot for one image: the browser sends the file to
// UploadURL; aether keeps Key.
type Upload struct {
	Key       string
	UploadURL string
}

// ImageStore hosts product images. The browser uploads straight to it.
type ImageStore interface {
	// Provider names the store on each media row.
	Provider() string
	BeginUpload(ctx context.Context, siteID uuid.UUID) (Upload, error)
	// PublicURL is where the image is served.
	PublicURL(key string) string
}
