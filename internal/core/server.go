// Package core is the commerce core: the API described by openapi.yaml and
// the background worker. It knows no vendor, only the ports.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	middleware "github.com/oapi-codegen/nethttp-middleware"

	"aether"
	"aether/internal/core/oapi"
	"aether/internal/ports"
)

const (
	requestTimeout = 30 * time.Second
	maxBodyBytes   = 1 << 20
)

// Deps are the outside services the API uses. Images and Payments may be
// nil: their routes then answer 503.
type Deps struct {
	Pool *pgxpool.Pool
	// PartnerKeys admit partner services to the admin routes; more
	// than one lets a key be rotated.
	PartnerKeys []string
	Mailer      ports.Mailer
	Images      ports.ImageStore
	Payments    ports.PaymentGateway
}

// Server implements every operation of the contract.
type Server struct {
	Deps
	guesses *rateLimiter
}

var _ oapi.StrictServerInterface = (*Server)(nil)

func New(deps Deps) *Server {
	return &Server{Deps: deps, guesses: newRateLimiter()}
}

// Handler is the whole HTTP surface: the API, the payment webhook, health
// checks and the spec.
func (s *Server) Handler() (http.Handler, error) {
	spec, err := openapi3.NewLoader().LoadFromData(aether.Spec)
	if err != nil {
		return nil, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	spec.Servers = nil // match requests on path alone

	strict := oapi.NewStrictHandlerWithOptions(s, []oapi.StrictMiddlewareFunc{s.limitGuessing}, oapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  func(w http.ResponseWriter, _ *http.Request, err error) { writeProblem(w, badRequest("%s", err)) },
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) { writeProblem(w, err) },
	})
	api := http.NewServeMux()
	oapi.HandlerWithOptions(strict, oapi.StdHTTPServerOptions{
		BaseRouter:       api,
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) { writeProblem(w, badRequest("%s", err)) },
	})
	validate := middleware.OapiRequestValidatorWithOptions(spec, &middleware.Options{
		Options:               openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		SilenceServersWarning: true,
		ErrorHandlerWithOpts: func(_ context.Context, err error, w http.ResponseWriter, _ *http.Request, _ middleware.ErrorHandlerOpts) {
			writeProblem(w, validationError(err))
		},
	})

	mux := http.NewServeMux()
	mux.Handle("/v1/", validate(api))
	mux.HandleFunc("POST /v1/webhooks/payments/{provider}", s.paymentWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(specJSON)
	})
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(docsPage))
	})

	var h http.Handler = mux
	h = withRequest(h)
	h = http.TimeoutHandler(h, requestTimeout, "")
	h = withLogging(h)
	h = withRequestID(h)
	return h, nil
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.Pool.Ping(r.Context()); err != nil {
		slog.Warn("database not ready", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// validationError: a body that breaks the schema is 422, anything else
// (unparseable JSON, a bad parameter) is 400.
func validationError(err error) error {
	var reqErr *openapi3filter.RequestError
	if errors.As(err, &reqErr) && reqErr.Parameter == nil {
		var schemaErr *openapi3.SchemaError
		if errors.As(reqErr.Err, &schemaErr) {
			return unprocessable("%s", schemaErr.Reason)
		}
	}
	return badRequest("%s", err)
}

type requestKey struct{}

// withRequest makes the incoming request (headers, client address, raw
// body) available to handlers, which the generated interface otherwise
// hides.
func withRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeProblem(w, badRequest("request body too large or unreadable"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		ctx := context.WithValue(r.Context(), requestKey{}, r)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, bodyKey{}, body)))
	})
}

type bodyKey struct{}

// sent reports which top-level fields the request body has, so PATCH can
// tell an absent field (unchanged) from null (cleared).
func sent(ctx context.Context) map[string]bool {
	body, _ := ctx.Value(bodyKey{}).([]byte)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	out := make(map[string]bool, len(fields))
	for k := range fields {
		out[k] = true
	}
	return out
}

func requestFrom(ctx context.Context) *http.Request {
	r, _ := ctx.Value(requestKey{}).(*http.Request)
	return r
}

const requestIDHeader = "X-Request-Id"

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = uuid.NewString()
			r.Header.Set(requestIDHeader, id)
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"latency_ms", time.Since(start).Milliseconds(), "request_id", r.Header.Get(requestIDHeader))
	})
}

const docsPage = `<!doctype html>
<html><head><title>aether API</title><meta charset="utf-8"></head>
<body><script id="api-reference" data-url="/openapi.json"></script>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script></body></html>`
