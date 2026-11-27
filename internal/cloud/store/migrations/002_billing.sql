-- 002_billing.sql: prepaid wallets and per-call billing.
--
-- Depends on 001_identity.sql: users(id uuid) PRIMARY KEY.
--
-- Every amount is int64 micro-CNY (1 CNY = 1,000,000 micros) and lives in a
-- bigint column. No numeric, real or double precision column holds money.
-- Token prices are micro-CNY per million tokens.
--
-- wallets.balance_micros may go below zero when a settlement's actual usage
-- exceeds its hold (a debt); wallets.held_micros never does. wallet_entries
-- is append-only: a trigger refuses UPDATE, DELETE and TRUNCATE. The ledger
-- and usage rows are financial records, so their user foreign keys do not
-- cascade: a user with records cannot be deleted until those records are
-- purged deliberately.

CREATE TABLE wallets (
	user_id        uuid        PRIMARY KEY REFERENCES users (id),
	balance_micros bigint      NOT NULL DEFAULT 0,
	held_micros    bigint      NOT NULL DEFAULT 0,
	version        bigint      NOT NULL DEFAULT 0,
	created_at     timestamptz NOT NULL DEFAULT now(),
	updated_at     timestamptz NOT NULL DEFAULT now(),
	CONSTRAINT wallets_held_check CHECK (held_micros >= 0),
	CONSTRAINT wallets_version_check CHECK (version >= 0)
);

COMMENT ON TABLE wallets IS 'One prepaid balance per user; available balance is balance_micros - held_micros.';
COMMENT ON COLUMN wallets.held_micros IS 'Sum of holds on reservations still in status = held.';
COMMENT ON COLUMN wallets.version IS 'Incremented by every write, for change detection.';

-- A reservation is one request's hold. request_id is the caller's
-- idempotency key: the first reservation for a (user, request_id) is
-- returned for every later call with the same pair. price_snapshot is the
-- resolved quote at reserve time, so a price change never reprices a
-- request already in flight.
CREATE TABLE billing_reservations (
	id              uuid        PRIMARY KEY,
	user_id         uuid        NOT NULL REFERENCES users (id),
	request_id      text        NOT NULL,
	status          text        NOT NULL DEFAULT 'held',
	channel         text        NOT NULL DEFAULT '',
	provider        text        NOT NULL,
	model           text        NOT NULL,
	price_snapshot  jsonb       NOT NULL,
	estimated_usage jsonb       NOT NULL,
	hold_micros     bigint      NOT NULL,
	charged_micros  bigint,
	expires_at      timestamptz NOT NULL,
	settled_at      timestamptz,
	refunded_at     timestamptz,
	created_at      timestamptz NOT NULL DEFAULT now(),
	updated_at      timestamptz NOT NULL DEFAULT now(),
	CONSTRAINT billing_reservations_request_id_check CHECK (request_id <> '' AND length(request_id) <= 200),
	CONSTRAINT billing_reservations_status_check CHECK (status IN ('held', 'settled', 'refunded')),
	CONSTRAINT billing_reservations_provider_check CHECK (provider <> ''),
	CONSTRAINT billing_reservations_model_check CHECK (model <> ''),
	CONSTRAINT billing_reservations_hold_check CHECK (hold_micros >= 0),
	CONSTRAINT billing_reservations_charged_check CHECK (charged_micros IS NULL OR charged_micros >= 0),
	CONSTRAINT billing_reservations_user_request_key UNIQUE (user_id, request_id)
);

COMMENT ON TABLE billing_reservations IS 'A durable hold for one request; settled or refunded exactly once.';
COMMENT ON COLUMN billing_reservations.request_id IS 'Caller idempotency key, unique per user and never reused.';
COMMENT ON COLUMN billing_reservations.price_snapshot IS 'pricing.Snapshot JSON fixed at reserve time.';
COMMENT ON COLUMN billing_reservations.expires_at IS 'A held reservation older than this is recoverable by RecoverStaleHolds.';

CREATE INDEX billing_reservations_stale_idx
	ON billing_reservations (expires_at, id)
	WHERE status = 'held';
CREATE INDEX billing_reservations_user_time_idx
	ON billing_reservations (user_id, created_at DESC, id);

-- The money ledger. Every row is immutable; the trigger below enforces it.
-- balance_delta_micros is the change to the user's spendable balance and
-- held_delta_micros the change to the held total. The after columns are the
-- wallet's state at the moment of the entry, for statements and audits.
--
-- reservation_id is deferrable because Reserve writes the hold entry first
-- and the reservation that names it second, in one transaction.
CREATE TABLE wallet_entries (
	id                    uuid        PRIMARY KEY,
	user_id               uuid        NOT NULL REFERENCES users (id),
	reservation_id        uuid        REFERENCES billing_reservations (id) DEFERRABLE INITIALLY DEFERRED,
	kind                  text        NOT NULL,
	balance_delta_micros  bigint      NOT NULL,
	held_delta_micros     bigint      NOT NULL,
	balance_after_micros  bigint      NOT NULL,
	held_after_micros     bigint      NOT NULL,
	idempotency_key       text        NOT NULL,
	description           text        NOT NULL DEFAULT '',
	created_at            timestamptz NOT NULL DEFAULT now(),
	CONSTRAINT wallet_entries_kind_check CHECK (kind IN ('credit', 'hold', 'settle', 'refund')),
	CONSTRAINT wallet_entries_shape_check CHECK (
		(kind = 'credit' AND reservation_id IS NULL AND balance_delta_micros > 0 AND held_delta_micros = 0)
		OR (kind = 'hold' AND reservation_id IS NOT NULL AND balance_delta_micros = 0 AND held_delta_micros >= 0)
		OR (kind = 'settle' AND reservation_id IS NOT NULL AND balance_delta_micros <= 0 AND held_delta_micros <= 0)
		OR (kind = 'refund' AND reservation_id IS NOT NULL AND balance_delta_micros = 0 AND held_delta_micros <= 0)
	),
	CONSTRAINT wallet_entries_held_after_check CHECK (held_after_micros >= 0),
	CONSTRAINT wallet_entries_idempotency_key_check CHECK (idempotency_key <> '' AND length(idempotency_key) <= 200),
	CONSTRAINT wallet_entries_description_check CHECK (length(description) <= 500),
	CONSTRAINT wallet_entries_idempotency_key_key UNIQUE (idempotency_key)
);

COMMENT ON TABLE wallet_entries IS 'Append-only money ledger; rows are never updated or deleted.';
COMMENT ON COLUMN wallet_entries.balance_delta_micros IS 'Signed change to wallets.balance_micros.';
COMMENT ON COLUMN wallet_entries.held_delta_micros IS 'Signed change to wallets.held_micros.';
COMMENT ON COLUMN wallet_entries.idempotency_key IS 'Globally unique: a payment replay or a reservation operation.';

-- One entry of each kind per reservation: a second settle or refund is
-- refused by the database even if two processes race.
CREATE UNIQUE INDEX wallet_entries_reservation_kind_idx
	ON wallet_entries (reservation_id, kind)
	WHERE reservation_id IS NOT NULL;
CREATE INDEX wallet_entries_user_time_idx
	ON wallet_entries (user_id, created_at DESC, id);

-- The hold entry must exist before its reservation is complete, but the
-- entry references the reservation. Reserve writes both in one transaction,
-- so the columns are added after the entry table and the entry's foreign
-- key is deferred.
ALTER TABLE billing_reservations
	ADD COLUMN hold_entry_id uuid NOT NULL,
	ADD COLUMN settlement_entry_id uuid,
	ADD COLUMN refund_entry_id uuid;

ALTER TABLE billing_reservations
	ADD CONSTRAINT billing_reservations_hold_entry_fkey
		FOREIGN KEY (hold_entry_id) REFERENCES wallet_entries (id),
	ADD CONSTRAINT billing_reservations_settlement_entry_fkey
		FOREIGN KEY (settlement_entry_id) REFERENCES wallet_entries (id),
	ADD CONSTRAINT billing_reservations_refund_entry_fkey
		FOREIGN KEY (refund_entry_id) REFERENCES wallet_entries (id);

-- A reservation's status and the fields that prove it stay in step.
ALTER TABLE billing_reservations
	ADD CONSTRAINT billing_reservations_status_shape_check CHECK (
		(status = 'held'
			AND charged_micros IS NULL
			AND settled_at IS NULL
			AND refunded_at IS NULL
			AND settlement_entry_id IS NULL
			AND refund_entry_id IS NULL)
		OR (status = 'settled'
			AND charged_micros IS NOT NULL
			AND settled_at IS NOT NULL
			AND refunded_at IS NULL
			AND settlement_entry_id IS NOT NULL
			AND refund_entry_id IS NULL)
		OR (status = 'refunded'
			AND charged_micros IS NULL
			AND refunded_at IS NOT NULL
			AND settled_at IS NULL
			AND settlement_entry_id IS NULL
			AND refund_entry_id IS NOT NULL)
	);

-- What a settled reservation actually used, at the snapshot price. The
-- unique reservation_id makes a replayed settlement unable to add a second
-- usage row.
CREATE TABLE usage_records (
	id                 uuid        PRIMARY KEY,
	user_id            uuid        NOT NULL REFERENCES users (id),
	reservation_id     uuid        NOT NULL UNIQUE REFERENCES billing_reservations (id),
	request_id         text        NOT NULL,
	channel            text        NOT NULL DEFAULT '',
	provider           text        NOT NULL,
	model              text        NOT NULL,
	input_tokens       bigint      NOT NULL DEFAULT 0,
	output_tokens      bigint      NOT NULL DEFAULT 0,
	cache_read_tokens  bigint      NOT NULL DEFAULT 0,
	cache_write_tokens bigint      NOT NULL DEFAULT 0,
	cost_micros        bigint      NOT NULL,
	price_snapshot     jsonb       NOT NULL,
	created_at         timestamptz NOT NULL DEFAULT now(),
	CONSTRAINT usage_records_request_id_check CHECK (request_id <> '' AND length(request_id) <= 200),
	CONSTRAINT usage_records_names_check CHECK (provider <> '' AND model <> ''),
	CONSTRAINT usage_records_tokens_check CHECK (
		input_tokens >= 0 AND output_tokens >= 0
		AND cache_read_tokens >= 0 AND cache_write_tokens >= 0
	),
	CONSTRAINT usage_records_cost_check CHECK (cost_micros >= 0)
);

COMMENT ON TABLE usage_records IS 'Per-request settled usage and its charge in micro-CNY.';
CREATE INDEX usage_records_user_time_idx
	ON usage_records (user_id, created_at DESC, id);

-- House channel prices. A row with zero rates is an explicit free price; an
-- absent row is an unknown price and pricing fails closed. Only one row per
-- (channel, provider, model) is current; closing one records its
-- effective_to and a new row becomes current.
CREATE TABLE house_channel_prices (
	id                       uuid        PRIMARY KEY,
	channel                  text        NOT NULL,
	provider                 text        NOT NULL,
	model                    text        NOT NULL,
	input_micros_per_mtok    bigint      NOT NULL,
	output_micros_per_mtok   bigint      NOT NULL,
	cache_read_micros_per_mtok  bigint   NOT NULL,
	cache_write_micros_per_mtok bigint   NOT NULL,
	fixed_per_call_micros    bigint      NOT NULL,
	currency                 text        NOT NULL DEFAULT 'CNY',
	effective_from           timestamptz NOT NULL DEFAULT now(),
	effective_to             timestamptz,
	created_at               timestamptz NOT NULL DEFAULT now(),
	updated_at               timestamptz NOT NULL DEFAULT now(),
	CONSTRAINT house_channel_prices_names_check CHECK (
		btrim(channel) <> '' AND btrim(provider) <> '' AND btrim(model) <> ''
	),
	CONSTRAINT house_channel_prices_rates_check CHECK (
		input_micros_per_mtok >= 0
		AND output_micros_per_mtok >= 0
		AND cache_read_micros_per_mtok >= 0
		AND cache_write_micros_per_mtok >= 0
		AND fixed_per_call_micros >= 0
	),
	CONSTRAINT house_channel_prices_currency_check CHECK (currency = 'CNY'),
	CONSTRAINT house_channel_prices_window_check CHECK (
		effective_to IS NULL OR effective_to > effective_from
	)
);

COMMENT ON TABLE house_channel_prices IS 'Versioned token prices in micro-CNY per million tokens.';
COMMENT ON COLUMN house_channel_prices.effective_to IS 'NULL marks the current price for its channel, provider and model.';

CREATE UNIQUE INDEX house_channel_prices_current_idx
	ON house_channel_prices (channel, provider, model)
	WHERE effective_to IS NULL;
CREATE UNIQUE INDEX house_channel_prices_version_idx
	ON house_channel_prices (channel, provider, model, effective_from);

CREATE OR REPLACE FUNCTION billing_wallet_entries_are_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
	RAISE EXCEPTION 'wallet_entries is append-only: % rejected', TG_OP;
END;
$$;

CREATE TRIGGER wallet_entries_append_only
	BEFORE UPDATE OR DELETE ON wallet_entries
	FOR EACH ROW
	EXECUTE FUNCTION billing_wallet_entries_are_append_only();

CREATE TRIGGER wallet_entries_reject_truncate
	BEFORE TRUNCATE ON wallet_entries
	FOR EACH STATEMENT
	EXECUTE FUNCTION billing_wallet_entries_are_append_only();
