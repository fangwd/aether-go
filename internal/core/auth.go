package core

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/time/rate"

	"aether/internal/core/oapi"
	"aether/internal/db"
)

// Staff is a person acting in a site's admin through a partner service.
type Staff struct {
	// Subject is how they appear in order history, as the partner names them
	// (e.g. provider:<user id>).
	Subject string
}

const maxSubjectLen = 200

func bearer(ctx context.Context) string {
	r := requestFrom(ctx)
	if r == nil {
		return ""
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || token == "" {
		return ""
	}
	return token
}

// staff admits a partner service by one of its keys. The partner has
// already decided the person may manage the site; it names them in the
// Aether-Actor header.
func (s *Server) staff(ctx context.Context) (Staff, error) {
	if !s.isPartner(bearer(ctx)) {
		return Staff{}, errUnauthorized
	}
	subject := strings.TrimSpace(requestFrom(ctx).Header.Get("Aether-Actor"))
	if subject == "" || len(subject) > maxSubjectLen {
		return Staff{}, badRequest("Aether-Actor must name who is acting")
	}
	return Staff{Subject: subject}, nil
}

func (s *Server) isPartner(token string) bool {
	if token == "" {
		return false
	}
	got := sha256.Sum256([]byte(token))
	ok := 0
	for _, key := range s.PartnerKeys {
		want := sha256.Sum256([]byte(key))
		ok |= subtle.ConstantTimeCompare(got[:], want[:])
	}
	return ok == 1
}

// sessionDays is how long shopper sessions last, extended as they are used.
const sessionDays = 30

// Shopper is a shopper signed in to their account on one site.
type Shopper struct {
	SiteID        uuid.UUID
	CustomerID    uuid.UUID
	SessionID     uuid.UUID
	Email         string
	EmailVerified bool
}

// shopper is the signed-in shopper, or nil when the request carries no
// bearer; a bearer that is not a live session on this site is a 401.
func (s *Server) shopper(ctx context.Context, siteID uuid.UUID) (*Shopper, error) {
	token := bearer(ctx)
	if token == "" {
		return nil, nil
	}
	var sh *Shopper
	err := db.InSite(ctx, s.Pool, siteID, func(_ pgx.Tx, q *db.Queries) error {
		row, err := q.FindSession(ctx, db.FindSessionParams{TokenHash: hashToken(token), Days: sessionDays})
		if errors.Is(err, pgx.ErrNoRows) {
			return errUnauthorized
		}
		if err != nil {
			return err
		}
		// Sliding expiry, written at most once a day per session.
		if row.Renew {
			if err := q.RenewSession(ctx, db.RenewSessionParams{ID: row.ID, Days: sessionDays}); err != nil {
				return err
			}
		}
		sh = &Shopper{SiteID: siteID, CustomerID: row.CustomerID, SessionID: row.ID, Email: row.Email, EmailVerified: row.Verified}
		return nil
	})
	return sh, err
}

func (s *Server) requireShopper(ctx context.Context, siteID uuid.UUID) (*Shopper, error) {
	sh, err := s.shopper(ctx, siteID)
	if err == nil && sh == nil {
		err = errUnauthorized
	}
	return sh, err
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// guessable operations take a password or a code; each client IP gets a
// burst of 10, then one every 2 seconds.
var guessable = map[string]bool{
	"SignUp": true, "SignIn": true, "RequestVerification": true,
	"ConfirmVerification": true, "RequestReset": true, "ConfirmReset": true,
}

type rateLimiter struct {
	mu      sync.Mutex
	clients map[string]*rate.Limiter
}

func newRateLimiter() *rateLimiter { return &rateLimiter{clients: map[string]*rate.Limiter{}} }

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.clients[key]
	if !ok {
		lim = rate.NewLimiter(rate.Every(2*time.Second), 10)
		l.clients[key] = lim
	}
	return lim.Allow()
}

// clientIP is the first X-Forwarded-For address (set by the ingress), else
// the peer address.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		first, _, _ := strings.Cut(fwd, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limitGuessing applies the per-IP limit to guessable operations.
func (s *Server) limitGuessing(next oapi.StrictHandlerFunc, operationID string) oapi.StrictHandlerFunc {
	if !guessable[operationID] {
		return next
	}
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		if !s.guesses.allow(clientIP(r)) {
			return nil, &APIError{Status: http.StatusTooManyRequests}
		}
		return next(ctx, w, r, request)
	}
}
