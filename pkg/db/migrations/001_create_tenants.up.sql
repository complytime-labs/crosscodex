CREATE TABLE IF NOT EXISTS tenants (
    tenant_id    TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
