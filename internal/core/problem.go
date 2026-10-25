package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"aether/internal/ports"
)

// APIError is an error with an HTTP status, answered as RFC 9457 problem
// details.
type APIError struct {
	Status int
	Detail string
}

func (e *APIError) Error() string { return fmt.Sprintf("%d %s", e.Status, e.Detail) }

var (
	errUnauthorized = &APIError{Status: http.StatusUnauthorized}
	errNotFound     = &APIError{Status: http.StatusNotFound}
	errUnavailable  = &APIError{Status: http.StatusServiceUnavailable}
	errInternal     = &APIError{Status: http.StatusInternalServerError}
)

func conflict(format string, args ...any) error {
	return &APIError{Status: http.StatusConflict, Detail: fmt.Sprintf(format, args...)}
}

func unprocessable(format string, args ...any) error {
	return &APIError{Status: http.StatusUnprocessableEntity, Detail: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) error {
	return &APIError{Status: http.StatusBadRequest, Detail: fmt.Sprintf(format, args...)}
}

var titles = map[int]string{
	http.StatusBadRequest:          "Malformed request",
	http.StatusUnauthorized:        "Invalid or expired credentials",
	http.StatusNotFound:            "Not found",
	http.StatusConflict:            "Conflict",
	http.StatusUnprocessableEntity: "Invalid request",
	http.StatusTooManyRequests:     "Too many requests",
	http.StatusServiceUnavailable:  "Temporarily unavailable",
	http.StatusInternalServerError: "Internal error",
}

const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgCheckViolation      = "23514"
)

// toAPIError maps any error to the answer a client gets. Unknown errors are
// logged and become 500s; provider messages never reach the client.
func toAPIError(err error) *APIError {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			return &APIError{Status: http.StatusConflict, Detail: fmt.Sprintf("already exists (%s)", pgErr.ConstraintName)}
		case pgForeignKeyViolation:
			return &APIError{Status: http.StatusUnprocessableEntity, Detail: fmt.Sprintf("references a record that does not exist (%s)", pgErr.ConstraintName)}
		case pgCheckViolation:
			return &APIError{Status: http.StatusUnprocessableEntity, Detail: fmt.Sprintf("invalid value (%s)", pgErr.ConstraintName)}
		}
	}
	var provErr *ports.ProviderError
	if errors.As(err, &provErr) {
		if provErr.Kind == ports.Rejected {
			slog.Warn("provider refused the request", "reason", provErr.Message)
			return &APIError{Status: http.StatusUnprocessableEntity, Detail: "the provider refused the request"}
		}
		slog.Warn("provider unavailable", "reason", provErr.Message)
		return errUnavailable
	}
	slog.Error("request failed", "error", err)
	return errInternal
}

func writeProblem(w http.ResponseWriter, err error) {
	e := toAPIError(err)
	body := map[string]any{"type": "about:blank", "title": titles[e.Status], "status": e.Status}
	if e.Detail != "" {
		body["detail"] = e.Detail
	}
	w.Header().Set("Content-Type", "application/problem+json")
	if e.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(body)
}
