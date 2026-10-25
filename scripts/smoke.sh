#!/usr/bin/env bash
# End-to-end check against a running stack (docker-compose up -d --build):
# staff (through Provider's partner key) set up a shop, a guest buys with
# pickup and pays, staff refund part of it, a shopper signs up, the order
# ships, and Mailpit receives every email. Payments go to stripe-mock; the
# webhooks Stripe would send are signed and posted by this script. Safe to
# rerun.
set -euo pipefail

API=${API:-http://127.0.0.1:2384}
MAILPIT=${MAILPIT:-http://127.0.0.1:8025}
SITE=01a0a4cb-904a-72b6-bc99-c75ce66d5d74
PARTNER_KEY=${PARTNER_KEY:-dev-partner-key-not-for-production}   # compose's AETHER_PARTNER_KEYS
RUN=$(date +%s)
EMAIL="guest-$RUN@example.com"
SHOPPER="shopper-$RUN@example.com"
STRIPE_WEBHOOK_SECRET=${STRIPE_WEBHOOK_SECRET:-whsec_mock}   # compose's AETHER_STRIPE_WEBHOOK_SECRET

step() { printf '\n==> %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# call METHOD URL [JSON] [extra curl args...]; prints the body, fails on HTTP >= 400.
call() {
  local method=$1 url=$2 body=${3:-} out status
  shift $(( $# < 3 ? $# : 3 ))
  out=$(mktemp)
  status=$(curl -sS -o "$out" -w '%{http_code}' -X "$method" "$url" -H 'content-type: application/json' \
    ${body:+--data "$body"} "$@")
  if (( status >= 400 )); then fail "$method $url -> $status $(cat "$out")"; fi
  cat "$out"; rm -f "$out"
}
staff() { call "$1" "$API/v1/sites/$SITE$2" "${3:-}" -H "authorization: Bearer $PARTNER_KEY" -H "aether-actor: provider:1" "${@:4}"; }
guest() { call "$1" "$API/v1/sites/$SITE$2" "${3:-}" -H "x-cart-token: $CART" "${@:4}"; }

# Posts a Stripe event signed like Stripe does: t=<ts>,v1=hmac_sha256("<ts>.<payload>").
webhook() {
  local payload=$1 ts sig
  ts=$(date +%s)
  sig=$(printf '%s.%s' "$ts" "$payload" | openssl dgst -sha256 -hmac "$STRIPE_WEBHOOK_SECRET" -hex | sed 's/^.* //')
  call POST "$API/v1/webhooks/payments/stripe" "$payload" -H "stripe-signature: t=$ts,v1=$sig" >/dev/null
}

wait_for_mail() { # wait_for_mail TO SUBJECT_FRAGMENT
  for _ in $(seq 1 30); do
    if curl -sS "$MAILPIT/api/v1/search?query=to:$1" | jq -e --arg s "$2" '.messages[] | select(.Subject | contains($s))' >/dev/null; then
      echo "received: \"$2\" -> $1"; return 0
    fi
    sleep 1
  done
  fail "no email \"$2\" for $1"
}

step "services are up"
for _ in $(seq 1 30); do curl -fsS -o /dev/null "$API/readyz" && break; sleep 1; done
curl -fsS -o /dev/null "$API/readyz" || fail "aether not ready"

step "shop settings and a verified sending domain"
staff PUT "" '{"name":"Smoke Thai","currency":"USD","timezone":"UTC"}' | jq -c '{name, currency}'
staff PUT /admin/email-sender '{"domain":"mail.smoke.example","from_name":"Smoke Thai"}' >/dev/null
staff GET /admin/email-sender | jq -e '.status == "verified"' >/dev/null || fail "sender not verified"

step "catalog: Pad Thai with sizes, a spice add-on, and stock"
PRODUCT=$(staff POST /admin/products "$(jq -nc --arg slug "pad-thai-$RUN" '{
  slug: $slug, name: "Pad Thai", price: "15.00", status: "active", variant_axes: ["Size"],
  variants: [{attributes: {Size: "Regular"}}, {attributes: {Size: "Large"}, price_extra: "4.00"}],
  option_groups: [{code: "spice", name: "Spice", min_select: 1, max_select: 1,
    values: [{code: "mild", label: "Mild"}, {code: "spicy", label: "Spicy", price_extra: "2.00"}]}]}')")
LARGE=$(jq -r '.variants[] | select(.attributes.Size == "Large") | .id' <<<"$PRODUCT")
SPICY=$(jq -r '.option_groups[0].values[] | select(.code == "spicy") | .id' <<<"$PRODUCT")
staff POST "/admin/inventory/$LARGE/adjustments" '{"quantity":10,"kind":"receipt"}' | jq -c '{on_hand, available}'
curl -sS -o /dev/null -X POST "$API/v1/sites/$SITE/admin/shipping-methods" -H "authorization: Bearer $PARTNER_KEY" -H "aether-actor: provider:1" \
  -H 'content-type: application/json' --data '{"code":"pickup","name":"Pick up","kind":"pickup"}'   # 409 on reruns

step "guest buys a Large Spicy Pad Thai for pickup"
CART=$(call POST "$API/v1/sites/$SITE/carts" "" | jq -r .cart_token)
guest POST /carts/current/items "$(jq -nc --arg v "$LARGE" --arg o "$SPICY" '{variant_id: $v, option_value_ids: [$o]}')" \
  | jq -e '.subtotal == "21.00"' >/dev/null || fail "line price is not 21.00"
guest POST /carts/current/quote '{"shipping_method_code":"pickup"}' | jq -c '{subtotal, tax_total, grand_total}'
PLACED=$(guest POST /checkout "$(jq -nc --arg e "$EMAIL" '{email: $e, shipping_method_code: "pickup"}')" \
  -H "idempotency-key: smoke-$RUN")
ORDER=$(jq .order <<<"$PLACED")
ORDER_TOKEN=$(jq -r .order_token <<<"$PLACED")
ORDER_ID=$(jq -r .id <<<"$ORDER")
jq -c '{order_number, grand_total, payment_status}' <<<"$ORDER"
wait_for_mail "$EMAIL" "order #$(jq -r .order_number <<<"$ORDER") received"

step "the shop connects Stripe (Standard account) and Stripe enables charges"
staff POST /admin/payments/onboarding '{"return_url":"https://example.com/done","refresh_url":"https://example.com/retry"}' \
  | jq -c '{onboarding_url: .url}'
ACCOUNT=$(staff GET "" | jq -r .payment_account_id)
webhook "$(jq -nc --arg a "$ACCOUNT" --arg s "$SITE" --arg id "evt_account_$RUN" \
  '{id: $id, type: "account.updated", account: $a, data: {object: {id: $a, charges_enabled: true, metadata: {site_id: $s}}}}')"
staff GET "" | jq -e '.payments_enabled' >/dev/null || fail "charges not enabled"

step "the guest pays; Stripe confirms the payment"
PAYMENT=$(call POST "$API/v1/sites/$SITE/orders/$ORDER_ID/payment" "" -H "x-order-token: $ORDER_TOKEN")
jq -c '{provider, amount, currency, stripe_account: .client.stripe_account}' <<<"$PAYMENT"
webhook "$(jq -nc --arg a "$ACCOUNT" --arg s "$SITE" --arg o "$ORDER_ID" --arg pi "$(jq -r .payment_reference <<<"$PAYMENT")" \
  --arg id "evt_paid_$RUN" \
  '{id: $id, type: "payment_intent.succeeded", account: $a, data: {object: {id: $pi, metadata: {site_id: $s, order_id: $o}}}}')"
staff GET "/admin/orders/$ORDER_ID" | jq -e '.payment_status == "paid"' >/dev/null || fail "order not paid"
echo "order paid"

step "staff refund 5.00"
staff POST "/admin/orders/$ORDER_ID/refunds" '{"amount":"5.00","reason":"smoke test"}' | jq -c '{payment_status, refunded_total}'
wait_for_mail "$EMAIL" "refund for order"

step "a shopper creates an account on the shop; the code email goes worker -> Mailpit"
call POST "$API/v1/sites/$SITE/account" "$(jq -nc --arg e "$SHOPPER" '{email: $e, password: "correct horse"}')" \
  -H 'x-forwarded-for: 192.0.2.50' | jq -c '.account'
wait_for_mail "$SHOPPER" "confirm your email"

step "staff hand the order over"
staff POST "/admin/orders/$ORDER_ID/shipments" '{}' | jq -c '{fulfilment_status}'
wait_for_mail "$EMAIL" "is on its way"

printf '\nSMOKE OK (order %s)\n' "$ORDER_ID"
