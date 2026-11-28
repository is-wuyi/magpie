-- Separate registration and password authentication from email-code login.
-- Existing users keep their passwordless sign-in until an operator disables it.

ALTER TABLE users
    ADD COLUMN password_hash text,
    ADD COLUMN password_hash_version smallint,
    ADD COLUMN password_updated_at timestamptz,
    ADD CONSTRAINT users_password_hash_length CHECK (
        password_hash IS NULL OR length(password_hash) BETWEEN 20 AND 512
    ),
    ADD CONSTRAINT users_password_hash_version_positive CHECK (
        password_hash_version IS NULL OR password_hash_version > 0
    ),
    ADD CONSTRAINT users_password_fields_complete CHECK (
        (password_hash IS NULL) = (password_hash_version IS NULL)
    );

CREATE TABLE auth_policy (
    id                      smallint    PRIMARY KEY DEFAULT 1,
    registration_enabled    boolean     NOT NULL,
    password_login_enabled  boolean     NOT NULL,
    code_login_enabled      boolean     NOT NULL,
    password_min_length     smallint    NOT NULL DEFAULT 12,
    version                 bigint      NOT NULL DEFAULT 1,
    updated_at              timestamptz NOT NULL DEFAULT now(),
    updated_by              uuid        REFERENCES users(id) ON DELETE SET NULL,

    CONSTRAINT auth_policy_singleton CHECK (id = 1),
    CONSTRAINT auth_policy_password_length CHECK (password_min_length BETWEEN 8 AND 72),
    CONSTRAINT auth_policy_login_available CHECK (password_login_enabled OR code_login_enabled),
    CONSTRAINT auth_policy_version_positive CHECK (version > 0)
);

INSERT INTO auth_policy (id, registration_enabled, password_login_enabled, code_login_enabled)
VALUES (1, true, true, true);

CREATE TABLE auth_attempts (
    id                uuid        PRIMARY KEY,
    kind              text        NOT NULL,
    email_normalized  text        NOT NULL,
    request_ip        inet,
    created_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT auth_attempts_kind_valid CHECK (kind = 'password'),
    CONSTRAINT auth_attempts_email_shape CHECK (
        email_normalized = lower(btrim(email_normalized))
        AND length(email_normalized) BETWEEN 3 AND 254
        AND position('@' in email_normalized) > 1
        AND position('@' in email_normalized) < length(email_normalized)
    )
);

CREATE INDEX auth_attempts_email_created
    ON auth_attempts (kind, email_normalized, created_at DESC);
CREATE INDEX auth_attempts_ip_created
    ON auth_attempts (kind, request_ip, created_at DESC)
    WHERE request_ip IS NOT NULL;
