CREATE TABLE receipt (
    id BIGSERIAL PRIMARY KEY,
    qr_code TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('complete', 'unavailable')),
    response JSONB,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE receipt_item (
    receipt_id BIGINT NOT NULL REFERENCES receipt(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    name TEXT NOT NULL,
    quantity NUMERIC,
    price NUMERIC,
    amount NUMERIC,
    PRIMARY KEY (receipt_id, position)
);
