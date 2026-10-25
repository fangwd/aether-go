package webhook_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"aether/internal/providers/webhook"
)

func TestCatalogChangedIsSigned(t *testing.T) {
	site := uuid.Must(uuid.NewV7())
	var signature, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, signature = string(b), r.Header.Get("Aether-Signature")
	}))
	defer server.Close()

	if err := webhook.New(server.Client(), server.URL, "secret").CatalogChanged(context.Background(), site); err != nil {
		t.Fatal(err)
	}
	if want := `{"site_id":"` + site.String() + `","type":"catalog.changed"}`; body != want {
		t.Errorf("body = %s", body)
	}
	ts, err := strconv.ParseInt(strings.TrimPrefix(strings.Split(signature, ",")[0], "t="), 10, 64)
	if err != nil {
		t.Fatalf("signature %q has no time", signature)
	}
	if signature != webhook.Sign([]byte("secret"), time.Unix(ts, 0), []byte(body)) {
		t.Errorf("signature %q does not verify", signature)
	}
}

func TestCatalogChangedFailureIsRetried(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	err := webhook.New(server.Client(), server.URL, "secret").CatalogChanged(context.Background(), uuid.New())
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("err = %v, want unavailable", err)
	}
}
