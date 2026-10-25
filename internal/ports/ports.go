// Package ports declares what aether needs from outside services. The
// commerce code knows nothing about the vendors behind them (payments, image
// hosting, email, staff identity); the binary plugs concrete providers in.
package ports

import "fmt"

// ErrorKind says whether a provider refused a request or could not answer.
type ErrorKind int

const (
	// Rejected: the provider refused the request (bad input, wrong state).
	// The message is for logs; callers get a generic answer.
	Rejected ErrorKind = iota
	// Unavailable: the provider could not be reached or failed; worth retrying.
	Unavailable
)

// ProviderError is a provider call that did not succeed.
type ProviderError struct {
	Kind    ErrorKind
	Message string
}

func (e *ProviderError) Error() string {
	if e.Kind == Unavailable {
		return "provider unavailable: " + e.Message
	}
	return e.Message
}

func Reject(format string, args ...any) error {
	return &ProviderError{Kind: Rejected, Message: fmt.Sprintf(format, args...)}
}

func Unavailablef(format string, args ...any) error {
	return &ProviderError{Kind: Unavailable, Message: fmt.Sprintf(format, args...)}
}
