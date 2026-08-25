-- Initial schema for the inventory reservation service.
--
-- Design note: stock.quantity stores PHYSICAL stock only. Available stock is
-- always computed as physical minus the sum of items on active, non-expired
-- reservations. There is no separate "reserved" counter to keep in sync, so
-- cancelling or expiring a reservation never has to mutate stock rows.

CREATE TABLE products (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE warehouses (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE stock (
    warehouse_id bigint NOT NULL REFERENCES warehouses (id),
    product_id   bigint NOT NULL REFERENCES products (id),
    quantity     bigint NOT NULL DEFAULT 0 CHECK (quantity >= 0),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (warehouse_id, product_id)
);

CREATE TYPE reservation_status AS ENUM ('active', 'confirmed', 'cancelled', 'expired');

CREATE TABLE reservations (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    warehouse_id    bigint NOT NULL REFERENCES warehouses (id),
    status          reservation_status NOT NULL DEFAULT 'active',
    -- Idempotency: one reservation per client-supplied key. request_hash lets
    -- us detect the same key being reused with a different payload.
    idempotency_key text NOT NULL UNIQUE,
    request_hash    text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    finalized_at    timestamptz
);

CREATE TABLE reservation_items (
    reservation_id uuid NOT NULL REFERENCES reservations (id) ON DELETE CASCADE,
    product_id     bigint NOT NULL REFERENCES products (id),
    quantity       bigint NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (reservation_id, product_id)
);

-- Supports the "reserved quantity per product" aggregation.
CREATE INDEX reservation_items_product_idx ON reservation_items (product_id);
-- Supports filtering active reservations by expiry.
CREATE INDEX reservations_status_expires_idx ON reservations (status, expires_at);
