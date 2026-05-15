CREATE TABLE IF NOT EXISTS jobs (
    job_id     TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL REFERENCES tenants(tenant_id),
    status     TEXT NOT NULL DEFAULT 'pending',
    config     JSONB,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS job_stages (
    job_id       TEXT NOT NULL REFERENCES jobs(job_id),
    stage_name   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending',
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    tenant_id    TEXT NOT NULL REFERENCES tenants(tenant_id),
    PRIMARY KEY (job_id, stage_name)
);
