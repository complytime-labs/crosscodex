ALTER TABLE catalogs
    DROP COLUMN IF EXISTS extractor_version,
    DROP COLUMN IF EXISTS extractor_name,
    DROP COLUMN IF EXISTS output_hash,
    DROP COLUMN IF EXISTS format,
    DROP COLUMN IF EXISTS content_size,
    DROP COLUMN IF EXISTS content_hash,
    DROP COLUMN IF EXISTS source_uri;

DROP POLICY IF EXISTS tenant_isolation ON controls;
DROP TABLE IF EXISTS controls;
