// Package webhook delivers aether's notices to a partner as signed HTTP POSTs.
//
// The body is JSON, e.g. {"type":"catalog.changed","site_id":"…"}. The
// Aether-Signature header is "t=<unix seconds>,v1=<hex HMAC-SHA256 of
// "<t>.<body>" under the shared secret>", so the receiver can check both who
// sent it and that it is fresh.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"aether/internal/ports"
)

type Hook struct {
	http   *http.Client
	url    string
	secret []byte
	now    func() time.Time
}

func New(client *http.Client, url, secret string) *Hook {
	return &Hook{http: client, url: url, secret: []byte(secret), now: time.Now}
}

// Sign is the Aether-Signature value for body sent at t.
func Sign(secret []byte, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (h *Hook) CatalogChanged(ctx context.Context, siteID uuid.UUID) error {
	body, err := json.Marshal(map[string]string{"type": "catalog.changed", "site_id": siteID.String()})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Aether-Signature", Sign(h.secret, h.now(), body))
	res, err := h.http.Do(req)
	if err != nil {
		return ports.Unavailablef("catalog webhook: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return ports.Unavailablef("catalog webhook: %s", res.Status)
	}
	return nil
}

var _ ports.CatalogHook = (*Hook)(nil)
