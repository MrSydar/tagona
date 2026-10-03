-- Per-collection statistics maintained by triggers:
--   collections.object_count  number of objects in the collection
--   collection_tags           registry of tags seen in a collection, with the
--                             number of objects each tag is known true/false for
--
-- Migrations are re-run on every startup, so everything here is idempotent. The
-- one-off backfill runs only when the schema is first created, under a lock
-- that blocks concurrent writes; the whole file executes in a single
-- transaction, so the triggers below take over atomically.
DO $migrate$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'collections'
          AND column_name = 'object_count'
    ) THEN
        LOCK TABLE objects, object_tags IN SHARE ROW EXCLUSIVE MODE;

        ALTER TABLE collections ADD COLUMN object_count BIGINT NOT NULL DEFAULT 0;

        CREATE TABLE IF NOT EXISTS collection_tags (
            collection_id UUID NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
            tag TEXT NOT NULL,
            true_count BIGINT NOT NULL DEFAULT 0,
            false_count BIGINT NOT NULL DEFAULT 0,
            first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
            PRIMARY KEY (collection_id, tag)
        );

        UPDATE collections c
        SET object_count = (SELECT count(*) FROM objects o WHERE o.collection_id = c.id);

        INSERT INTO collection_tags (collection_id, tag, true_count, false_count, first_seen_at)
        SELECT collection_id, tag,
               count(*) FILTER (WHERE value),
               count(*) FILTER (WHERE NOT value),
               min(updated_at)
        FROM object_tags
        GROUP BY collection_id, tag;
    END IF;
END
$migrate$;

-- Statement-level triggers with transition tables aggregate a statement's rows
-- into one counter update per (collection, tag). Rows are locked in sorted
-- order so concurrent writers cannot deadlock on counters. Decrements only
-- UPDATE existing rows, so cascading deletes of a collection stay harmless.

CREATE OR REPLACE FUNCTION tagona_object_tags_inserted() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO collection_tags (collection_id, tag, true_count, false_count)
    SELECT collection_id, tag,
           count(*) FILTER (WHERE value),
           count(*) FILTER (WHERE NOT value)
    FROM new_rows
    GROUP BY collection_id, tag
    ORDER BY collection_id, tag
    ON CONFLICT (collection_id, tag) DO UPDATE
    SET true_count = collection_tags.true_count + EXCLUDED.true_count,
        false_count = collection_tags.false_count + EXCLUDED.false_count;
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION tagona_object_tags_updated() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM collection_tags ct
    WHERE (ct.collection_id, ct.tag) IN (SELECT collection_id, tag FROM new_rows)
    ORDER BY ct.collection_id, ct.tag
    FOR UPDATE;

    UPDATE collection_tags ct
    SET true_count = ct.true_count + d.t,
        false_count = ct.false_count + d.f
    FROM (
        SELECT collection_id, tag, sum(t) AS t, sum(f) AS f
        FROM (
            SELECT collection_id, tag, -(value::int) AS t, -((NOT value)::int) AS f FROM old_rows
            UNION ALL
            SELECT collection_id, tag, value::int, (NOT value)::int FROM new_rows
        ) x
        GROUP BY collection_id, tag
    ) d
    WHERE ct.collection_id = d.collection_id AND ct.tag = d.tag;
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION tagona_object_tags_deleted() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM collection_tags ct
    WHERE (ct.collection_id, ct.tag) IN (SELECT collection_id, tag FROM old_rows)
    ORDER BY ct.collection_id, ct.tag
    FOR UPDATE;

    UPDATE collection_tags ct
    SET true_count = ct.true_count - d.t,
        false_count = ct.false_count - d.f
    FROM (
        SELECT collection_id, tag,
               count(*) FILTER (WHERE value) AS t,
               count(*) FILTER (WHERE NOT value) AS f
        FROM old_rows
        GROUP BY collection_id, tag
    ) d
    WHERE ct.collection_id = d.collection_id AND ct.tag = d.tag;
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION tagona_objects_inserted() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    UPDATE collections c
    SET object_count = c.object_count + d.n
    FROM (SELECT collection_id, count(*) AS n FROM new_rows GROUP BY collection_id) d
    WHERE c.id = d.collection_id;
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION tagona_objects_deleted() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    UPDATE collections c
    SET object_count = c.object_count - d.n
    FROM (SELECT collection_id, count(*) AS n FROM old_rows GROUP BY collection_id) d
    WHERE c.id = d.collection_id;
    RETURN NULL;
END
$$;

CREATE OR REPLACE TRIGGER tagona_object_tags_inserted
AFTER INSERT ON object_tags
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION tagona_object_tags_inserted();

CREATE OR REPLACE TRIGGER tagona_object_tags_updated
AFTER UPDATE ON object_tags
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION tagona_object_tags_updated();

CREATE OR REPLACE TRIGGER tagona_object_tags_deleted
AFTER DELETE ON object_tags
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT EXECUTE FUNCTION tagona_object_tags_deleted();

CREATE OR REPLACE TRIGGER tagona_objects_inserted
AFTER INSERT ON objects
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION tagona_objects_inserted();

CREATE OR REPLACE TRIGGER tagona_objects_deleted
AFTER DELETE ON objects
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT EXECUTE FUNCTION tagona_objects_deleted();
