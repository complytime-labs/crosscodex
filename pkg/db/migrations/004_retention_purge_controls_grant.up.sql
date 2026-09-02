-- Catalog purge deletes controls (FK catalog_id → catalogs, no cascade).
-- purge_user already holds catalogs/classifications/embeddings (migration 001);
-- grant the remaining catalog dependent. controls has no immutability delete
-- trigger, so a grant alone suffices.
GRANT SELECT, DELETE ON public.controls TO purge_user;
