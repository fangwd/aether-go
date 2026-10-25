package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"

	"aether/internal/ports"
	"aether/internal/providers/cloudflare"
	"aether/internal/providers/ses"
	"aether/internal/providers/smtp"
	"aether/internal/providers/stripe"
	"aether/internal/providers/webhook"
)

var outbound = &http.Client{Timeout: 10 * time.Second}

// imageStore is Cloudflare Images; uploads answer 503 until it is configured.
func imageStore() ports.ImageStore {
	account, token, delivery := env("AETHER_CLOUDFLARE_IMAGES_ACCOUNT_ID", ""), env("AETHER_CLOUDFLARE_IMAGES_API_TOKEN", ""), env("AETHER_CLOUDFLARE_IMAGES_DELIVERY_URL", "")
	if account == "" || token == "" || delivery == "" {
		slog.Warn("no image host configured; media uploads are disabled")
		return nil
	}
	return cloudflare.New(outbound, cloudflare.Config{
		APIURL:      env("AETHER_CLOUDFLARE_IMAGES_API_URL", "https://api.cloudflare.com"),
		AccountID:   account,
		APIToken:    token,
		DeliveryURL: delivery,
		Variant:     env("AETHER_CLOUDFLARE_IMAGES_VARIANT", "public"),
		Environment: env("AETHER_CLOUDFLARE_IMAGES_ENVIRONMENT", "development"),
	})
}

// catalogHook posts catalog.changed to the partner that renders sites (Nova's
// backend); without a URL, changes are not announced.
func catalogHook() (ports.CatalogHook, error) {
	url, secret := env("AETHER_CATALOG_WEBHOOK_URL", ""), env("AETHER_CATALOG_WEBHOOK_SECRET", "")
	if url == "" && secret == "" {
		slog.Warn("no catalog webhook configured; catalog changes are not announced")
		return nil, nil
	}
	if url == "" || len(secret) < 32 {
		return nil, fmt.Errorf("AETHER_CATALOG_WEBHOOK_URL needs AETHER_CATALOG_WEBHOOK_SECRET of at least 32 characters, and the other way round")
	}
	return webhook.New(outbound, url, secret), nil
}

// paymentGateway is Stripe Connect; payment endpoints answer 503 until it is
// configured. Use test-mode keys in development and staging.
func paymentGateway() ports.PaymentGateway {
	secret, publishable, webhook := env("AETHER_STRIPE_SECRET_KEY", ""), env("AETHER_STRIPE_PUBLISHABLE_KEY", ""), env("AETHER_STRIPE_WEBHOOK_SECRET", "")
	if secret == "" || publishable == "" || webhook == "" {
		slog.Warn("no payment provider configured; online payments are disabled")
		return nil
	}
	return stripe.New(outbound, stripe.Config{
		APIURL:         env("AETHER_STRIPE_API_URL", "https://api.stripe.com"),
		SecretKey:      secret,
		PublishableKey: publishable,
		WebhookSecret:  webhook,
	})
}

// mailer sends through SES (standard AWS environment), SMTP to a local
// catcher such as Mailpit, or only logs.
func mailer(ctx context.Context) (ports.Mailer, error) {
	switch transport := env("AETHER_EMAIL_TRANSPORT", "log"); transport {
	case "ses":
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, err
		}
		return ses.New(sesv2.NewFromConfig(cfg)), nil
	case "smtp":
		port, err := envInt("AETHER_SMTP_PORT", 1025)
		if err != nil {
			return nil, err
		}
		return smtp.New(env("AETHER_SMTP_HOST", "127.0.0.1"), port), nil
	case "log":
		return ports.LogMailer{}, nil
	default:
		return nil, fmt.Errorf("AETHER_EMAIL_TRANSPORT must be ses, smtp or log, not %q", transport)
	}
}
