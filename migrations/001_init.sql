CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE wallets (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id    TEXT NOT NULL UNIQUE,
    balance     NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK(balance >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE transfers(
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key TEXT NOT NULL UNIQUE,
    from_wallet_id  UUID NOT NULL REFERENCES wallets(id),
    to_wallet_id    UUID NOT NULL REFERENCES wallets(id),
    amount          NUMERIC(20,8) NOT NULL CHECK(amount > 0),
    status          TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','PROCESSED','FAILED')),
    failure_reason  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE ledger_entries(
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    wallet_id   UUID NOT NULL REFERENCES wallets(id),
    transfer_id UUID NOT NULL REFERENCES transfers(id),
    type        TEXT NOT NULL CHECK (type IN ('DEBIT','CREDIT')),
    amount      NUMERIC(20,8) NOT NULL CHECK (amount > 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()

    CONSTRAINT unique_transfer_wallet_type
    UNIQUE (transfer_id, wallet_id, type)
);

CREATE INDEX idx_transfers_from_wallet ON transfers(from_wallet_id);
CREATE INDEX idx_transfers_to_wallet ON transfers(to_wallet_id);
CREATE INDEX idx_ledger_wallet ON ledger_entries(wallet_id);