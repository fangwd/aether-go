# aether

Commerce API (catalog, cart, checkout, orders) for shopping sites.

## Layout

| Package | Role |
| --- | --- |
| `openapi.yaml` | the API contract; `internal/core/oapi` is generated from it (oapi-codegen) and requests are validated against it |
| `migrations/`, `internal/db` | the schema (goose) and typed queries generated from `internal/db/queries/*.sql` (sqlc) |
| `internal/commerce` | pure commerce rules: money, pricing, promotions, tax, shipping, totals |
| `internal/ports` | what core needs from outside: `ImageStore`, `PaymentGateway`, `Mailer` |
| `internal/core` | the API handlers and worker; knows no vendor, only the ports |
| `internal/providers/*` | one package per vendor implementing a port: `stripe`, `cloudflare` (images), `ses`, `smtp` |
| `cmd/aether` | the binary: reads configuration and plugs providers into core |

Every table carries `site_id` and is guarded by Postgres row-level security. Handlers run their
queries inside `db.InSite`, which scopes the transaction to one site.

After changing `openapi.yaml`, a query or a migration, run `go generate ./...` and commit the
generated code.

## Local setup

Uses a local Postgres (`127.0.0.1:5433`) with its own database, `aether_dev`.

```sh
cp .env.example .env && set -a && . ./.env && set +a

# once per Postgres cluster: the roles the API and workers connect as
psql postgresql://postgres:postgres@127.0.0.1:5433/postgres \
  -v app_password=aether_app -v worker_password=aether_worker -f db/bootstrap.sql

createdb -h 127.0.0.1 -p 5433 -U postgres aether_dev
go run ./cmd/aether migrate
go run ./cmd/aether serve      # API docs at http://127.0.0.1:2384/docs
go run ./cmd/aether worker     # shop emails, unpaid-order and cart expiry, sending-domain checks
```

Admin routes are for partner services, not browsers. Storefront routes are public; a
signed-in shopper adds their session token.

## Partner access

Provider decides who may manage a site, then calls the site's `/admin` routes server to server with:

- `Authorization: Bearer <partner key>`: one of `AETHER_PARTNER_KEYS` (comma-separated, at least 32
  characters each, so a key can be rotated by adding the new one before removing the old).
- `Aether-Actor: provider:<user id>`: who is acting, recorded in order history.

aether trusts a valid key fully: it does no per-user or per-role checks of its own. A missing or
unknown key is a 401; a missing actor is a 400. The key gives access to every site, so it lives only
in Provider's backend.

## Shopper accounts

Each site's shoppers have their own accounts, kept apart from provider users and from other sites. An
account is a `customer` with a password. Routes, all under `/v1/sites/{site_id}`:

| Endpoint | Body | Returns |
| --- | --- | --- |
| `POST /account` | `email`, `password` (8-128 chars) | 201 session; emails a verification code |
| `POST /account/sessions` | `email`, `password` | session |
| `DELETE /account/sessions/current` | (session) | 204 |
| `GET /account` | (session) | `{ id, email, email_verified }` |
| `POST /account/email-verification` (`/confirm`) | `email` (+ `code`) | 202 (204) |
| `POST /account/password-reset` (`/confirm`) | `email` (+ `code`, `new_password`) | 202 (204); ends every session |

A session is `{ session_token, expires_at, account }`. Send the token as `Authorization: Bearer`.
Tokens are opaque: only their sha256 is stored. Sessions last 30 days and are extended as they are
used. Codes are six digits, valid 15 minutes, with 5 attempts. Requests for codes always answer 202,
so they reveal nothing about which emails have accounts. Routes that take a password or code are rate
limited per client IP (bursts of 10, then one every 2 seconds). Orders placed as a guest with the
same email join the account once the email is verified.

On a deployed site (e.g. `example.com`), the storefront should reach aether on its own origin.
The provider's hosting forwards `example.com/_commerce/*` to aether and fills in the site id from the
domain. Then a session can become an httpOnly cookie on `example.com` without any cross-site
cookies. That forwarding is not built yet.

## Database roles

| Role | Used by | Row-level security |
| --- | --- | --- |
| migrator (e.g. `postgres`) | `aether migrate` | owns the schema |
| `aether_app` | `aether serve` | enforced |
| `aether_worker` | `aether worker` | bypassed to find due work across sites; each item is then handled as `aether_app` for its site |

## Tests

```sh
go test ./...     # needs AETHER_TEST_DATABASE_URL (a role that can create databases) and the bootstrap roles
```

Each test gets its own database, cloned from a template migrated once per schema version
(`aether_test_<hash>`). Integration tests connect as `aether_app` (`SET ROLE`), because a superuser
bypasses RLS.

## Background work

`aether worker` (any number of replicas; work is claimed with `FOR UPDATE SKIP LOCKED`):

- Delivers queued events (`outbox_event`): order confirmation, shipping and refund emails, and
  account codes. Failures retry with exponential backoff, up to 10 attempts; a code is removed from
  its event once delivered.
- Shop emails go out from the site's own verified sending domain
  (`PUT /v1/sites/{site_id}/admin/email-sender` returns the DKIM records to publish). Until it is
  verified they come from the provider's address (`AETHER_EMAIL_FALLBACK_FROM`, the same verified SES
  sender the partner uses) under the shop's name, with replies going to the shop's reply-to.
- Cancels orders unpaid after 30 minutes on sites that take online payments (cancelling the open
  payment first), expires stale carts and sessions, and purges old idempotency keys and events.

## Everything together (Docker)

Runs Postgres (port 5434), Mailpit (UI on http://127.0.0.1:8025), `stripe-mock` (Stripe's official
API mock), aether (2384) and the worker, with a dev partner key for admin calls.

```sh
docker-compose up -d --build
scripts/smoke.sh                   # end-to-end: catalog, checkout, payment + refund, sign-up, emails
```

Payments go to `stripe-mock`; since it sends no webhooks, the smoke script posts the events Stripe
would send, signed with the stack's webhook secret, so signature checks and payment handling run
for real. For a real test-mode account, run `go run ./cmd/aether serve` with
`AETHER_STRIPE_API_URL=https://api.stripe.com`, `AETHER_STRIPE_SECRET_KEY`,
`AETHER_STRIPE_PUBLISHABLE_KEY` and `AETHER_STRIPE_WEBHOOK_SECRET` set, and forward webhooks with
`stripe listen --forward-connect-to localhost:2384/v1/webhooks/payments/stripe`.
