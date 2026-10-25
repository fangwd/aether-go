package apitest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"aether/internal/core"
	"aether/internal/db"
)

// PartnerKey is the key the tests' partner service presents, and
// Actor whom it names as acting.
const (
	PartnerKey = "test-partner-key-0123456789abcdef"
	Actor      = "provider:1"
)

// API is one test's running API with fakes for every outside service.
type API struct {
	T testing.TB
	Databases
	Server  *core.Server
	Handler http.Handler
	Mailer  *RecordingMailer
	// clientIP is sent as X-Forwarded-For, so each test has its own
	// rate-limit bucket.
	clientIP string
}

// New starts the API on a fresh database. Images and payments are off
// (nil) unless a test sets them on Server.Deps before calling Rebuild.
func New(t testing.TB) *API {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	dbs := NewDatabases(t)
	mailer := &RecordingMailer{}
	srv := core.New(core.Deps{Pool: dbs.App, PartnerKeys: []string{PartnerKey}, Mailer: mailer})
	id := uuid.New()
	api := &API{T: t, Databases: dbs, Server: srv, Mailer: mailer, clientIP: fmt.Sprintf("10.0.%d.%d", id[14], id[15])}
	api.Rebuild()
	return api
}

// Rebuild rebuilds the handler after a test changes Server.Deps.
func (a *API) Rebuild() {
	h, err := a.Server.Handler()
	if err != nil {
		a.T.Fatal(err)
	}
	a.Handler = h
}

// J is a decoded JSON value with path lookups.
type J struct{ V any }

// Get walks object keys (string) and array indexes (int).
func (j J) Get(path ...any) J {
	v := j.V
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			arr, _ := v.([]any)
			if k < 0 {
				k += len(arr)
			}
			if k < 0 || k >= len(arr) {
				return J{}
			}
			v = arr[k]
		}
	}
	return J{V: v}
}

func (j J) Str() string {
	s, _ := j.V.(string)
	return s
}

func (j J) Num() float64 {
	n, _ := j.V.(float64)
	return n
}

func (j J) Bool() bool {
	b, _ := j.V.(bool)
	return b
}

func (j J) Len() int {
	arr, _ := j.V.([]any)
	return len(arr)
}

func (j J) Arr() []J {
	arr, _ := j.V.([]any)
	out := make([]J, len(arr))
	for i, v := range arr {
		out[i] = J{V: v}
	}
	return out
}

// Find is the first array element whose field equals value.
func (j J) Find(field string, value any) J {
	for _, e := range j.Arr() {
		if e.Get(field).V == value {
			return e
		}
	}
	return J{}
}

func (j J) String() string {
	b, _ := json.Marshal(j.V)
	return string(b)
}

// Request builds a request; headers alternate name, value. Every request
// names Actor in Aether-Actor, as the partner does.
func (a *API) Request(method, path string, body any, headers ...string) *http.Request {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			a.T.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Forwarded-For", a.clientIP)
	req.Header.Set("Aether-Actor", Actor)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return req
}

// Send runs a request and decodes its JSON body.
func (a *API) Send(req *http.Request) (int, J) {
	rec := httptest.NewRecorder()
	a.Handler.ServeHTTP(rec, req)
	var v any
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return rec.Code, J{V: v}
}

// Call sends method + path with an optional bearer and JSON body.
func (a *API) Call(method, path, bearer string, body any) (int, J) {
	var headers []string
	if bearer != "" {
		headers = []string{"Authorization", "Bearer " + bearer}
	}
	return a.Send(a.Request(method, path, body, headers...))
}

func (a *API) Get(path, bearer string) (int, J) { return a.Call(http.MethodGet, path, bearer, nil) }
func (a *API) Post(path, bearer string, body any) (int, J) {
	return a.Call(http.MethodPost, path, bearer, body)
}
func (a *API) Put(path, bearer string, body any) (int, J) {
	return a.Call(http.MethodPut, path, bearer, body)
}
func (a *API) Patch(path, bearer string, body any) (int, J) {
	return a.Call(http.MethodPatch, path, bearer, body)
}
func (a *API) Delete(path, bearer string) (int, J) {
	return a.Call(http.MethodDelete, path, bearer, nil)
}

// Want fails the test unless status is want.
func (a *API) Want(status, want int, body J) {
	a.T.Helper()
	if status != want {
		a.T.Fatalf("status %d, want %d: %s", status, want, body)
	}
}

// M is a JSON object literal.
type M = map[string]any

// Shop is a site with commerce turned on, in USD; Key is the partner key
// that manages it.
type Shop struct {
	api *API
	ID  uuid.UUID
	Key string
}

func (a *API) OpenShop() Shop {
	a.T.Helper()
	id, _ := uuid.NewV7()
	status, body := a.Put(fmt.Sprintf("/v1/sites/%s", id), PartnerKey, M{"name": "Shop", "currency": "USD"})
	a.Want(status, http.StatusOK, body)
	return Shop{api: a, ID: id, Key: PartnerKey}
}

func (s Shop) Admin(path string) string { return fmt.Sprintf("/v1/sites/%s/admin%s", s.ID, path) }
func (s Shop) Store(path string) string { return fmt.Sprintf("/v1/sites/%s%s", s.ID, path) }

// PadThai is the product used across tests: base 15.00, Large +4.00, a
// Spice add-on (one of mild/spicy +2.00) and Extras (up to two).
func (s Shop) PadThai() J {
	s.api.T.Helper()
	status, product := s.api.Post(s.Admin("/products"), s.Key, M{
		"slug": "pad-thai", "name": "Pad Thai", "price": "15.00", "status": "active",
		"variant_axes": []string{"Size"},
		"variants": []M{
			{"sku": "PT-R", "attributes": M{"Size": "Regular"}},
			{"sku": "PT-L", "attributes": M{"Size": "Large"}, "price_extra": "4.00"},
		},
		"option_groups": []M{
			{"code": "spice", "name": "Spice", "min_select": 1, "max_select": 1, "values": []M{
				{"code": "mild", "label": "Mild", "is_default": true},
				{"code": "spicy", "label": "Spicy", "price_extra": "2.00"},
			}},
			{"code": "extras", "name": "Extras", "max_select": 2, "values": []M{
				{"code": "egg", "label": "Fried egg", "price_extra": "1.50"},
				{"code": "peanuts", "label": "Peanuts", "price_extra": "0.50"},
				{"code": "lime", "label": "Lime"},
			}},
		},
	})
	s.api.Want(status, http.StatusCreated, product)
	return product
}

// VariantID is the id of the variant whose Size is size.
func VariantID(product J, size string) string {
	for _, v := range product.Get("variants").Arr() {
		if v.Get("attributes", "Size").Str() == size {
			return v.Get("id").Str()
		}
	}
	panic("no variant " + size)
}

// OptionID is the id of an option value by group and value code.
func OptionID(product J, group, value string) string {
	return product.Get("option_groups").Find("code", group).Get("values").Find("code", value).Get("id").Str()
}

// UnverifiedShopper signs up a shopper account and returns its session token.
func (s Shop) UnverifiedShopper(email string) string {
	s.api.T.Helper()
	status, session := s.api.Post(s.Store("/account"), "", M{"email": email, "password": "correct horse"})
	s.api.Want(status, http.StatusCreated, session)
	return session.Get("session_token").Str()
}

// Shopper is a shopper account whose email is verified with the emailed code.
func (s Shop) Shopper(email string) string {
	s.api.T.Helper()
	token := s.UnverifiedShopper(email)
	status, body := s.api.Post(s.Store("/account/email-verification/confirm"), "", M{"email": email, "code": s.EmailedCode(email)})
	s.api.Want(status, http.StatusNoContent, body)
	return token
}

// EmailedCode is the latest account code queued for this address.
func (s Shop) EmailedCode(email string) string {
	s.api.T.Helper()
	var code string
	err := db.InSite(context.Background(), s.api.App, s.ID, func(tx pgx.Tx, _ *db.Queries) error {
		return tx.QueryRow(context.Background(),
			`SELECT payload->>'code' FROM outbox_event WHERE topic = 'account.code' AND payload->>'to' = $1
			  ORDER BY created_at DESC LIMIT 1`, email).Scan(&code)
	})
	if err != nil {
		s.api.T.Fatal(err)
	}
	return code
}
