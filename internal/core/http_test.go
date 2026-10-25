package core_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"aether/internal/apitest"
)

func TestHealthAndSpec(t *testing.T) {
	api := apitest.New(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		if status, _ := api.Get(path, ""); status != http.StatusOK {
			t.Errorf("%s: %d", path, status)
		}
	}
	status, spec := api.Get("/openapi.json", "")
	api.Want(status, http.StatusOK, spec)
	if spec.Get("paths", "/v1/sites/{site_id}/admin/products", "post").V == nil {
		t.Error("the spec lacks product creation")
	}
	if spec.Get("components", "securitySchemes", "bearer").V == nil {
		t.Error("the spec lacks the bearer scheme")
	}
}

func TestHealthzAnswersWithARequestID(t *testing.T) {
	api := apitest.New(t)
	rec := httptest.NewRecorder()

	api.Handler.ServeHTTP(rec, api.Request(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status %d", rec.Code)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Errorf("no request id: %v", rec.Header())
	}
}

func TestSiteSettings(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()

	status, site := api.Get(shop.Store(""), "")
	api.Want(status, http.StatusOK, site)
	if site.Get("currency").Str() != "USD" || site.Get("timezone").Str() != "UTC" {
		t.Errorf("site: %s", site)
	}
	if status, body := api.Put(shop.Store(""), shop.Key, apitest.M{"name": "Shop", "currency": "usd"}); status != http.StatusUnprocessableEntity {
		t.Errorf("lowercase currency: %d %s", status, body)
	}
	if status, _ := api.Put(shop.Store(""), shop.Key, apitest.M{"name": "Shop", "currency": "USD", "timezone": "Mars/Base"}); status != http.StatusUnprocessableEntity {
		t.Errorf("unknown timezone: %d", status)
	}
	if status, _ := api.Get("/v1/sites/"+uuid.NewString(), ""); status != http.StatusNotFound {
		t.Errorf("a site without commerce: %d", status)
	}
}
