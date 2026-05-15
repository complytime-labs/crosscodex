CREATE TABLE IF NOT EXISTS vote_summaries (
    job_id     TEXT NOT NULL REFERENCES jobs(job_id),
    source_id  TEXT NOT NULL,
    target_id  TEXT NOT NULL,
    consensus  TEXT NOT NULL,
    confidence DOUBLE PRECISION NOT NULL,
    viability  DOUBLE PRECISION NOT NULL,
    tenant_id  TEXT NOT NULL REFERENCES tenants(tenant_id),
    PRIMARY KEY (job_id, source_id, target_id)
);
