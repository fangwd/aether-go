package core

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"aether/internal/core/oapi"
	"aether/internal/db"
)

// CreateUpload starts an image upload: the browser sends the file straight
// to the image store, and products refer to the returned media_id.
func (s *Server) CreateUpload(ctx context.Context, req oapi.CreateUploadRequestObject) (oapi.CreateUploadResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(req.Body.MimeType, "image/") {
		return nil, unprocessable("mime_type must be an image type")
	}
	if s.Images == nil {
		return nil, errUnavailable
	}
	upload, err := s.Images.BeginUpload(ctx, req.SiteId)
	if err != nil {
		return nil, err
	}
	id := newID()
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		return q.InsertMedia(ctx, db.InsertMediaParams{
			SiteID: req.SiteId, ID: id, StorageProvider: s.Images.Provider(), StorageKey: upload.Key,
			MimeType: req.Body.MimeType, Alt: req.Body.Alt,
		})
	})
	if err != nil {
		return nil, err
	}
	return oapi.CreateUpload201JSONResponse{MediaId: id, UploadUrl: upload.UploadURL, Url: s.Images.PublicURL(upload.Key)}, nil
}
