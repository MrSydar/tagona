CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS collections (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name TEXT NOT NULL UNIQUE,
    tagger_version TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_collections_name ON collections(name);
CREATE INDEX IF NOT EXISTS idx_collections_created_at_id ON collections(created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS objects (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    collection_id UUID NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    content_hash TEXT NOT NULL,
    date TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    size_bytes INT NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ,
    payload_key TEXT NOT NULL,
    UNIQUE(collection_id, content_hash)
);

CREATE INDEX IF NOT EXISTS idx_objects_collection_date_id ON objects(collection_id, date DESC, id ASC);
CREATE INDEX IF NOT EXISTS idx_objects_expires_at ON objects(expires_at);

CREATE TABLE IF NOT EXISTS object_tags (
    object_id UUID NOT NULL REFERENCES objects(id) ON DELETE CASCADE,
    collection_id UUID NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    tag TEXT NOT NULL,
    value BOOLEAN NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (object_id, tag)
);

CREATE INDEX IF NOT EXISTS idx_object_tags_query ON object_tags(collection_id, tag, value, object_id);
CREATE INDEX IF NOT EXISTS idx_object_tags_object_tag ON object_tags(object_id, tag);
