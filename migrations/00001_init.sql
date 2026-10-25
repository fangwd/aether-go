-- +goose Up
-- ============================================================================
-- Aether commerce schema: catalog / cart / checkout / order
-- PostgreSQL 14+.
--
-- Model: one shared database for every provider site, single merchant per site.
--   * Tenancy: every table carries site_id (e.g. provider's project id) and is
--     guarded by row-level security keyed on the transaction setting
--     app.site_id. Relationships use composite (site_id, id) foreign keys,
--     because foreign key checks bypass RLS: a single-column FK would let a
--     row point at another site's row.
--   * Catalog: products with a base price. Variants (Size L, Black) are the
--     stockable units and add a price_extra on top of the base. Option
--     groups are order-time add-ons that are not stocked ("Spicy +$2",
--     "choose up to 3 extras"), each value with its own price_extra.
--     Line unit price = product.price + variant.price_extra
--                       + sum(chosen option values' price_extra).
--   * Checkout is transient (no tables): it reads the cart, re-prices it,
--     quotes shipping and tax, applies promotions, then writes an order in
--     one transaction.
--   * Orders are immutable snapshots of names, prices, tax and addresses.
--   * Shoppers: a customer with a password is an account on that site;
--     guests are password-less profiles. Staff act through a partner service
--     (the partner service), which names them (provider:<user id>) in actor_subject.
--
-- Conventions:
--   * Table names are singular. "order" is a reserved word and must be
--     double-quoted in SQL.
--   * Roles (db/bootstrap.sql, once per cluster): the migrator owns the
--     schema; aether_app serves the API under RLS; aether_worker has
--     BYPASSRLS for cross-site jobs.
--   * ids: uuid, generated in the application as UUIDv7. Entity tables use
--     PRIMARY KEY (site_id, id): RLS adds site_id to every query, so the
--     primary key serves both.
--   * money: numeric(19,4) in the site's currency. Orders snapshot the
--     currency at placement, and payments/refunds carry the same code,
--     enforced by composite foreign keys onto "order" (site_id, id,
--     currency). Round to the currency's minor units before persisting.
--   * timestamps: timestamptz everywhere.
--   * status/type columns are text + CHECK (not native enums).
--   * Nothing that orders reference is hard-deleted: catalog rows are
--     archived via status. PG14 cannot SET NULL one column of a composite
--     foreign key, so there is no ON DELETE SET NULL across tables.
--   * updated_at maintained by trigger (set_updated_at).
--
-- Concurrency patterns (enforced by DDL below, driven by app):
--   * Reserve stock atomically, no retry loop:
--       UPDATE inventory
--          SET reserved = reserved + $qty, version = version + 1
--        WHERE variant_id = $v AND on_hand - reserved >= $qty;
--     rowcount 0 => insufficient stock. Lock multi-line carts in variant_id
--     order to avoid deadlocks.
--   * Allocate an order number:
--       UPDATE site SET next_order_number = next_order_number + 1
--        RETURNING next_order_number - 1;
--   * Redeem a promotion / coupon atomically:
--       UPDATE promotion SET usage_count = usage_count + 1
--        WHERE id = $1 AND status = 'active'
--          AND (usage_limit IS NULL OR usage_count < usage_limit);
--     (same shape for coupon_code.max_uses / usage_count).
-- ============================================================================

-- ============================================================================
-- 0. Extensions, privileges and shared functions
-- ============================================================================

CREATE EXTENSION IF NOT EXISTS citext;      -- case-insensitive emails/codes

-- Applies to every table this migrator creates, now and in later migrations.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO aether_app, aether_worker;

-- The site of the current transaction, set by the API with
-- set_config('app.site_id', $1, true). NULL when unset; NULLIF covers the
-- empty string a custom setting reverts to after a transaction ends.
-- +goose StatementBegin
CREATE FUNCTION app_site_id() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$ SELECT NULLIF(current_setting('app.site_id', true), '')::uuid $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- ============================================================================
-- 1. Site
-- ============================================================================

-- One row per provider site with commerce turned on.
CREATE TABLE site (
    id                  uuid PRIMARY KEY,              -- e.g. provider's project id

    name                text NOT NULL,
    currency            char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'), -- ISO 4217; fixed once the first order exists (app)
    timezone            text NOT NULL DEFAULT 'UTC',   -- IANA name

    next_order_number   bigint NOT NULL DEFAULT 1 CHECK (next_order_number > 0),

    -- Prices on this site already include tax; tax is extracted, not added.
    prices_include_tax  boolean NOT NULL DEFAULT false,
    -- Where the business is: the tax place for pickup orders.
    country_code        char(2) CHECK (country_code ~ '^[A-Z]{2}$'),
    region_code         text,

    payment_account_id  text UNIQUE,                   -- the merchant's account at the payment provider
    payments_enabled    boolean NOT NULL DEFAULT false,-- the provider lets it take payments

    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- The site's own sending domain. Shop emails go out from it once the email
-- provider has verified it (DNS records published by the merchant); until
-- then from the provider's address. A domain belongs to one site.
CREATE TABLE email_sender (
    site_id             uuid PRIMARY KEY REFERENCES site(id),

    domain              text NOT NULL UNIQUE,          -- lowercase, e.g. mail.example.com
    from_local          text NOT NULL DEFAULT 'orders',-- orders@<domain>
    from_name           text,                          -- defaults to the site name
    reply_to            citext,

    status              text NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'verified', 'failed')),
    -- DNS records the merchant must publish: [{"type","name","value"}].
    dns_records         jsonb NOT NULL DEFAULT '[]',

    verified_at         timestamptz,
    checked_at          timestamptz,

    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- ============================================================================
-- 2. Media
-- ============================================================================

-- Asset metadata; files live with the image store.
CREATE TABLE media (
    site_id             uuid NOT NULL REFERENCES site(id),
    id                  uuid NOT NULL,

    storage_provider    text NOT NULL,                 -- the image store's name
    storage_key         text NOT NULL,                 -- the store's id for the image

    mime_type           text NOT NULL,

    width               integer,
    height              integer,
    size_bytes          bigint,

    checksum            text,                          -- sha256 for dedupe/integrity

    metadata            jsonb NOT NULL DEFAULT '{}',   -- alt text, focal point, etc.

    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, storage_provider, storage_key)
);

CREATE UNIQUE INDEX uq_media_checksum ON media (site_id, checksum) WHERE checksum IS NOT NULL;

-- ============================================================================
-- 3. Customers
-- ============================================================================

-- Buyer profile. With a password it is a shopper account on this site;
-- without, a guest profile (one per email, reused by guest checkouts). When
-- an account verifies its email, the guest profile with it folds in.
CREATE TABLE customer (
    site_id             uuid NOT NULL REFERENCES site(id),
    id                  uuid NOT NULL,

    email               citext,
    password_hash       text,                          -- argon2id PHC string; NULL = guest
    email_verified_at   timestamptz,
    phone               text,

    first_name          text,
    last_name           text,

    metadata            jsonb NOT NULL DEFAULT '{}',

    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    CHECK (password_hash IS NULL OR email IS NOT NULL)
);

CREATE UNIQUE INDEX uq_customer_guest_email ON customer (site_id, email)
    WHERE email IS NOT NULL AND password_hash IS NULL;
CREATE UNIQUE INDEX uq_customer_account_email ON customer (site_id, email)
    WHERE password_hash IS NOT NULL;

-- Signed-in shopper sessions. Only the sha256 of the token is stored.
CREATE TABLE customer_session (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    customer_id         uuid NOT NULL,

    token_hash          bytea NOT NULL UNIQUE,

    expires_at          timestamptz NOT NULL,          -- extended as the session is used
    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    FOREIGN KEY (site_id, customer_id) REFERENCES customer (site_id, id) ON DELETE CASCADE
);

-- Six-digit codes emailed to verify an address or reset a password. A new
-- code replaces any earlier unused one for the same purpose.
CREATE TABLE customer_code (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    customer_id         uuid NOT NULL,

    purpose             text NOT NULL CHECK (purpose IN ('verify_email', 'reset_password')),
    code_hash           bytea NOT NULL,                -- sha256 of the code
    attempts            integer NOT NULL DEFAULT 0,

    expires_at          timestamptz NOT NULL,
    used_at             timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    FOREIGN KEY (site_id, customer_id) REFERENCES customer (site_id, id) ON DELETE CASCADE
);

CREATE TABLE customer_address (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    customer_id         uuid NOT NULL,

    type                text NOT NULL
                        CHECK (type IN ('billing', 'shipping')),

    first_name          text,
    last_name           text,
    company             text,

    line1               text NOT NULL,
    line2               text,
    city                text,
    state               text,                          -- free text; normalize in app per country
    postal_code         text,
    country_code        char(2) NOT NULL,              -- ISO 3166-1 alpha-2

    phone               text,

    is_default          boolean NOT NULL DEFAULT false,

    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    FOREIGN KEY (site_id, customer_id) REFERENCES customer (site_id, id) ON DELETE CASCADE
);

-- at most one default address per (customer, type)
CREATE UNIQUE INDEX uq_customer_address_default ON customer_address (site_id, customer_id, type) WHERE is_default;

-- ============================================================================
-- 4. Tax
-- ============================================================================

CREATE TABLE tax_class (
    site_id             uuid NOT NULL REFERENCES site(id),
    id                  uuid NOT NULL,

    code                text NOT NULL,                 -- 'standard', 'reduced', 'exempt'
    name                text NOT NULL,

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, code)
);

-- Rates per (class, jurisdiction, priority). Resolution in app: most specific
-- jurisdiction wins; same-specificity rows with different priorities STACK
-- (state + county). Orders snapshot computed tax; never re-derive.
CREATE TABLE tax_rate (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    tax_class_id        uuid NOT NULL,

    country_code        char(2),                       -- NULL = anywhere
    region_code         text,                          -- NULL = whole country

    rate                numeric(9,6) NOT NULL
                        CHECK (rate >= 0 AND rate < 1),-- fraction: 0.100000 = 10%

    priority            integer NOT NULL DEFAULT 0,    -- different priorities stack

    valid_from          timestamptz,
    valid_until         timestamptz,

    PRIMARY KEY (site_id, id),
    FOREIGN KEY (site_id, tax_class_id) REFERENCES tax_class (site_id, id)
);

-- ============================================================================
-- 5. Catalog: categories, products, variants, options
-- ============================================================================

-- Category tree (adjacency list).
CREATE TABLE category (
    site_id             uuid NOT NULL REFERENCES site(id),
    id                  uuid NOT NULL,
    parent_id           uuid,

    slug                text NOT NULL,
    name                text NOT NULL,
    description         text,

    status              text NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'inactive')),

    sort_order          integer NOT NULL DEFAULT 0,
    metadata            jsonb NOT NULL DEFAULT '{}',

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, slug),
    FOREIGN KEY (site_id, parent_id) REFERENCES category (site_id, id) ON DELETE CASCADE -- cascade deletes the subtree
);

-- A product carries the base price. Every product has at least one variant;
-- a product without real variations has exactly one default variant with
-- attributes = {} and price_extra = 0.
CREATE TABLE product (
    site_id             uuid NOT NULL REFERENCES site(id),
    id                  uuid NOT NULL,
    tax_class_id        uuid,                          -- NULL = default class (app)

    slug                text NOT NULL,
    name                text NOT NULL,
    short_description   text,
    description         text,

    price               numeric(19,4) NOT NULL CHECK (price >= 0),
    compare_at_price    numeric(19,4) CHECK (compare_at_price IS NULL OR compare_at_price > price), -- "was" price for strikethrough
    cost_amount         numeric(19,4),                 -- unit cost (private, never exposed on storefront)

    status              text NOT NULL DEFAULT 'draft'
                        CHECK (status IN ('draft', 'active', 'archived')),

    -- Variant axes in display order, e.g. {Size,Colour}; keys of
    -- product_variant.attributes.
    variant_axes        text[] NOT NULL DEFAULT '{}',

    attributes          jsonb NOT NULL DEFAULT '{}',   -- free-form specs: {"material":"cotton"}
    metadata            jsonb NOT NULL DEFAULT '{}',

    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    published_at        timestamptz,

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, slug),
    FOREIGN KEY (site_id, tax_class_id) REFERENCES tax_class (site_id, id)
);

-- A concrete, stockable variation of a product along its axes
-- (Size=L, Colour=Black). App must normalize axis values before insert
-- (10 and 10.0 are different jsonb values).
CREATE TABLE product_variant (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    product_id          uuid NOT NULL,

    sku                 text,                          -- stock code
    barcode             text,                          -- UPC/EAN
    name                text,                          -- e.g. "Large / Black"

    attributes          jsonb NOT NULL DEFAULT '{}',   -- axis values only: {"Size":"L","Colour":"Black"}

    price_extra         numeric(19,4) NOT NULL DEFAULT 0, -- signed, added to product.price; final price must be >= 0 (app)

    status              text NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'inactive')),

    sort_order          integer NOT NULL DEFAULT 0,

    weight              numeric(14,4),                 -- shipping weight
    weight_unit         text CHECK (weight_unit IN ('kg', 'g', 'lb', 'oz')),
    length              numeric(14,4),
    width               numeric(14,4),
    height              numeric(14,4),
    dimension_unit      text CHECK (dimension_unit IN ('cm', 'm', 'in')),

    metadata            jsonb NOT NULL DEFAULT '{}',

    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    -- Deferred to commit, so a variant set can be rewritten in one
    -- transaction with values moving between rows.
    UNIQUE (site_id, sku) DEFERRABLE INITIALLY DEFERRED,
    UNIQUE (site_id, product_id, attributes) DEFERRABLE INITIALLY DEFERRED, -- no duplicate axis combinations per product
    FOREIGN KEY (site_id, product_id) REFERENCES product (site_id, id) ON DELETE CASCADE
);

-- Order-time add-ons that change the price without being a stockable unit
-- ("Spice: Mild / Spicy +2.00", "Extras: choose up to 3"). A dish with
-- spice x protein x extras would otherwise need a variant per combination.
CREATE TABLE product_option_group (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    product_id          uuid NOT NULL,

    code                text NOT NULL,
    name                text NOT NULL,

    -- Selection rules. min_select 1 makes the group required; max_select 1
    -- makes it a single choice. "up to three extras" is (0, 3).
    min_select          integer NOT NULL DEFAULT 1 CHECK (min_select >= 0),
    max_select          integer CHECK (max_select IS NULL OR max_select >= 1),

    sort_order          integer NOT NULL DEFAULT 0,

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, product_id, code) DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (site_id, product_id) REFERENCES product (site_id, id) ON DELETE CASCADE,
    CHECK (max_select IS NULL OR max_select >= min_select)
);

CREATE TABLE product_option_value (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    group_id            uuid NOT NULL,

    code                text NOT NULL,
    label               text NOT NULL,

    price_extra         numeric(19,4) NOT NULL DEFAULT 0, -- signed: spicy +2.0000, no cheese -1.0000

    is_default          boolean NOT NULL DEFAULT false,-- preselected in the storefront
    sort_order          integer NOT NULL DEFAULT 0,

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, group_id, code) DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (site_id, group_id) REFERENCES product_option_group (site_id, id) ON DELETE CASCADE
);

-- Category assignments. At most one primary category per product.
CREATE TABLE product_category (
    site_id             uuid NOT NULL,
    product_id          uuid NOT NULL,
    category_id         uuid NOT NULL,

    is_primary          boolean NOT NULL DEFAULT false,
    position            integer NOT NULL DEFAULT 0,

    PRIMARY KEY (site_id, product_id, category_id),
    FOREIGN KEY (site_id, product_id) REFERENCES product (site_id, id) ON DELETE CASCADE,
    FOREIGN KEY (site_id, category_id) REFERENCES category (site_id, id) ON DELETE CASCADE
);

CREATE TABLE product_media (
    site_id             uuid NOT NULL,
    product_id          uuid NOT NULL,
    media_id            uuid NOT NULL,

    role                text NOT NULL DEFAULT 'gallery'
                        CHECK (role IN ('gallery', 'thumbnail', 'video', 'swatch')),
    position            integer NOT NULL DEFAULT 0,

    PRIMARY KEY (site_id, product_id, media_id),
    FOREIGN KEY (site_id, product_id) REFERENCES product (site_id, id) ON DELETE CASCADE,
    FOREIGN KEY (site_id, media_id) REFERENCES media (site_id, id) ON DELETE CASCADE
);

CREATE TABLE variant_media (
    site_id             uuid NOT NULL,
    variant_id          uuid NOT NULL,
    media_id            uuid NOT NULL,

    position            integer NOT NULL DEFAULT 0,

    PRIMARY KEY (site_id, variant_id, media_id),
    FOREIGN KEY (site_id, variant_id) REFERENCES product_variant (site_id, id) ON DELETE CASCADE,
    FOREIGN KEY (site_id, media_id) REFERENCES media (site_id, id) ON DELETE CASCADE
);

-- ============================================================================
-- 6. Inventory (per variant)
-- ============================================================================

-- Stock of one variant. A variant without a row is not stock-tracked
-- (e.g. made-to-order dishes) and is always purchasable.
CREATE TABLE inventory (
    site_id             uuid NOT NULL,
    variant_id          uuid NOT NULL,

    on_hand             bigint NOT NULL DEFAULT 0,
    reserved            bigint NOT NULL DEFAULT 0,     -- held by active inventory_reservation rows
    available           bigint GENERATED ALWAYS AS (on_hand - reserved) STORED,

    version             bigint NOT NULL DEFAULT 0,     -- optimistic concurrency

    updated_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, variant_id),
    FOREIGN KEY (site_id, variant_id) REFERENCES product_variant (site_id, id) ON DELETE CASCADE,

    CHECK (on_hand >= 0),
    CHECK (reserved >= 0),
    CHECK (reserved <= on_hand)
);

-- Append-only stock ledger; on_hand must always equal the sum of quantities
-- per variant.
CREATE TABLE inventory_transaction (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    variant_id          uuid NOT NULL,

    transaction_type    text NOT NULL
                        CHECK (transaction_type IN ('receipt', 'adjustment', 'sale', 'return')),

    quantity            bigint NOT NULL CHECK (quantity <> 0), -- signed change to on_hand (+in / -out)

    reference_type      text,                          -- 'order', 'refund', 'count', ...
    reference_id        uuid,

    metadata            jsonb NOT NULL DEFAULT '{}',
    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    FOREIGN KEY (site_id, variant_id) REFERENCES inventory (site_id, variant_id)
);

-- ============================================================================
-- 7. Cart
-- ============================================================================

-- Guest carts are addressed by a random token only the guest's browser
-- holds (sha256 stored); shopper carts by customer.
CREATE TABLE cart (
    site_id             uuid NOT NULL REFERENCES site(id),
    id                  uuid NOT NULL,

    customer_id         uuid,
    token_hash          bytea,

    coupon_code         citext,                        -- entered by the shopper; validated at pricing time

    status              text NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'converted', 'merged', 'expired')),

    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz,                   -- reaper marks expired + releases reservations

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, token_hash),
    FOREIGN KEY (site_id, customer_id) REFERENCES customer (site_id, id),
    CHECK (customer_id IS NOT NULL OR token_hash IS NOT NULL)
);

-- one active cart per customer
CREATE UNIQUE INDEX uq_cart_active_customer ON cart (site_id, customer_id) WHERE status = 'active' AND customer_id IS NOT NULL;

CREATE TABLE cart_item (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    cart_id             uuid NOT NULL,
    variant_id          uuid NOT NULL,

    quantity            integer NOT NULL CHECK (quantity > 0),

    -- Chosen option values, sorted by the app so array equality de-duplicates
    -- lines: the same dish mild and spicy are two lines. App validates they
    -- belong to the variant's product and satisfy min/max_select.
    option_value_ids    uuid[] NOT NULL DEFAULT '{}',

    unit_price          numeric(19,4),                 -- last-seen price; ALWAYS re-priced at render/checkout

    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, cart_id, variant_id, option_value_ids),
    FOREIGN KEY (site_id, cart_id) REFERENCES cart (site_id, id) ON DELETE CASCADE,
    FOREIGN KEY (site_id, variant_id) REFERENCES product_variant (site_id, id)
);

-- ============================================================================
-- 8. Shipping configuration
-- ============================================================================

CREATE TABLE shipping_zone (
    site_id             uuid NOT NULL REFERENCES site(id),
    id                  uuid NOT NULL,

    code                text NOT NULL,
    name                text NOT NULL,                 -- e.g. "Domestic", "International"

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, code)
);

-- Zone membership. region_code '*' = entire country; otherwise a country
-- subdivision code (state/province).
CREATE TABLE shipping_zone_region (
    site_id             uuid NOT NULL,
    zone_id             uuid NOT NULL,

    country_code        char(2) NOT NULL,
    region_code         text NOT NULL DEFAULT '*',

    PRIMARY KEY (site_id, zone_id, country_code, region_code),
    FOREIGN KEY (site_id, zone_id) REFERENCES shipping_zone (site_id, id) ON DELETE CASCADE
);

-- A method serves exactly the zones it has shipping_rate rows for.
CREATE TABLE shipping_method (
    site_id             uuid NOT NULL REFERENCES site(id),
    id                  uuid NOT NULL,
    tax_class_id        uuid,                          -- NULL = shipping not taxed

    code                text NOT NULL,
    name                text NOT NULL,
    carrier             text,                          -- 'auspost', 'dhl', ...

    -- pickup needs no address and has no rates: it is always free.
    kind                text NOT NULL DEFAULT 'delivery' CHECK (kind IN ('delivery', 'pickup')),

    status              text NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'inactive')),

    metadata            jsonb NOT NULL DEFAULT '{}',

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, code),
    FOREIGN KEY (site_id, tax_class_id) REFERENCES tax_class (site_id, id)
);

-- Flat/tiered rates per (method, zone): pick the row with the highest
-- min_order_amount <= cart subtotal.
CREATE TABLE shipping_rate (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    method_id           uuid NOT NULL,
    zone_id             uuid NOT NULL,

    min_order_amount    numeric(19,4) NOT NULL DEFAULT 0 CHECK (min_order_amount >= 0),
    max_order_amount    numeric(19,4) CHECK (max_order_amount IS NULL OR max_order_amount > min_order_amount),

    price               numeric(19,4) NOT NULL CHECK (price >= 0),

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, method_id, zone_id, min_order_amount),
    FOREIGN KEY (site_id, method_id) REFERENCES shipping_method (site_id, id) ON DELETE CASCADE,
    FOREIGN KEY (site_id, zone_id) REFERENCES shipping_zone (site_id, id) ON DELETE CASCADE
);

-- ============================================================================
-- 9. Orders and shipments
-- ============================================================================

-- The order is an immutable historical snapshot: names, prices, tax and
-- addresses are copied in at placement time. Payment and fulfilment
-- progress are tracked separately; cancellation is fulfilment_status.
CREATE TABLE "order" (
    site_id             uuid NOT NULL REFERENCES site(id),
    id                  uuid NOT NULL,

    customer_id         uuid,
    customer_email      citext NOT NULL,                 -- snapshot; survives customer erasure

    order_number        bigint NOT NULL,                 -- from site.next_order_number
    access_token_hash   bytea,                           -- guest order lookup; sha256 of the token

    currency            char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'), -- ISO 4217; all money columns below are in it

    payment_status      text NOT NULL DEFAULT 'pending'
                        CHECK (payment_status IN ('pending', 'paid', 'partially_refunded', 'refunded')),
    fulfilment_status   text NOT NULL DEFAULT 'unfulfilled'
                        CHECK (fulfilment_status IN ('unfulfilled', 'partially_fulfilled', 'fulfilled', 'cancelled')),

    shipping_method_code text,                           -- snapshot of the chosen shipping_method.code; NULL = nothing to ship
    prices_include_tax  boolean NOT NULL DEFAULT false,  -- snapshot of the site's: whether line prices include tax_total

    subtotal            numeric(19,4) NOT NULL DEFAULT 0,
    discount_total      numeric(19,4) NOT NULL DEFAULT 0,
    shipping_total      numeric(19,4) NOT NULL DEFAULT 0, -- before tax
    shipping_tax        numeric(19,4) NOT NULL DEFAULT 0, -- kept apart from line tax; often rated differently
    tax_total           numeric(19,4) NOT NULL DEFAULT 0, -- line tax + shipping_tax
    grand_total         numeric(19,4) NOT NULL DEFAULT 0,
    refunded_total      numeric(19,4) NOT NULL DEFAULT 0 CHECK (refunded_total >= 0), -- sum of succeeded refunds
    CHECK (refunded_total <= grand_total),

    discount_snapshot   jsonb NOT NULL DEFAULT '{}',     -- applied promotions/coupons and per-rule amounts

    placed_at           timestamptz,
    cancelled_at        timestamptz,

    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    metadata            jsonb NOT NULL DEFAULT '{}',

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, order_number),
    UNIQUE (site_id, access_token_hash),
    UNIQUE (site_id, id, currency),                      -- target for currency-matching FKs
    FOREIGN KEY (site_id, customer_id) REFERENCES customer (site_id, id)
);

-- One line per purchased variant + option combination.
CREATE TABLE order_item (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    order_id            uuid NOT NULL,

    product_id          uuid,
    variant_id          uuid,

    sku                 text,                          -- snapshot
    product_name        text NOT NULL,                 -- snapshot
    variant_name        text,                          -- snapshot

    quantity            integer NOT NULL CHECK (quantity > 0),

    unit_price          numeric(19,4) NOT NULL,        -- base + variant extra + option extras
    discount_total      numeric(19,4) NOT NULL DEFAULT 0,
    tax_total           numeric(19,4) NOT NULL DEFAULT 0,
    total               numeric(19,4) NOT NULL,        -- unit_price*quantity - discount + tax (app-computed)

    refunded_quantity   integer NOT NULL DEFAULT 0 CHECK (refunded_quantity >= 0 AND refunded_quantity <= quantity),

    product_snapshot    jsonb NOT NULL DEFAULT '{}',   -- frozen product/variant state
    -- Frozen option choices: [{"group":"Spice","label":"Spicy","price_extra":"2.0000"}].
    -- unit_price already includes these; the array is what the receipt prints.
    options             jsonb NOT NULL DEFAULT '[]',

    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, id, order_id),                    -- target for shipment_item
    FOREIGN KEY (site_id, order_id) REFERENCES "order" (site_id, id),
    FOREIGN KEY (site_id, product_id) REFERENCES product (site_id, id),
    FOREIGN KEY (site_id, variant_id) REFERENCES product_variant (site_id, id)
);

-- Address snapshots (copied from customer_address or entered at checkout).
CREATE TABLE order_address (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    order_id            uuid NOT NULL,

    type                text NOT NULL CHECK (type IN ('billing', 'shipping')),

    first_name          text,
    last_name           text,
    company             text,

    line1               text NOT NULL,
    line2               text,
    city                text,
    state               text,
    postal_code         text,
    country_code        char(2) NOT NULL,

    phone               text,

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, order_id, type),
    FOREIGN KEY (site_id, order_id) REFERENCES "order" (site_id, id) ON DELETE CASCADE
);

-- Status audit trail.
CREATE TABLE order_status_history (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    order_id            uuid NOT NULL,

    status_type         text NOT NULL CHECK (status_type IN ('payment', 'fulfilment')),
    from_status         text,
    to_status           text NOT NULL,
    reason              text,

    actor_subject       text,                          -- provider:<user id> for staff; NULL = system/workflow

    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    FOREIGN KEY (site_id, order_id) REFERENCES "order" (site_id, id) ON DELETE CASCADE
);

-- A physical parcel; an order may ship in several parcels.
CREATE TABLE shipment (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    order_id            uuid NOT NULL,

    carrier             text,
    service             text,
    tracking_number     text,

    status              text NOT NULL
                        CHECK (status IN ('in_transit', 'delivered', 'failed', 'returned')),

    shipped_at          timestamptz,
    delivered_at        timestamptz,

    metadata            jsonb NOT NULL DEFAULT '{}',   -- label urls, package dims/weight

    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, id, order_id),                    -- target for shipment_item
    FOREIGN KEY (site_id, order_id) REFERENCES "order" (site_id, id)
);

-- Units of a line in a parcel. order_id is carried so a shipment can only
-- contain lines from its own order.
CREATE TABLE shipment_item (
    site_id             uuid NOT NULL,
    shipment_id         uuid NOT NULL,
    order_item_id       uuid NOT NULL,
    order_id            uuid NOT NULL,

    quantity            integer NOT NULL CHECK (quantity > 0),

    PRIMARY KEY (site_id, shipment_id, order_item_id),
    FOREIGN KEY (site_id, shipment_id, order_id) REFERENCES shipment (site_id, id, order_id) ON DELETE CASCADE,
    FOREIGN KEY (site_id, order_item_id, order_id) REFERENCES order_item (site_id, id, order_id)
);

-- Stock an order holds until it is paid (converted into a sale) or
-- cancelled (released). Unpaid orders are cancelled by the worker.
CREATE TABLE inventory_reservation (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    variant_id          uuid NOT NULL,
    order_id            uuid NOT NULL,

    quantity            bigint NOT NULL CHECK (quantity > 0),

    status              text NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'released', 'converted')),

    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    FOREIGN KEY (site_id, variant_id) REFERENCES inventory (site_id, variant_id),
    -- Deferred: checkout holds stock before it writes the order.
    FOREIGN KEY (site_id, order_id) REFERENCES "order" (site_id, id) DEFERRABLE INITIALLY DEFERRED
);

-- ============================================================================
-- 10. Payments and refunds
-- ============================================================================

-- One row per payment attempt against an order (retries create new rows).
-- No card numbers/CVV ever - provider tokenization only.
CREATE TABLE payment (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    order_id            uuid NOT NULL,
    currency            char(3) NOT NULL,

    provider            text NOT NULL,                 -- the payment provider's name
    provider_reference  text,                          -- the provider's id for this payment

    amount              numeric(19,4) NOT NULL CHECK (amount > 0),

    status              text NOT NULL
                        CHECK (status IN ('open', 'processing', 'succeeded', 'failed', 'cancelled')),

    metadata            jsonb NOT NULL DEFAULT '{}',   -- client: what the storefront's payment SDK needs

    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, id, order_id),                    -- target for refund
    FOREIGN KEY (site_id, order_id, currency) REFERENCES "order" (site_id, id, currency)
);

-- webhook dedupe: a provider reference identifies exactly one payment
CREATE UNIQUE INDEX uq_payment_provider_ref
    ON payment (site_id, provider, provider_reference) WHERE provider_reference IS NOT NULL;

-- Money back to the buyer.
CREATE TABLE refund (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    order_id            uuid NOT NULL,
    currency            char(3) NOT NULL,
    payment_id          uuid,

    provider            text NOT NULL,
    provider_reference  text,

    amount              numeric(19,4) NOT NULL CHECK (amount > 0),
    reason              text,

    status              text NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'succeeded', 'failed', 'cancelled')),

    metadata            jsonb NOT NULL DEFAULT '{}',   -- refunded lines/quantities, restock decisions

    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    FOREIGN KEY (site_id, order_id, currency) REFERENCES "order" (site_id, id, currency),
    FOREIGN KEY (site_id, payment_id, order_id) REFERENCES payment (site_id, id, order_id)
);

CREATE UNIQUE INDEX uq_refund_provider_ref
    ON refund (site_id, provider, provider_reference) WHERE provider_reference IS NOT NULL;

-- ============================================================================
-- 11. Promotions
-- ============================================================================

-- Rules/actions stay flexible jsonb interpreted by the promotion engine.
-- A promotion is either automatic (no coupon_code rows) or coupon-driven;
-- a single shared code is one coupon_code row with max_uses NULL.
CREATE TABLE promotion (
    site_id             uuid NOT NULL REFERENCES site(id),
    id                  uuid NOT NULL,

    name                text NOT NULL,

    promotion_type      text NOT NULL,                 -- engine-defined: 'cart_discount', 'product_discount', 'free_shipping', ...

    starts_at           timestamptz,
    ends_at             timestamptz,
    CHECK (ends_at IS NULL OR starts_at IS NULL OR ends_at > starts_at),

    usage_limit         bigint,                        -- global redemptions cap
    usage_count         bigint NOT NULL DEFAULT 0 CHECK (usage_count >= 0), -- increment atomically (see header)
    per_customer_limit  integer,                       -- enforced by counting promotion_redemption per customer

    status              text NOT NULL DEFAULT 'active'
                        CHECK (status IN ('draft', 'active', 'paused', 'expired')),

    rules               jsonb NOT NULL DEFAULT '{}',   -- conditions DSL
    actions             jsonb NOT NULL DEFAULT '{}',   -- discount definitions

    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id)
);

-- Codes that unlock a promotion: one shared public code, or bulk-issued
-- unique codes (optionally targeted at a customer). Single-use = max_uses 1.
CREATE TABLE coupon_code (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    promotion_id        uuid NOT NULL,

    code                citext NOT NULL,

    customer_id         uuid,                          -- NULL = anyone may use it

    max_uses            integer CHECK (max_uses IS NULL OR max_uses > 0), -- NULL = unlimited
    usage_count         integer NOT NULL DEFAULT 0 CHECK (usage_count >= 0), -- increment atomically (see header)

    status              text NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'disabled')),

    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, code),
    UNIQUE (site_id, id, promotion_id),                -- target for promotion_redemption
    FOREIGN KEY (site_id, promotion_id) REFERENCES promotion (site_id, id) ON DELETE CASCADE,
    FOREIGN KEY (site_id, customer_id) REFERENCES customer (site_id, id)
);

-- Redemption log: enforces per_customer_limit and gives an audit trail for
-- automatic promotions. customer_id NULL = guest order.
CREATE TABLE promotion_redemption (
    site_id             uuid NOT NULL,
    id                  uuid NOT NULL,
    promotion_id        uuid NOT NULL,
    customer_id         uuid,
    coupon_code_id      uuid,
    order_id            uuid NOT NULL,

    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site_id, id),
    UNIQUE (site_id, promotion_id, order_id),          -- one redemption per promotion per order
    FOREIGN KEY (site_id, promotion_id) REFERENCES promotion (site_id, id) ON DELETE CASCADE,
    FOREIGN KEY (site_id, customer_id) REFERENCES customer (site_id, id),
    -- A redemption's coupon must belong to the promotion being redeemed.
    FOREIGN KEY (site_id, coupon_code_id, promotion_id) REFERENCES coupon_code (site_id, id, promotion_id),
    FOREIGN KEY (site_id, order_id) REFERENCES "order" (site_id, id) ON DELETE CASCADE
);

-- ============================================================================
-- 12. Outbox and idempotency
-- ============================================================================

-- Transactional outbox: written in the SAME transaction as the state change
-- and processed by the worker. Delete rows once processed and past
-- retention.
CREATE TABLE outbox_event (
    site_id             uuid NOT NULL REFERENCES site(id),
    id                  uuid NOT NULL,

    topic               text NOT NULL,
    event_key           text,                          -- ordering key for consumers (e.g. order id)

    payload             jsonb NOT NULL,

    attempts            integer NOT NULL DEFAULT 0,
    last_error          text,
    next_retry_at       timestamptz,                   -- backoff scheduling

    created_at          timestamptz NOT NULL DEFAULT now(),
    published_at        timestamptz,

    PRIMARY KEY (site_id, id)
);

-- API-level dedupe for unsafe retries (order placement, webhooks).
CREATE TABLE idempotency_key (
    site_id             uuid NOT NULL REFERENCES site(id),
    key                 text NOT NULL,                 -- client-supplied (UUID recommended)
    request_path        text NOT NULL,

    response_status     integer,
    response_body       jsonb,

    created_at          timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz NOT NULL,          -- reaper deletes expired keys

    PRIMARY KEY (site_id, key)
);

-- ============================================================================
-- 13. Invariants spanning rows
-- ============================================================================

-- Repeating a stock movement for the same source event is a no-op rather
-- than a double count.
CREATE UNIQUE INDEX uq_inventory_transaction_source
    ON inventory_transaction (site_id, transaction_type, reference_type, reference_id, variant_id)
    WHERE reference_id IS NOT NULL;

-- Units shipped across all shipments cannot exceed units ordered.
-- +goose StatementBegin
CREATE FUNCTION assert_shipped_quantity() RETURNS trigger AS $$
DECLARE ordered integer; shipped integer;
BEGIN
    SELECT oi.quantity INTO ordered FROM order_item oi
     WHERE oi.site_id = NEW.site_id AND oi.id = NEW.order_item_id;
    SELECT COALESCE(sum(si.quantity), 0) INTO shipped
      FROM shipment_item si
     WHERE si.site_id = NEW.site_id AND si.order_item_id = NEW.order_item_id;
    IF shipped > ordered THEN
        RAISE EXCEPTION 'order item % has % units but % are shipped', NEW.order_item_id, ordered, shipped
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER assert_shipped_quantity
    AFTER INSERT OR UPDATE ON shipment_item
    DEFERRABLE INITIALLY IMMEDIATE
    FOR EACH ROW EXECUTE FUNCTION assert_shipped_quantity();

-- Succeeded refunds cannot exceed what the order was charged.
-- +goose StatementBegin
CREATE FUNCTION assert_refund_total() RETURNS trigger AS $$
DECLARE charged numeric; refunded numeric;
BEGIN
    IF NEW.status <> 'succeeded' THEN
        RETURN NEW;
    END IF;
    SELECT o.grand_total INTO charged FROM "order" o
     WHERE o.site_id = NEW.site_id AND o.id = NEW.order_id;
    SELECT COALESCE(sum(r.amount), 0) INTO refunded
      FROM refund r
     WHERE r.site_id = NEW.site_id AND r.order_id = NEW.order_id AND r.status = 'succeeded';
    IF refunded > charged THEN
        RAISE EXCEPTION 'refunds of % exceed the % charged on order %', refunded, charged, NEW.order_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER assert_refund_total
    AFTER INSERT OR UPDATE ON refund
    DEFERRABLE INITIALLY IMMEDIATE
    FOR EACH ROW EXECUTE FUNCTION assert_refund_total();

-- The stock ledger is append-only: corrections are new adjustment rows.
-- +goose StatementBegin
CREATE FUNCTION forbid_ledger_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only; write a correcting row instead', TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER forbid_inventory_transaction_change BEFORE UPDATE OR DELETE ON inventory_transaction
    FOR EACH ROW EXECUTE FUNCTION forbid_ledger_change();

-- ============================================================================
-- 14. Row-level security
-- ============================================================================

-- Every table is visible only within the transaction's site. FORCE applies
-- the policy to the table owner too; superusers and aether_worker
-- (BYPASSRLS) are exempt by design.
ALTER TABLE site FORCE ROW LEVEL SECURITY;
ALTER TABLE site ENABLE ROW LEVEL SECURITY;
CREATE POLICY site_isolation ON site
    USING (id = app_site_id()) WITH CHECK (id = app_site_id());

-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'email_sender', 'media', 'customer', 'customer_session', 'customer_code',
        'customer_address', 'tax_class', 'tax_rate',
        'category', 'product', 'product_variant', 'product_option_group',
        'product_option_value', 'product_category', 'product_media', 'variant_media',
        'inventory', 'inventory_transaction', 'inventory_reservation',
        'cart', 'cart_item', 'shipping_zone', 'shipping_zone_region',
        'shipping_method', 'shipping_rate', 'order', 'order_item', 'order_address',
        'order_status_history', 'shipment', 'shipment_item', 'payment',
        'refund', 'promotion', 'coupon_code',
        'promotion_redemption', 'outbox_event', 'idempotency_key'
    ] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY site_isolation ON %I USING (site_id = app_site_id()) WITH CHECK (site_id = app_site_id())',
            t);
    END LOOP;
END;
$$;
-- +goose StatementEnd

-- ============================================================================
-- 15. updated_at triggers
-- ============================================================================

CREATE TRIGGER set_updated_at BEFORE UPDATE ON site                FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON email_sender        FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON customer            FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON product             FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON product_variant     FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON inventory           FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON cart                FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON cart_item           FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON "order"             FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON shipment            FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON payment             FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON refund              FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON promotion           FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================================
-- 16. Indexes (hot paths; PK/UNIQUE indexes are implicit and lead with site_id)
-- ============================================================================

-- catalog -------------------------------------------------------------------
CREATE INDEX idx_category_parent              ON category (site_id, parent_id);
CREATE INDEX idx_product_status               ON product (site_id, status);
CREATE INDEX idx_product_attributes           ON product USING gin (attributes jsonb_path_ops);
CREATE INDEX idx_product_variant_product      ON product_variant (site_id, product_id, sort_order);
CREATE INDEX idx_product_variant_barcode      ON product_variant (site_id, barcode) WHERE barcode IS NOT NULL;
CREATE INDEX idx_product_option_group_product ON product_option_group (site_id, product_id, sort_order);
CREATE INDEX idx_product_option_value_group   ON product_option_value (site_id, group_id, sort_order);
CREATE INDEX idx_product_category_category    ON product_category (site_id, category_id, product_id);
CREATE UNIQUE INDEX uq_product_category_primary ON product_category (site_id, product_id) WHERE is_primary;
CREATE INDEX idx_product_media_media          ON product_media (site_id, media_id);
CREATE INDEX idx_variant_media_media          ON variant_media (site_id, media_id);

-- inventory -----------------------------------------------------------------
CREATE INDEX idx_inventory_transaction_variant ON inventory_transaction (site_id, variant_id, created_at);
CREATE INDEX idx_inventory_transaction_reference ON inventory_transaction (site_id, reference_type, reference_id);
CREATE INDEX idx_inventory_reservation_variant ON inventory_reservation (site_id, variant_id, status);
CREATE INDEX idx_inventory_reservation_order  ON inventory_reservation (site_id, order_id);

-- cart ----------------------------------------------------------------------
CREATE INDEX idx_cart_item_variant            ON cart_item (site_id, variant_id);
CREATE INDEX idx_cart_expiry                  ON cart (expires_at) WHERE status = 'active'; -- reaper, cross-site

-- tax / shipping ------------------------------------------------------------
CREATE INDEX idx_tax_rate_lookup              ON tax_rate (site_id, tax_class_id, country_code, region_code, priority);
CREATE INDEX idx_shipping_zone_region_lookup  ON shipping_zone_region (site_id, country_code, region_code);
CREATE INDEX idx_shipping_rate_zone           ON shipping_rate (site_id, zone_id);

-- orders --------------------------------------------------------------------
CREATE INDEX idx_order_customer               ON "order" (site_id, customer_id, created_at DESC);
CREATE INDEX idx_order_fulfilment             ON "order" (site_id, fulfilment_status, created_at DESC);
CREATE INDEX idx_order_payment                ON "order" (site_id, payment_status, created_at DESC);
CREATE INDEX idx_order_item_order             ON order_item (site_id, order_id);
CREATE INDEX idx_order_item_variant           ON order_item (site_id, variant_id);
CREATE INDEX idx_order_item_product           ON order_item (site_id, product_id);
CREATE INDEX idx_order_status_history_order   ON order_status_history (site_id, order_id, created_at);
CREATE INDEX idx_shipment_order               ON shipment (site_id, order_id);
CREATE INDEX idx_shipment_item_item           ON shipment_item (site_id, order_item_id);

-- payments ------------------------------------------------------------------
CREATE INDEX idx_payment_order                ON payment (site_id, order_id);
CREATE INDEX idx_refund_order                 ON refund (site_id, order_id);

-- promotions ----------------------------------------------------------------
CREATE INDEX idx_promotion_status             ON promotion (site_id, status);
CREATE INDEX idx_coupon_code_promotion        ON coupon_code (site_id, promotion_id);
CREATE INDEX idx_coupon_code_customer         ON coupon_code (site_id, customer_id);
CREATE INDEX idx_promotion_redemption_customer ON promotion_redemption (site_id, customer_id, promotion_id);
CREATE INDEX idx_promotion_redemption_order   ON promotion_redemption (site_id, order_id);

-- customers / infra ---------------------------------------------------------
CREATE INDEX idx_customer_address_customer    ON customer_address (site_id, customer_id);
CREATE INDEX idx_customer_session_customer    ON customer_session (site_id, customer_id);
CREATE INDEX idx_customer_session_expiry      ON customer_session (expires_at); -- reaper, cross-site
CREATE INDEX idx_customer_code_open           ON customer_code (site_id, customer_id, purpose) WHERE used_at IS NULL;
CREATE INDEX idx_email_sender_pending         ON email_sender (checked_at) WHERE status = 'pending'; -- worker, cross-site
CREATE INDEX idx_outbox_event_unprocessed     ON outbox_event (created_at) WHERE published_at IS NULL; -- worker, cross-site
CREATE INDEX idx_idempotency_key_expiry       ON idempotency_key (expires_at);
