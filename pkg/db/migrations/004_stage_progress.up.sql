-- Per-stage task progress for `run status` (issue #278).
-- One row per (job, stage) counts the stage's tasks as they finish, so an
-- operator can see done/total, failures, retries and cache hits while the job
-- runs. A stage that has not announced its size holds
-- tasks_total 0 until it does.
--
-- app_user is a restricted role so that a compromised daemon cannot truncate,
-- alter or bypass tenant isolation on this table: it may only read and write
-- rows of the tenant named by app.current_tenant. Unlike job_stages there is
-- no completed-job immutability trigger: progress is a live counter, and a
-- finished job's final counts are archived by retention.
CREATE TABLE IF NOT EXISTS job_stage_progress (
    tenant_id        TEXT NOT NULL REFERENCES tenants(tenant_id),
    job_id           TEXT NOT NULL,
    stage_name       TEXT NOT NULL,
    tasks_total      INTEGER NOT NULL DEFAULT 0 CHECK (tasks_total >= 0),
    tasks_done       INTEGER NOT NULL DEFAULT 0 CHECK (tasks_done >= 0),
    tasks_succeeded  INTEGER NOT NULL DEFAULT 0 CHECK (tasks_succeeded >= 0),
    tasks_failed     INTEGER NOT NULL DEFAULT 0 CHECK (tasks_failed >= 0),
    tasks_retried    INTEGER NOT NULL DEFAULT 0 CHECK (tasks_retried >= 0),
    tasks_cache_hits INTEGER NOT NULL DEFAULT 0 CHECK (tasks_cache_hits >= 0),
    last_success_at  TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, job_id, stage_name),
    CONSTRAINT job_stage_progress_done_check CHECK (tasks_done = tasks_succeeded + tasks_failed),
    -- Composite so the check names the row's own tenant: foreign key checks
    -- bypass RLS, so a job_id alone would let one tenant attach a row to
    -- another tenant's job.
    CONSTRAINT job_stage_progress_job_tenant_fkey
        FOREIGN KEY (job_id, tenant_id) REFERENCES public.jobs (job_id, tenant_id)
);

ALTER TABLE job_stage_progress ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON job_stage_progress
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON job_stage_progress TO app_user;

-- Retention purges the progress with its job (children first, as for
-- job_stages); purge_user needs only SELECT and DELETE.
GRANT SELECT, DELETE ON public.job_stage_progress TO purge_user;
