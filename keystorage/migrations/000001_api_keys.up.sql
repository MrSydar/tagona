-- The table lives in the schema this service owns (see KEYSTORAGE_PG_SCHEMA, default "keys"); the
-- connection's search_path points there, so names are unqualified. Idempotent: it runs on every start.
-- gen_random_uuid() is built in, so no extension (and no access to the public schema) is needed.
CREATE TABLE IF NOT EXISTS api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key_hash TEXT NOT NULL UNIQUE,
    key_prefix TEXT NOT NULL,
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
