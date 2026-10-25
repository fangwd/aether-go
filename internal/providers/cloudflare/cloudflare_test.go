package cloudflare_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"aether/internal/ports"
	"aether/internal/providers/cloudflare"
)

func store(server *httptest.Server) *cloudflare.Images {
	return cloudflare.New(server.Client(), cloudflare.Config{
		APIURL:      server.URL,
		AccountID:   "acct",
		APIToken:    "token",
		DeliveryURL: "https://imagedelivery.net/hash",
		Variant:     "1280",
		Environment: "test",
	})
}

func TestUploadsGoStraightToCloudflare(t *testing.T) {
	siteID := uuid.Must(uuid.NewV7())
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/client/v4/accounts/acct/images/v2/direct_upload" ||
			r.Header.Get("Authorization") != "Bearer token" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var meta map[string]string
		if r.FormValue("requireSignedURLs") != "false" ||
			json.Unmarshal([]byte(r.FormValue("metadata")), &meta) != nil ||
			meta["env"] != "test" || meta["site_id"] != siteID.String() {
			t.Errorf("unexpected form: %v", r.MultipartForm)
		}
		_, _ = w.Write([]byte(`{"success": true, "errors": [],
			"result": {"id": "img-1", "uploadURL": "https://upload.imagedelivery.net/one-time"}}`))
	}))
	defer server.Close()
	images := store(server)

	upload, err := images.BeginUpload(context.Background(), siteID)
	if err != nil {
		t.Fatal(err)
	}

	want := ports.Upload{Key: "img-1", UploadURL: "https://upload.imagedelivery.net/one-time"}
	if upload != want {
		t.Errorf("upload = %+v, want %+v", upload, want)
	}
	if got := images.PublicURL("img-1"); got != "https://imagedelivery.net/hash/img-1/1280" {
		t.Errorf("public url = %q", got)
	}
	if got := images.Provider(); got != "cloudflare_images" {
		t.Errorf("provider = %q", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
}

func TestACloudflareFailureIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"success": false, "errors": [{"code": 5400}]}`))
	}))
	defer server.Close()

	_, err := store(server).BeginUpload(context.Background(), uuid.Must(uuid.NewV7()))

	var perr *ports.ProviderError
	if !errors.As(err, &perr) || perr.Kind != ports.Unavailable {
		t.Fatalf("err = %v, want unavailable", err)
	}
}
