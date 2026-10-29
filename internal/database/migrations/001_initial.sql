CREATE TABLE images (
    id text PRIMARY KEY,
    job_id text NOT NULL UNIQUE,
    status text NOT NULL CHECK (status IN ('queued', 'processing', 'succeeded', 'failed')),
    format text NOT NULL,
    width integer NOT NULL CHECK (width > 0),
    height integer NOT NULL CHECK (height > 0),
    bytes bigint NOT NULL CHECK (bytes >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    content_hash text NOT NULL,
    idempotency_key text UNIQUE,
    profile jsonb NOT NULL,
    pipeline text NOT NULL
);
CREATE INDEX images_listing ON images (id);
CREATE INDEX images_status_listing ON images (status, id);

CREATE TABLE jobs (
    id text PRIMARY KEY,
    image_id text NOT NULL UNIQUE REFERENCES images(id) ON DELETE CASCADE,
    status text NOT NULL CHECK (status IN ('queued', 'processing', 'succeeded', 'failed')),
    generated integer NOT NULL DEFAULT 0 CHECK (generated >= 0),
    planned integer NOT NULL CHECK (planned > 0),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt BETWEEN 0 AND 3),
    token text NOT NULL DEFAULT '',
    lease_until timestamptz,
    error_code text NOT NULL DEFAULT '',
    error_message text NOT NULL DEFAULT '',
    processing_ms bigint NOT NULL DEFAULT 0 CHECK (processing_ms >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (generated <= planned)
);
CREATE INDEX jobs_claim ON jobs (created_at, id) WHERE status IN ('queued', 'processing');

CREATE TABLE assets (
    id text PRIMARY KEY,
    image_id text NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    format text NOT NULL,
    filename text NOT NULL UNIQUE,
    width integer NOT NULL CHECK (width > 0),
    height integer NOT NULL CHECK (height > 0),
    bytes bigint NOT NULL CHECK (bytes >= 0),
    duration_ms bigint NOT NULL CHECK (duration_ms >= 0)
);
CREATE INDEX assets_image ON assets (image_id, id);
