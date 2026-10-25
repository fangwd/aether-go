package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/alexedwards/argon2id"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"aether/internal/core/oapi"
	"aether/internal/db"
)

// Shopper accounts on a site's storefront: sign-up, sessions, email
// verification and password reset by emailed code. An account is a
// customer with a password; accounts on one site know nothing of others.

const (
	codeTTLMinutes  = 15
	maxCodeAttempts = 5
	verifyEmail     = "verify_email"
	resetPassword   = "reset_password"
	minPassword     = 8
	maxPassword     = 128
)

// dummyHash is checked against when an email is unknown, so a sign-in
// takes as long whether or not the account exists.
var dummyHash, _ = argon2id.CreateHash("not-a-real-password", argon2id.DefaultParams)

func checkPassword(password string) error {
	if n := utf8.RuneCountInString(password); n < minPassword || n > maxPassword {
		return unprocessable("password must be %d to %d characters", minPassword, maxPassword)
	}
	return nil
}

func passwordMatches(password, hash string) bool {
	ok, err := argon2id.ComparePasswordAndHash(password, hash)
	return err == nil && ok
}

// randomCode is a uniformly random six-digit code.
func randomCode() string {
	limit := uint32(1<<32 - 1)
	limit -= limit % 1_000_000 // reject the tail that would bias % 1_000_000
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		if n := binary.LittleEndian.Uint32(b[:]); n < limit {
			return fmt.Sprintf("%06d", n%1_000_000)
		}
	}
}

func startSession(ctx context.Context, q *db.Queries, siteID uuid.UUID, account oapi.Account) (oapi.Session, error) {
	token := newToken()
	expires, err := q.InsertSession(ctx, db.InsertSessionParams{SiteID: siteID, ID: newID(), CustomerID: account.Id, TokenHash: hashToken(token), Days: sessionDays})
	return oapi.Session{SessionToken: token, ExpiresAt: expires, Account: account}, err
}

// sendCode replaces any open code for this purpose with a new one and
// queues the email that carries it.
func sendCode(ctx context.Context, q *db.Queries, siteID, customerID uuid.UUID, email, purpose string) error {
	if err := q.RetireCodes(ctx, db.RetireCodesParams{CustomerID: customerID, Purpose: purpose}); err != nil {
		return err
	}
	code := randomCode()
	if err := q.InsertCode(ctx, db.InsertCodeParams{SiteID: siteID, ID: newID(), CustomerID: customerID, Purpose: purpose,
		CodeHash: hashToken(code), Minutes: codeTTLMinutes}); err != nil {
		return err
	}
	return emit(ctx, q, siteID, "account.code", customerID, map[string]any{"purpose": purpose, "to": email, "code": code})
}

var errInvalidCode = unprocessable("the code is wrong or has expired; request a new one")

// checkCode uses up a matching open code. A wrong code counts an attempt,
// which the caller must commit even though it answers an error.
func checkCode(ctx context.Context, q *db.Queries, customerID uuid.UUID, purpose, code string) (valid, counted bool, err error) {
	open, err := q.OpenCode(ctx, db.OpenCodeParams{CustomerID: customerID, Purpose: purpose})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil || open.Attempts >= maxCodeAttempts {
		return false, false, err
	}
	if subtle.ConstantTimeCompare(open.CodeHash, hashToken(strings.TrimSpace(code))) != 1 {
		return false, true, q.CountCodeAttempt(ctx, open.ID)
	}
	return true, false, q.UseCode(ctx, open.ID)
}

// verified marks the account's email verified and takes over the guest
// profile (orders, addresses, promotions) that used it.
func verified(ctx context.Context, q *db.Queries, customerID uuid.UUID, email string) error {
	if err := q.MarkEmailVerified(ctx, customerID); err != nil {
		return err
	}
	guest, err := q.GuestProfileForUpdate(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	move := db.MoveOrdersParams{Account: &customerID, Guest: &guest}
	for _, f := range []func(context.Context) error{
		func(ctx context.Context) error { return q.MoveOrders(ctx, move) },
		func(ctx context.Context) error {
			return q.MoveAddresses(ctx, db.MoveAddressesParams{Account: customerID, Guest: guest})
		},
		func(ctx context.Context) error { return q.MoveCoupons(ctx, db.MoveCouponsParams(move)) },
		func(ctx context.Context) error { return q.MoveRedemptions(ctx, db.MoveRedemptionsParams(move)) },
		func(ctx context.Context) error { return q.DeleteCustomer(ctx, guest) },
	} {
		if err := f(ctx); err != nil {
			return err
		}
	}
	return nil
}

// SignUp creates an account and signs it in; a verification code is
// emailed. Orders placed as a guest with this email join the account once
// the email is verified.
func (s *Server) SignUp(ctx context.Context, req oapi.SignUpRequestObject) (oapi.SignUpResponseObject, error) {
	email, err := normalizeEmail(req.Body.Email)
	if err != nil {
		return nil, err
	}
	if err := checkPassword(req.Body.Password); err != nil {
		return nil, err
	}
	hash, err := argon2id.CreateHash(req.Body.Password, argon2id.DefaultParams)
	if err != nil {
		return nil, err
	}
	var session oapi.Session
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := loadSite(ctx, q); err != nil {
			return err
		}
		id, err := q.InsertAccount(ctx, db.InsertAccountParams{SiteID: req.SiteId, ID: newID(), Email: &email, PasswordHash: &hash})
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict("an account with this email already exists; sign in instead")
		}
		if err != nil {
			return err
		}
		if err := sendCode(ctx, q, req.SiteId, id, email, verifyEmail); err != nil {
			return err
		}
		session, err = startSession(ctx, q, req.SiteId, oapi.Account{Id: id, Email: email})
		return err
	})
	if err != nil {
		return nil, err
	}
	slog.Info("account created", "site_id", req.SiteId, "customer_id", session.Account.Id)
	return oapi.SignUp201JSONResponse(session), nil
}

// Me is the signed-in account.
func (s *Server) Me(ctx context.Context, req oapi.MeRequestObject) (oapi.MeResponseObject, error) {
	sh, err := s.requireShopper(ctx, req.SiteId)
	if err != nil {
		return nil, err
	}
	return oapi.Me200JSONResponse{Id: sh.CustomerID, Email: sh.Email, EmailVerified: sh.EmailVerified}, nil
}

func (s *Server) SignIn(ctx context.Context, req oapi.SignInRequestObject) (oapi.SignInResponseObject, error) {
	email := strings.ToLower(strings.TrimSpace(req.Body.Email))
	var account db.AccountByEmailRow
	found := true
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := loadSite(ctx, q); err != nil {
			return err
		}
		var err error
		account, err = q.AccountByEmail(ctx, email)
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	// Unknown emails take as long as wrong passwords.
	hash := account.PasswordHash
	if !found {
		hash = dummyHash
	}
	if !passwordMatches(req.Body.Password, hash) || !found {
		return nil, errUnauthorized
	}
	var session oapi.Session
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		session, err = startSession(ctx, q, req.SiteId, oapi.Account{Id: account.ID, Email: email, EmailVerified: account.Verified})
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.SignIn200JSONResponse(session), nil
}

// SignOut ends the session the request is signed in with.
func (s *Server) SignOut(ctx context.Context, req oapi.SignOutRequestObject) (oapi.SignOutResponseObject, error) {
	sh, err := s.requireShopper(ctx, req.SiteId)
	if err != nil {
		return nil, err
	}
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		return q.DeleteSession(ctx, sh.SessionID)
	})
	if err != nil {
		return nil, err
	}
	return oapi.SignOut204Response{}, nil
}

// RequestVerification emails a new verification code. Always 202, so it
// reveals nothing about which emails have accounts.
func (s *Server) RequestVerification(ctx context.Context, req oapi.RequestVerificationRequestObject) (oapi.RequestVerificationResponseObject, error) {
	email := strings.ToLower(strings.TrimSpace(req.Body.Email))
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		id, err := q.UnverifiedAccountByEmail(ctx, email)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return sendCode(ctx, q, req.SiteId, id, email, verifyEmail)
	})
	if err != nil {
		return nil, err
	}
	return oapi.RequestVerification202Response{}, nil
}

// confirmCode runs fn when code is a valid open code of purpose for the
// account with email. Wrong attempts are counted even though the answer is
// an error.
func (s *Server) confirmCode(ctx context.Context, siteID uuid.UUID, email, purpose, code string, fn func(q *db.Queries, customerID uuid.UUID) error) error {
	wrong := false
	err := db.InSite(ctx, s.Pool, siteID, func(_ pgx.Tx, q *db.Queries) error {
		account, err := q.AccountByEmail(ctx, email)
		if errors.Is(err, pgx.ErrNoRows) {
			return errInvalidCode
		}
		if err != nil {
			return err
		}
		valid, counted, err := checkCode(ctx, q, account.ID, purpose, code)
		if err != nil {
			return err
		}
		if !valid {
			wrong = counted
			if !counted {
				return errInvalidCode
			}
			return nil // commit the counted attempt
		}
		return fn(q, account.ID)
	})
	if err == nil && wrong {
		return errInvalidCode
	}
	return err
}

func (s *Server) ConfirmVerification(ctx context.Context, req oapi.ConfirmVerificationRequestObject) (oapi.ConfirmVerificationResponseObject, error) {
	email := strings.ToLower(strings.TrimSpace(req.Body.Email))
	err := s.confirmCode(ctx, req.SiteId, email, verifyEmail, req.Body.Code, func(q *db.Queries, id uuid.UUID) error {
		return verified(ctx, q, id, email)
	})
	if err != nil {
		return nil, err
	}
	return oapi.ConfirmVerification204Response{}, nil
}

// RequestReset emails a password reset code. Always 202.
func (s *Server) RequestReset(ctx context.Context, req oapi.RequestResetRequestObject) (oapi.RequestResetResponseObject, error) {
	email := strings.ToLower(strings.TrimSpace(req.Body.Email))
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		account, err := q.AccountByEmail(ctx, email)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return sendCode(ctx, q, req.SiteId, account.ID, email, resetPassword)
	})
	if err != nil {
		return nil, err
	}
	return oapi.RequestReset202Response{}, nil
}

// ConfirmReset sets a new password and ends every session. Receiving the
// code proves the email address too, so it is marked verified.
func (s *Server) ConfirmReset(ctx context.Context, req oapi.ConfirmResetRequestObject) (oapi.ConfirmResetResponseObject, error) {
	if err := checkPassword(req.Body.NewPassword); err != nil {
		return nil, err
	}
	hash, err := argon2id.CreateHash(req.Body.NewPassword, argon2id.DefaultParams)
	if err != nil {
		return nil, err
	}
	email := strings.ToLower(strings.TrimSpace(req.Body.Email))
	err = s.confirmCode(ctx, req.SiteId, email, resetPassword, req.Body.Code, func(q *db.Queries, id uuid.UUID) error {
		if err := q.SetPassword(ctx, db.SetPasswordParams{ID: id, PasswordHash: &hash}); err != nil {
			return err
		}
		if err := q.DeleteSessionsOf(ctx, id); err != nil {
			return err
		}
		return verified(ctx, q, id, email)
	})
	if err != nil {
		return nil, err
	}
	return oapi.ConfirmReset204Response{}, nil
}
