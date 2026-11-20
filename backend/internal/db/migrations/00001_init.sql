-- +goose Up

-- Players. One account per (provider, external id). Dev logins use provider 'dev'.
CREATE TABLE users (
    id             BIGSERIAL PRIMARY KEY,
    oauth_provider TEXT        NOT NULL,
    oauth_id       TEXT        NOT NULL,
    name           TEXT        NOT NULL UNIQUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (oauth_provider, oauth_id)
);

-- Ledger accounts. Every user has one; the system has mint, burn and market_reserve.
-- System accounts have NO cached balance row on purpose: caching them would make
-- every trade in the game contend on the same row. Their balance is derived from entries.
CREATE TABLE accounts (
    id      BIGSERIAL PRIMARY KEY,
    kind    TEXT   NOT NULL CHECK (kind IN ('user', 'system')),
    user_id BIGINT UNIQUE REFERENCES users (id),
    name    TEXT   NOT NULL UNIQUE,
    CHECK ((kind = 'user') = (user_id IS NOT NULL))
);

INSERT INTO accounts (kind, name) VALUES
    ('system', 'system:mint'),
    ('system', 'system:burn'),
    ('system', 'system:market_reserve');

-- Cached spendable balance per user. Must always equal the sum of that user's ledger entries
-- (checked by reconciliation). The CHECK is a last line of defence against overspending.
CREATE TABLE wallets (
    user_id    BIGINT PRIMARY KEY REFERENCES users (id),
    account_id BIGINT NOT NULL UNIQUE REFERENCES accounts (id),
    balance    BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0)
);

-- One row per money movement (signup grant, buy, sell, ...).
CREATE TABLE ledger_txns (
    id         BIGSERIAL PRIMARY KEY,
    kind       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Double-entry lines. For every txn the amounts sum to zero (enforced below).
CREATE TABLE ledger_entries (
    id         BIGSERIAL PRIMARY KEY,
    txn_id     BIGINT      NOT NULL REFERENCES ledger_txns (id),
    account_id BIGINT      NOT NULL REFERENCES accounts (id),
    amount     BIGINT      NOT NULL CHECK (amount <> 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id);
CREATE INDEX ledger_entries_txn_idx ON ledger_entries (txn_id);

-- +goose StatementBegin
-- Checked at COMMIT (deferred), so all entries of a txn can be inserted first.
CREATE FUNCTION ledger_txn_must_balance() RETURNS trigger AS $$
DECLARE
    total BIGINT;
BEGIN
    SELECT COALESCE(SUM(amount), 0) INTO total FROM ledger_entries WHERE txn_id = NEW.txn_id;
    IF total <> 0 THEN
        RAISE EXCEPTION 'ledger txn % does not balance (sum = %)', NEW.txn_id, total
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_txn_must_balance();

-- +goose StatementBegin
-- The ledger is append-only: history can never be edited or deleted.
CREATE FUNCTION ledger_is_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ledger tables are append-only' USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_is_append_only();
CREATE TRIGGER ledger_txns_append_only
    BEFORE UPDATE OR DELETE ON ledger_txns
    FOR EACH ROW EXECUTE FUNCTION ledger_is_append_only();

-- Tradable cards. price = base + slope * circulating.
-- slope is even so the curve area slope*(s1^2 - s0^2)/2 is always an exact integer.
-- Hard limits keep all curve maths far away from int64 overflow.
CREATE TABLE cards (
    id          BIGSERIAL PRIMARY KEY,
    slug        TEXT        NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    name        TEXT        NOT NULL,
    creator_id  BIGINT REFERENCES users (id),
    supply_type TEXT        NOT NULL CHECK (supply_type IN ('fixed', 'unlimited')),
    max_supply  BIGINT CHECK (max_supply > 0 AND max_supply <= 1000000),
    circulating BIGINT      NOT NULL DEFAULT 0 CHECK (circulating >= 0 AND circulating <= 1000000),
    base        BIGINT      NOT NULL CHECK (base > 0 AND base <= 1000000),
    slope       BIGINT      NOT NULL CHECK (slope >= 0 AND slope <= 10000 AND slope % 2 = 0),
    royalty_bps INT         NOT NULL DEFAULT 0 CHECK (royalty_bps BETWEEN 0 AND 1000),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((supply_type = 'fixed') = (max_supply IS NOT NULL)),
    CHECK (max_supply IS NULL OR circulating <= max_supply)
);

-- What each player owns. cost_basis is the total coins paid for the current qty
-- (average cost = cost_basis / qty), which avoids rounding drift from storing an average.
CREATE TABLE holdings (
    user_id    BIGINT NOT NULL REFERENCES users (id),
    card_id    BIGINT NOT NULL REFERENCES cards (id),
    qty        BIGINT NOT NULL CHECK (qty >= 0),
    cost_basis BIGINT NOT NULL CHECK (cost_basis >= 0),
    PRIMARY KEY (user_id, card_id)
);
CREATE INDEX holdings_card_idx ON holdings (card_id) WHERE qty > 0;

-- Every executed trade. UNIQUE (user_id, idempotency_key) makes retries and double-clicks no-ops.
CREATE TABLE trades (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT      NOT NULL REFERENCES users (id),
    card_id         BIGINT      NOT NULL REFERENCES cards (id),
    side            TEXT        NOT NULL CHECK (side IN ('buy', 'sell')),
    qty             BIGINT      NOT NULL CHECK (qty > 0),
    amount          BIGINT      NOT NULL CHECK (amount > 0), -- area under the curve
    fee             BIGINT      NOT NULL CHECK (fee >= 0),   -- burned
    royalty         BIGINT      NOT NULL DEFAULT 0 CHECK (royalty >= 0),
    total           BIGINT      NOT NULL,                    -- coins paid (buy) or received (sell)
    price_after     BIGINT      NOT NULL,
    idempotency_key TEXT        NOT NULL CHECK (length(idempotency_key) BETWEEN 8 AND 100),
    ledger_txn_id   BIGINT      NOT NULL REFERENCES ledger_txns (id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, idempotency_key)
);
CREATE INDEX trades_card_time_idx ON trades (card_id, created_at DESC);

-- +goose Down
DROP TABLE trades;
DROP TABLE holdings;
DROP TABLE cards;
DROP TABLE ledger_entries;
DROP TABLE ledger_txns;
DROP FUNCTION ledger_txn_must_balance;
DROP FUNCTION ledger_is_append_only;
DROP TABLE wallets;
DROP TABLE accounts;
DROP TABLE users;
