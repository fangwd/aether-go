package smtp_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"aether/internal/ports"
	"aether/internal/providers/smtp"
)

func kind(t *testing.T, err error) ports.ErrorKind {
	t.Helper()
	var perr *ports.ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want a provider error", err)
	}
	return perr.Kind
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

func TestUnparseableAddressesAreRejected(t *testing.T) {
	for _, mutate := range []func(*ports.Email){
		func(e *ports.Email) { e.From = "not an address" },
		func(e *ports.Email) { e.To = "ann@" },
		func(e *ports.Email) { e.ReplyTo = "<<nope" },
	} {
		e := email()
		mutate(&e)
		if k := kind(t, smtp.New("127.0.0.1", 1).Send(context.Background(), e)); k != ports.Rejected {
			t.Errorf("%+v: kind = %d, want rejected", e, k)
		}
	}
}

func TestAnUnreachableServerIsUnavailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	if k := kind(t, smtp.New("127.0.0.1", port).Send(context.Background(), email())); k != ports.Unavailable {
		t.Errorf("kind = %d, want unavailable", k)
	}
}

func TestDomainsVerifyAtOnce(t *testing.T) {
	m := smtp.New("127.0.0.1", 1025)
	records, err := m.RegisterDomain(context.Background(), "shop.example")
	if err != nil || len(records) != 1 || records[0] != (ports.DNSRecord{Type: "CNAME", Name: "local._domainkey.shop.example", Value: "local.dkim.invalid"}) {
		t.Errorf("records = %+v, %v", records, err)
	}
	if status, err := m.DomainStatus(context.Background(), "shop.example"); err != nil || status != ports.DomainVerified {
		t.Errorf("status = %q, %v", status, err)
	}
}
