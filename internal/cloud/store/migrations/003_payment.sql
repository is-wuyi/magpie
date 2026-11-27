-- 003_payment.sql: recharge orders and verified payment notifications.
--
-- Depends on 001_identity.sql: users(id uuid) PRIMARY KEY.
--
-- Money is stored as integers only. amount_fen is what Alipay and WeChat Pay
-- bill (CNY cents); amount_micro_cny is what the wallet credits (1 CNY =
-- 1,000,000 micros, so 1 fen = 10,000 micros). A CHECK keeps the two in step,
-- and no numeric, real or double precision column holds money.
--
-- A payment order is created by the server from one of the deployment's
-- amount presets, never from a client-supplied amount. It moves
-- pending -> success exactly once, and only a success order may be credited.
-- A pending order can also become failed (the provider closed it) or expired
-- (the deployment closed it with the provider first).
--
-- This file stores no private key, API v3 key, provider secret or raw
-- callback body. payment_events keeps the SHA-256 digest of the signed
-- notification plus the non-secret fields needed to audit a credit. Only a
-- notification whose signature verified can be stored: the
-- signature_verified CHECK makes that a database rule, not only a code path.

CREATE TABLE payment_orders (
    id                uuid        PRIMARY KEY,
    merchant_order_no text        NOT NULL,
    user_id           uuid        NOT NULL REFERENCES users (id),
    provider          text        NOT NULL,
    provider_trade_no text,
    amount_fen        bigint      NOT NULL,
    amount_micro_cny  bigint      NOT NULL,
    currency          text        NOT NULL DEFAULT 'CNY',
    status            text        NOT NULL DEFAULT 'pending',
    credit_state      text        NOT NULL DEFAULT 'none',
    subject           text        NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,
    paid_at           timestamptz,
    credited_at       timestamptz,

    CONSTRAINT payment_orders_merchant_order_no_key UNIQUE (merchant_order_no),
    -- The pair lets payment_events prove that its order id and merchant
    -- order number name the same order.
    CONSTRAINT payment_orders_id_merchant_key UNIQUE (id, merchant_order_no),
    CONSTRAINT payment_orders_merchant_order_no_shape CHECK (
        length(merchant_order_no) BETWEEN 6 AND 32
        AND merchant_order_no ~ '^[0-9A-Za-z_|*-]+$'
    ),
    CONSTRAINT payment_orders_provider_valid CHECK (provider IN ('alipay', 'wechat')),
    CONSTRAINT payment_orders_status_valid CHECK (
        status IN ('pending', 'success', 'failed', 'expired')
    ),
    CONSTRAINT payment_orders_credit_state_valid CHECK (
        credit_state IN ('none', 'pending', 'credited')
    ),
    CONSTRAINT payment_orders_currency_cny CHECK (currency = 'CNY'),
    CONSTRAINT payment_orders_amount_positive CHECK (amount_fen > 0),
    CONSTRAINT payment_orders_amount_bounded CHECK (amount_fen <= 1000000000),
    CONSTRAINT payment_orders_amount_micro_matches CHECK (
        amount_micro_cny = amount_fen * 10000
    ),
    CONSTRAINT payment_orders_subject_shape CHECK (
        btrim(subject) <> '' AND length(subject) <= 127
    ),
    CONSTRAINT payment_orders_trade_no_shape CHECK (
        provider_trade_no IS NULL
        OR (length(provider_trade_no) BETWEEN 1 AND 64)
    ),
    CONSTRAINT payment_orders_paid_has_time CHECK (
        status <> 'success' OR paid_at IS NOT NULL
    ),
    CONSTRAINT payment_orders_paid_time_shape CHECK (
        paid_at IS NULL OR status IN ('success', 'failed', 'expired')
    ),
    CONSTRAINT payment_orders_credited_has_time CHECK (
        credit_state <> 'credited' OR credited_at IS NOT NULL
    ),
    CONSTRAINT payment_orders_credited_time_shape CHECK (
        credited_at IS NULL OR credit_state = 'credited'
    ),
    CONSTRAINT payment_orders_credited_is_success CHECK (
        credit_state <> 'credited' OR status = 'success'
    ),
    CONSTRAINT payment_orders_credit_only_after_paid CHECK (
        credit_state = 'none' OR status = 'success'
    ),
    CONSTRAINT payment_orders_success_owes_credit CHECK (
        status <> 'success' OR credit_state <> 'none'
    ),
    CONSTRAINT payment_orders_success_has_trade_no CHECK (
        status <> 'success' OR provider_trade_no IS NOT NULL
    )
);

COMMENT ON TABLE payment_orders IS 'One server-priced recharge; pending -> success at most once.';
COMMENT ON COLUMN payment_orders.merchant_order_no IS 'out_trade_no sent to the provider; unique and never reused.';
COMMENT ON COLUMN payment_orders.provider_trade_no IS 'Provider transaction id; unique per provider when present.';
COMMENT ON COLUMN payment_orders.amount_fen IS 'Amount billed in CNY cents.';
COMMENT ON COLUMN payment_orders.amount_micro_cny IS 'Wallet credit in micro-CNY; always amount_fen * 10000.';
COMMENT ON COLUMN payment_orders.credit_state IS 'Whether the wallet credit for a success order is still owed (pending) or done (credited).';

-- A provider transaction id may pay exactly one order. NULL is allowed for
-- pending orders; PostgreSQL's UNIQUE would allow many NULLs anyway, but the
-- partial index also keeps the index small.
CREATE UNIQUE INDEX payment_orders_provider_trade_no_idx
    ON payment_orders (provider, provider_trade_no)
    WHERE provider_trade_no IS NOT NULL;

CREATE INDEX payment_orders_user_created_idx
    ON payment_orders (user_id, created_at DESC, id);

-- The retry job scans oldest unpaid credits first.
CREATE INDEX payment_orders_pending_credit_idx
    ON payment_orders (updated_at, id)
    WHERE status = 'success' AND credit_state = 'pending';

-- Closing unpaid orders before their expiry.
CREATE INDEX payment_orders_pending_expiry_idx
    ON payment_orders (expires_at, id)
    WHERE status = 'pending';

-- An order's identity and amount never change, and its status only moves
-- through the documented state machine:
--   pending -> success | failed | expired
--   success -> success (the credit fields advance)
--   failed, expired -> unchanged
-- A provider trade number, once recorded, is immutable; a credited order
-- never loses its credit. The application enforces this too; the trigger
-- makes a direct UPDATE unable to bypass it.
CREATE OR REPLACE FUNCTION payment_orders_keep_identity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.id <> OLD.id
        OR NEW.merchant_order_no <> OLD.merchant_order_no
        OR NEW.user_id <> OLD.user_id
        OR NEW.provider <> OLD.provider
        OR NEW.amount_fen <> OLD.amount_fen
        OR NEW.amount_micro_cny <> OLD.amount_micro_cny
        OR NEW.currency <> OLD.currency
        OR NEW.subject <> OLD.subject
        OR NEW.created_at <> OLD.created_at
        OR NEW.expires_at <> OLD.expires_at
    THEN
        RAISE EXCEPTION 'payment_orders identity and amount are immutable';
    END IF;
    IF OLD.status = 'success' AND NEW.status <> 'success' THEN
        RAISE EXCEPTION 'a successful payment_orders row cannot leave success';
    END IF;
    IF OLD.status IN ('failed', 'expired') AND NEW.status <> OLD.status THEN
        RAISE EXCEPTION 'a failed or expired payment_orders row is terminal';
    END IF;
    IF OLD.credit_state = 'credited' AND NEW.credit_state <> 'credited' THEN
        RAISE EXCEPTION 'a credited payment_orders row cannot lose its credit';
    END IF;
    IF OLD.provider_trade_no IS NOT NULL
        AND NEW.provider_trade_no IS DISTINCT FROM OLD.provider_trade_no
    THEN
        RAISE EXCEPTION 'a recorded provider_trade_no is immutable';
    END IF;
    IF OLD.paid_at IS NOT NULL AND NEW.paid_at IS DISTINCT FROM OLD.paid_at THEN
        RAISE EXCEPTION 'a recorded paid_at is immutable';
    END IF;
    IF OLD.credited_at IS NOT NULL AND NEW.credited_at IS DISTINCT FROM OLD.credited_at THEN
        RAISE EXCEPTION 'a recorded credited_at is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER payment_orders_keep_identity
    BEFORE UPDATE ON payment_orders
    FOR EACH ROW
    EXECUTE FUNCTION payment_orders_keep_identity();

CREATE TABLE payment_events (
    id                 uuid        PRIMARY KEY,
    provider           text        NOT NULL,
    provider_event_id  text        NOT NULL,
    order_id           uuid        NOT NULL,
    merchant_order_no  text        NOT NULL,
    event_type         text        NOT NULL,
    provider_trade_no  text,
    amount_fen         bigint,
    signature_verified boolean     NOT NULL,
    payload_sha256     text        NOT NULL,
    outcome            text        NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT payment_events_provider_event_key UNIQUE (provider, provider_event_id),
    CONSTRAINT payment_events_order_fkey FOREIGN KEY (order_id, merchant_order_no)
        REFERENCES payment_orders (id, merchant_order_no),
    CONSTRAINT payment_events_provider_valid CHECK (provider IN ('alipay', 'wechat')),
    CONSTRAINT payment_events_event_id_shape CHECK (
        length(provider_event_id) BETWEEN 1 AND 128
    ),
    CONSTRAINT payment_events_event_type_shape CHECK (
        btrim(event_type) <> '' AND length(event_type) <= 64
    ),
    CONSTRAINT payment_events_amount_positive CHECK (
        amount_fen IS NULL OR (amount_fen > 0 AND amount_fen <= 1000000000)
    ),
    CONSTRAINT payment_events_trade_no_shape CHECK (
        provider_trade_no IS NULL
        OR (length(provider_trade_no) BETWEEN 1 AND 64)
    ),
    CONSTRAINT payment_events_digest_shape CHECK (
        payload_sha256 ~ '^[0-9a-f]{64}$'
    ),
    CONSTRAINT payment_events_outcome_valid CHECK (
        outcome IN ('credit_pending', 'credited', 'not_paid', 'late', 'duplicate')
    ),
    CONSTRAINT payment_events_signature_verified CHECK (signature_verified)
);

COMMENT ON TABLE payment_events IS 'Verified provider notifications; the unique pair makes a replay a no-op.';
COMMENT ON COLUMN payment_events.provider_event_id IS 'Alipay notify_id (or its signed-content digest) / WeChat event id.';
COMMENT ON COLUMN payment_events.payload_sha256 IS 'SHA-256 of the signed content; the raw callback is not stored.';
COMMENT ON COLUMN payment_events.outcome IS 'What this notification did to the order; credit_pending means the wallet credit is still owed.';

CREATE INDEX payment_events_order_idx
    ON payment_events (order_id, created_at, id);

-- A payment event is the audit record of one verified notification: its
-- identity, order, amount, digest and provider fields never change. Only the
-- outcome may advance (credit_pending -> credited); credited, late, duplicate
-- and not_paid are final. Events are append-only, so a direct UPDATE or
-- DELETE cannot rewrite a credit's evidence.
CREATE OR REPLACE FUNCTION payment_events_keep_audit()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.id <> OLD.id
        OR NEW.provider <> OLD.provider
        OR NEW.provider_event_id <> OLD.provider_event_id
        OR NEW.order_id <> OLD.order_id
        OR NEW.merchant_order_no <> OLD.merchant_order_no
        OR NEW.event_type <> OLD.event_type
        OR NEW.provider_trade_no IS DISTINCT FROM OLD.provider_trade_no
        OR NEW.amount_fen IS DISTINCT FROM OLD.amount_fen
        OR NEW.signature_verified <> OLD.signature_verified
        OR NEW.payload_sha256 <> OLD.payload_sha256
        OR NEW.created_at <> OLD.created_at
    THEN
        RAISE EXCEPTION 'payment_events audit fields are immutable';
    END IF;
    IF OLD.outcome = 'credit_pending' AND NEW.outcome NOT IN ('credit_pending', 'credited') THEN
        RAISE EXCEPTION 'an owed payment event can only become credited';
    END IF;
    IF OLD.outcome = 'credited' AND NEW.outcome <> 'credited' THEN
        RAISE EXCEPTION 'a credited payment event cannot lose its outcome';
    END IF;
    IF OLD.outcome IN ('late', 'duplicate', 'not_paid') AND NEW.outcome <> OLD.outcome THEN
        RAISE EXCEPTION 'a final payment event outcome is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER payment_events_keep_audit
    BEFORE UPDATE ON payment_events
    FOR EACH ROW
    EXECUTE FUNCTION payment_events_keep_audit();

CREATE OR REPLACE FUNCTION payment_events_are_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'payment_events is append-only: % rejected', TG_OP;
END;
$$;

CREATE TRIGGER payment_events_reject_delete
    BEFORE DELETE ON payment_events
    FOR EACH ROW
    EXECUTE FUNCTION payment_events_are_append_only();

CREATE TRIGGER payment_events_reject_truncate
    BEFORE TRUNCATE ON payment_events
    FOR EACH STATEMENT
    EXECUTE FUNCTION payment_events_are_append_only();
