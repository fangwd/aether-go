package ses_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"

	"aether/internal/ports"
	"aether/internal/providers/ses"
)

func mailer(server *httptest.Server) *ses.Mailer {
	return ses.New(sesv2.New(sesv2.Options{
		BaseEndpoint:     aws.String(server.URL),
		Region:           "us-east-1",
		Credentials:      credentials.NewStaticCredentialsProvider("AKIDTEST", "secret", ""),
		HTTPClient:       server.Client(),
		RetryMaxAttempts: 1,
	}))
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRegisteringADomainReturnsItsDKIMRecords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/email/identities" ||
			!strings.Contains(readBody(t, r), "mail.shop.example") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"IdentityType": "DOMAIN", "VerifiedForSendingStatus": false,
			"DkimAttributes": {"Tokens": ["abc", "def"], "Status": "PENDING"}}`))
	}))
	defer server.Close()

	records, err := mailer(server).RegisterDomain(context.Background(), "mail.shop.example")
	if err != nil {
		t.Fatal(err)
	}

	want := []ports.DNSRecord{
		{Type: "CNAME", Name: "abc._domainkey.mail.shop.example", Value: "abc.dkim.amazonses.com"},
		{Type: "CNAME", Name: "def._domainkey.mail.shop.example", Value: "def.dkim.amazonses.com"},
	}
	if !reflect.DeepEqual(records, want) {
		t.Errorf("records = %+v, want %+v", records, want)
	}
}

func TestDomainStatusFollowsSES(t *testing.T) {
	identities := map[string]string{
		"/v2/email/identities/ok.example":   `{"VerifiedForSendingStatus": true, "DkimAttributes": {"Status": "SUCCESS", "Tokens": []}}`,
		"/v2/email/identities/wait.example": `{"VerifiedForSendingStatus": false, "DkimAttributes": {"Status": "PENDING", "Tokens": []}}`,
		"/v2/email/identities/bad.example":  `{"VerifiedForSendingStatus": false, "DkimAttributes": {"Status": "FAILED", "Tokens": []}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := identities[r.URL.Path]
		if r.Method != http.MethodGet || !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	m := mailer(server)

	for domain, want := range map[string]ports.DomainStatus{
		"ok.example":   ports.DomainVerified,
		"wait.example": ports.DomainPending,
		"bad.example":  ports.DomainFailed,
	} {
		got, err := m.DomainStatus(context.Background(), domain)
		if err != nil || got != want {
			t.Errorf("%s: status = %q, %v; want %q", domain, got, err, want)
		}
	}
}

func email() ports.Email {
	return ports.Email{
		From:    "Shop <orders@mail.shop.example>",
		To:      "ann@example.com",
		Subject: "Hi",
		Text:    "Hello",
		HTML:    "<p>Hello</p>",
	}
}

func TestSendingGoesThroughSES(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/v2/email/outbound-emails" ||
			!strings.Contains(body, "orders@mail.shop.example") || !strings.Contains(body, "ann@example.com") {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		_, _ = w.Write([]byte(`{"MessageId": "m-1"}`))
	}))
	defer server.Close()

	if err := mailer(server).Send(context.Background(), email()); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
}

func TestRefusedMessagesAreRejectedAndOutagesUnavailable(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/email/outbound-emails" {
			http.NotFound(w, r)
			return
		}
		status, code := http.StatusBadRequest, "MessageRejected"
		if calls.Add(1) > 1 {
			status, code = http.StatusTooManyRequests, "TooManyRequestsException"
		}
		w.Header().Set("x-amzn-ErrorType", code)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message": "nope"}`))
	}))
	defer server.Close()
	m := mailer(server)

	for _, want := range []ports.ErrorKind{ports.Rejected, ports.Unavailable} {
		err := m.Send(context.Background(), email())
		var perr *ports.ProviderError
		if !errors.As(err, &perr) || perr.Kind != want {
			t.Errorf("err = %v, want kind %d", err, want)
		}
	}
}
