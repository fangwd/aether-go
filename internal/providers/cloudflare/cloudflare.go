// Package cloudflare is aether's image store on Cloudflare Images: one-time
// direct-creator uploads from the browser; images are public (unsigned) so
// published sites can show them.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"aether/internal/ports"
)

const provider = "cloudflare_images"

type Config struct {
	// APIURL is e.g. https://api.cloudflare.com.
	APIURL    string
	AccountID string
	APIToken  string
	// DeliveryURL is e.g. https://imagedelivery.net/<account hash>.
	DeliveryURL string
	// Variant is the account's delivery variant images are served at, e.g. public.
	Variant string
	// Environment is recorded in image metadata to tell environments apart.
	Environment string
}

type Images struct {
	http   *http.Client
	config Config
}

var _ ports.ImageStore = (*Images)(nil)

func New(client *http.Client, config Config) *Images {
	return &Images{http: client, config: config}
}

func (*Images) Provider() string { return provider }

type response struct {
	Success bool `json:"success"`
	Result  *struct {
		ID        string `json:"id"`
		UploadURL string `json:"uploadURL"`
	} `json:"result"`
	Errors []json.RawMessage `json:"errors"`
}

func (c *Images) BeginUpload(ctx context.Context, siteID uuid.UUID) (ports.Upload, error) {
	url := fmt.Sprintf("%s/client/v4/accounts/%s/images/v2/direct_upload",
		strings.TrimRight(c.config.APIURL, "/"), c.config.AccountID)
	metadata, err := json.Marshal(map[string]string{"env": c.config.Environment, "site_id": siteID.String()})
	if err != nil {
		return ports.Upload{}, err
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("requireSignedURLs", "false")
	_ = form.WriteField("metadata", string(metadata))
	if err := form.Close(); err != nil {
		return ports.Upload{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return ports.Upload{}, ports.Unavailablef("cloudflare: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.config.APIToken)
	req.Header.Set("Content-Type", form.FormDataContentType())
	res, err := c.http.Do(req)
	if err != nil {
		return ports.Upload{}, ports.Unavailablef("cloudflare: %v", err)
	}
	defer res.Body.Close()

	var out response
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return ports.Upload{}, ports.Unavailablef("cloudflare %s: %v", res.Status, err)
	}
	if !out.Success || out.Result == nil {
		errs, _ := json.Marshal(out.Errors)
		return ports.Upload{}, ports.Unavailablef("cloudflare %s: %s", res.Status, errs)
	}
	return ports.Upload{Key: out.Result.ID, UploadURL: out.Result.UploadURL}, nil
}

func (c *Images) PublicURL(key string) string {
	return fmt.Sprintf("%s/%s/%s", strings.TrimRight(c.config.DeliveryURL, "/"), key, c.config.Variant)
}
