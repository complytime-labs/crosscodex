-- Teardown for the initial schema, in reverse dependency order. Drops every
-- recorded judgement, identity event and tenant: use it only on a database
-- whose data may be lost.
--
-- Refused while identity_events holds any row: the audit log is permanent,
-- and every human judgement's reviewer was registered through it, so an
-- empty audit log also means no human judgement is lost. To decommission a
-- database that holds records, export them with 'crosscodexd admin identity
-- export --out <file>' and drop the database (DROP DATABASE never runs this
-- file) instead of tearing the schema down.

DO $$
DECLARE
    v_events bigint;
BEGIN
    IF to_regclass('public.identity_events') IS NOT NULL THEN
        SELECT count(*) INTO v_events FROM public.identity_events;
        IF v_events > 0 THEN
            RAISE EXCEPTION 'cannot tear down the schema: identity_events holds % audit records, which are permanent. To decommission this database, export them with crosscodexd admin identity export --out <file>, then drop the database instead of running this teardown', v_events;
        END IF;
    END IF;
END $$;

-- Graph lifecycle
DROP TRIGGER IF EXISTS tenant_graph_create ON public.tenants;
DROP FUNCTION IF EXISTS public.create_tenant_graph();
DROP FUNCTION IF EXISTS public.assert_tenant_graph(TEXT);
DROP FUNCTION IF EXISTS public.tenant_graph_name();

-- Revoke the ag_catalog-wide grants. These target pre-existing objects owned
-- by the age extension (not dropped here), so DROP ROLE fails with "cannot
-- be dropped because some objects depend on it" unless they are revoked
-- first.
REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA ag_catalog FROM graph_user;
REVOKE SELECT ON ALL TABLES IN SCHEMA ag_catalog FROM graph_user;
REVOKE USAGE ON SCHEMA ag_catalog FROM graph_user;

-- Restricted-role access
DROP FUNCTION IF EXISTS public.resumable_jobs();
DROP FUNCTION IF EXISTS public.require_active_reviewer(text);
DROP FUNCTION IF EXISTS public.enter_tenant(text);
DROP FUNCTION IF EXISTS public.tenant_is_active(text);
-- schema_migrations belongs to golang-migrate and outlives this teardown.
REVOKE SELECT ON schema_migrations FROM app_user;

-- Tenant administration
DROP FUNCTION IF EXISTS public.list_retention_tenants();
DROP FUNCTION IF EXISTS public.export_identity_events(bigint, int);
DROP FUNCTION IF EXISTS public.trace_pseudonym(text, text, text, bytea, bytea);
DROP FUNCTION IF EXISTS public.list_reviewers(text);
DROP FUNCTION IF EXISTS public.unhide_reviewer(text, text, text, text, bytea, bytea);
DROP FUNCTION IF EXISTS public.hide_reviewer(text, text, text, text, bytea, bytea);
DROP FUNCTION IF EXISTS public.restore_reviewer(text, text, text, text, bytea, bytea);
DROP FUNCTION IF EXISTS public.delete_reviewer(text, text, text, text, bytea, bytea);
DROP FUNCTION IF EXISTS public.add_reviewer(text, text, text, text, bytea, bytea);
DROP FUNCTION IF EXISTS public.unhide_tenant(text, text, text, bytea, bytea);
DROP FUNCTION IF EXISTS public.hide_tenant(text, text, text, bytea, bytea);
DROP FUNCTION IF EXISTS public.restore_tenant(text, text, text, bytea, bytea);
DROP FUNCTION IF EXISTS public.delete_tenant(text, text, text, bytea, bytea);
DROP FUNCTION IF EXISTS public.record_identity_event(text, text, text, text, text, text, text, bytea, bytea);
DROP FUNCTION IF EXISTS public.list_tenants();
DROP FUNCTION IF EXISTS public.set_tenant_status(text, text);
DROP FUNCTION IF EXISTS public.provision_tenant(text, text);

-- Tables (reverse dependency order). DROP TABLE fires no row or TRUNCATE
-- trigger, so the refusal triggers do not block it; they go with their
-- tables.
DROP TABLE IF EXISTS artifact_pair_verdicts;
DROP TABLE IF EXISTS retention_holds;
DROP TABLE IF EXISTS controls;
DROP TABLE IF EXISTS embeddings;
DROP TABLE IF EXISTS classifications;
DROP TABLE IF EXISTS catalogs;
DROP TABLE IF EXISTS relationship_candidates;
DROP TABLE IF EXISTS analysis_results;
DROP TABLE IF EXISTS requires_consensus;
DROP TABLE IF EXISTS requires_votes;
DROP TABLE IF EXISTS requires_candidates;
DROP TABLE IF EXISTS vote_summaries;
DROP TABLE IF EXISTS job_stages;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS reviewers;
DROP TABLE IF EXISTS identity_events;
DROP TABLE IF EXISTS tenants;

-- Trigger functions, now unreferenced
DROP FUNCTION IF EXISTS prevent_classification_mutation();
DROP FUNCTION IF EXISTS prevent_completed_job_child_delete();
DROP FUNCTION IF EXISTS prevent_completed_job_child_mutation();
DROP FUNCTION IF EXISTS prevent_completed_job_delete();
DROP FUNCTION IF EXISTS prevent_completed_job_mutation();
DROP FUNCTION IF EXISTS public.refuse_change();

-- Roles. A group role cannot be dropped while a grant references it, so the
-- schema grant and the memberships go first.
REVOKE USAGE ON SCHEMA public FROM purge_user;
REVOKE pg_read_all_settings FROM backup_user;
REVOKE retention_purge FROM purge_user;
DROP ROLE IF EXISTS tenant_admin;
DROP ROLE IF EXISTS backup_user;
DROP ROLE IF EXISTS purge_user;
DROP ROLE IF EXISTS retention_purge;
DROP ROLE IF EXISTS graph_user;
DROP ROLE IF EXISTS app_user;
