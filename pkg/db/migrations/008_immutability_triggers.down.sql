DROP TRIGGER IF EXISTS classifications_immutable_delete ON classifications;
DROP TRIGGER IF EXISTS classifications_immutable_update ON classifications;
DROP FUNCTION IF EXISTS prevent_classification_mutation();

DROP TRIGGER IF EXISTS vote_summaries_immutable_delete ON vote_summaries;
DROP TRIGGER IF EXISTS vote_summaries_immutable_update ON vote_summaries;

DROP TRIGGER IF EXISTS job_stages_immutable_delete ON job_stages;
DROP TRIGGER IF EXISTS job_stages_immutable_update ON job_stages;

DROP FUNCTION IF EXISTS prevent_completed_job_child_delete();
DROP FUNCTION IF EXISTS prevent_completed_job_child_mutation();

DROP TRIGGER IF EXISTS jobs_immutable_delete ON jobs;
DROP TRIGGER IF EXISTS jobs_immutable_update ON jobs;

DROP FUNCTION IF EXISTS prevent_completed_job_delete();
DROP FUNCTION IF EXISTS prevent_completed_job_mutation();
