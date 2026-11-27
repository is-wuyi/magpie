CREATE TABLE house_channels (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'offline')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE providers (
    id uuid PRIMARY KEY,
    public_id text NOT NULL UNIQUE,
    owner_user_id uuid REFERENCES users(id) ON DELETE CASCADE,
    house_channel_id uuid REFERENCES house_channels(id) ON DELETE CASCADE,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((owner_user_id IS NULL) <> (house_channel_id IS NULL)),
    CHECK (public_id = lower(public_id) AND length(public_id) BETWEEN 3 AND 240)
);
CREATE INDEX providers_owner_idx ON providers(owner_user_id);
CREATE INDEX providers_house_idx ON providers(house_channel_id);

CREATE TABLE provider_secrets (
    provider_id uuid PRIMARY KEY REFERENCES providers(id) ON DELETE CASCADE,
    config_ciphertext bytea NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE channel_grants (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id uuid NOT NULL REFERENCES house_channels(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, channel_id)
);
CREATE INDEX channel_grants_channel_idx ON channel_grants(channel_id);

CREATE TABLE provider_groups (
    id uuid PRIMARY KEY,
    public_id text NOT NULL UNIQUE,
    owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (public_id = lower(public_id) AND length(public_id) BETWEEN 3 AND 240)
);
CREATE INDEX provider_groups_owner_idx ON provider_groups(owner_user_id);

CREATE TABLE provider_group_secrets (
    group_id uuid PRIMARY KEY REFERENCES provider_groups(id) ON DELETE CASCADE,
    config_ciphertext bytea NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name text NOT NULL,
    secret_hash bytea NOT NULL UNIQUE,
    masked text NOT NULL,
    models jsonb NOT NULL DEFAULT '[]'::jsonb,
    disabled boolean NOT NULL DEFAULT false,
    expires_at timestamptz,
    max_concurrency integer NOT NULL DEFAULT 0 CHECK (max_concurrency BETWEEN 0 AND 1000),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);
CREATE INDEX api_keys_user_idx ON api_keys(user_id);

CREATE TABLE api_key_channels (
    key_id uuid NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    channel_id uuid NOT NULL REFERENCES house_channels(id) ON DELETE CASCADE,
    PRIMARY KEY (key_id, channel_id)
);
