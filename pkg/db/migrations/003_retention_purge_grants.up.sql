-- Completed-job child rows blocked DELETE FROM jobs (FK, no cascade). purge_user
-- already holds jobs/job_stages/vote_summaries (migration 001); grant the two
-- remaining FK children so retention can purge a completed job with its children.
-- These tables have no immutability delete trigger, so a grant alone suffices.
GRANT SELECT, DELETE ON public.analysis_results        TO purge_user;
GRANT SELECT, DELETE ON public.relationship_candidates TO purge_user;
