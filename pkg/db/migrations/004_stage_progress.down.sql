-- Reverses 004_stage_progress.up.sql. Per-stage task progress is lost; the jobs
-- and job_stages rows are untouched. Dropping a table also drops its policy,
-- grants and constraints.
DROP TABLE IF EXISTS job_stage_progress;
