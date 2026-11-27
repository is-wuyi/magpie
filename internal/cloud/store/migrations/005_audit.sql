CREATE TABLE audit_logs (
    id uuid PRIMARY KEY,
    actor_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    action text NOT NULL,
    target_type text NOT NULL,
    target_id text NOT NULL,
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    request_ip inet,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (length(action) BETWEEN 1 AND 100),
    CHECK (length(target_type) BETWEEN 1 AND 50),
    CHECK (length(target_id) <= 200)
);
CREATE INDEX audit_logs_time_idx ON audit_logs(created_at DESC, id);
CREATE INDEX audit_logs_actor_time_idx ON audit_logs(actor_user_id, created_at DESC);
CREATE INDEX audit_logs_target_idx ON audit_logs(target_type, target_id, created_at DESC);
