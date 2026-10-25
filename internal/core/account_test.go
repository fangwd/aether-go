package core_test

import (
	"net/http"
	"reflect"
	"slices"
	"testing"

	"aether/internal/apitest"
)

func accountSignIn(api *apitest.API, shop apitest.Shop, email, password string) (int, apitest.J) {
	return api.Post(shop.Store("/account/sessions"), "", apitest.M{"email": email, "password": password})
}

func TestSignUpSignInAndOut(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	body := apitest.M{"email": " Ann@Example.com ", "password": "correct horse"}

	status, session := api.Post(shop.Store("/account"), "", body)
	api.Want(status, http.StatusCreated, session)
	want := map[string]any{"id": session.Get("account", "id").V, "email": "ann@example.com", "email_verified": false}
	if !reflect.DeepEqual(session.Get("account").V, want) {
		t.Errorf("account %s", session.Get("account"))
	}
	if status, _ := api.Post(shop.Store("/account"), "", body); status != http.StatusConflict {
		t.Errorf("second sign-up: %d", status)
	}
	if status, _ := accountSignIn(api, shop, "ann@example.com", "wrong horse"); status != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", status)
	}
	if status, _ := accountSignIn(api, shop, "nobody@example.com", "correct horse"); status != http.StatusUnauthorized {
		t.Errorf("unknown email: %d", status)
	}

	status, signedIn := accountSignIn(api, shop, "ANN@example.com", "correct horse")
	api.Want(status, http.StatusOK, signedIn)
	token := signedIn.Get("session_token").Str()
	if _, me := api.Get(shop.Store("/account"), token); me.Get("id").V != session.Get("account", "id").V {
		t.Errorf("me %s", me)
	}

	if status, body := api.Delete(shop.Store("/account/sessions/current"), token); status != http.StatusNoContent {
		t.Errorf("sign out: %d %s", status, body)
	}
	if status, _ := api.Get(shop.Store("/account"), token); status != http.StatusUnauthorized {
		t.Errorf("signed-out session: %d", status)
	}
	first := session.Get("session_token").Str()
	if status, _ := api.Get(shop.Store("/account"), first); status != http.StatusOK {
		t.Errorf("other sessions stay signed in: %d", status)
	}
}

func TestAccountsBelongToOneSite(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	other := api.OpenShop()
	shop.UnverifiedShopper("ann@example.com")

	if status, _ := accountSignIn(api, other, "ann@example.com", "correct horse"); status != http.StatusUnauthorized {
		t.Errorf("sign in on another site: %d", status)
	}
	body := apitest.M{"email": "ann@example.com", "password": "another horse"}
	if status, body := api.Post(other.Store("/account"), "", body); status != http.StatusCreated {
		t.Errorf("sign up on another site: %d %s", status, body)
	}
}

func TestVerificationCodesAreSingleUseAndLimited(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	token := shop.UnverifiedShopper("ann@example.com")
	code := shop.EmailedCode("ann@example.com")
	confirm := func(code string) int {
		status, _ := api.Post(shop.Store("/account/email-verification/confirm"), "", apitest.M{"email": "ann@example.com", "code": code})
		return status
	}

	for range 5 {
		if status := confirm("000000"); status != http.StatusUnprocessableEntity {
			t.Errorf("wrong code: %d", status)
		}
	}
	if status := confirm(code); status != http.StatusUnprocessableEntity {
		t.Errorf("out of attempts, even with the right code: %d", status)
	}

	status, body := api.Post(shop.Store("/account/email-verification"), "", apitest.M{"email": "ann@example.com"})
	api.Want(status, http.StatusAccepted, body)
	fresh := shop.EmailedCode("ann@example.com")
	if status := confirm(fresh); status != http.StatusNoContent {
		t.Errorf("fresh code: %d", status)
	}
	if status := confirm(fresh); status != http.StatusUnprocessableEntity {
		t.Errorf("reused code: %d", status)
	}
	if _, me := api.Get(shop.Store("/account"), token); me.Get("email_verified").V != true {
		t.Errorf("me %s", me)
	}
}

func TestAPasswordResetSignsOutEverywhere(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	old := shop.UnverifiedShopper("ann@example.com")
	reset := shop.Store("/account/password-reset")

	if status, _ := api.Post(reset, "", apitest.M{"email": "nobody@example.com"}); status != http.StatusAccepted {
		t.Errorf("unknown emails look the same: %d", status)
	}
	if status, _ := api.Post(reset, "", apitest.M{"email": "ann@example.com"}); status != http.StatusAccepted {
		t.Errorf("reset request: %d", status)
	}
	code := shop.EmailedCode("ann@example.com")
	status, body := api.Post(shop.Store("/account/password-reset/confirm"), "",
		apitest.M{"email": "ann@example.com", "code": code, "new_password": "battery staple"})
	api.Want(status, http.StatusNoContent, body)

	if status, _ := api.Get(shop.Store("/account"), old); status != http.StatusUnauthorized {
		t.Errorf("old session after reset: %d", status)
	}
	if status, _ := accountSignIn(api, shop, "ann@example.com", "correct horse"); status != http.StatusUnauthorized {
		t.Errorf("old password after reset: %d", status)
	}
	status, session := accountSignIn(api, shop, "ann@example.com", "battery staple")
	api.Want(status, http.StatusOK, session)
	if session.Get("account", "email_verified").V != true {
		t.Errorf("the reset code proved the email: %s", session)
	}
}

func TestSignInAttemptsAreRateLimited(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()

	var statuses []int
	for range 12 {
		req := api.Request(http.MethodPost, shop.Store("/account/sessions"),
			apitest.M{"email": "ann@example.com", "password": "guess"}, "X-Forwarded-For", "192.0.2.7")
		status, _ := api.Send(req)
		statuses = append(statuses, status)
	}

	if !slices.Contains(statuses, http.StatusTooManyRequests) {
		t.Errorf("statuses %v", statuses)
	}
}
