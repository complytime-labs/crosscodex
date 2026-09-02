-- requires_candidates holds per-job relationship candidates (associated by
-- job_id column, no FK). It is archived and purged as part of the completed-job
-- aggregate, so purge_user needs DELETE. The table has no immutability delete
-- trigger, so a grant alone suffices.
GRANT SELECT, DELETE ON public.requires_candidates TO purge_user;
