-- Per-collection statistics maintained by triggers:
--   collections.object_count     number of objects in the collection
--   collection_tags              registry of tags seen in a collection; a row is also the lock that serializes the
--                                writers of one tag's counters
--   collection_tag_buckets       how many objects have a score in each bucket, per tag: the distribution of the
--                                tag's scores, from which the API derives the number of evaluated objects and
--                                the percentiles
--
-- A score falls in one of 22 buckets: 0 is exactly 0, 21 is exactly 1 (grep, forced tags and yes/no taggers
-- produce nothing else), and 1..20 are the twenty bins of width 0.05 in between, from 0 (exclusive) to 1
-- (exclusive).
--
-- Migrations are re-run on every startup, so everything here is idempotent. The one-off backfill runs only when
-- the schema is first created, under a lock that blocks concurrent writes; the whole file executes in a single
-- transaction, so the triggers below take over atomically.

CREATE OR REPLACE FUNCTION tagona_score_bucket(score DOUBLE PRECISION) RETURNS SMALLINT
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT CASE
        WHEN score <= 0 THEN 0
        WHEN score >= 1 THEN 21
        ELSE 1 + LEAST(floor(score * 20)::int, 19)
    END::smallint
$$;

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
            first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
            PRIMARY KEY (collection_id, tag)
        );

        CREATE TABLE IF NOT EXISTS collection_tag_buckets (
            collection_id UUID NOT NULL,
            tag TEXT NOT NULL,
            bucket SMALLINT NOT NULL CHECK (bucket BETWEEN 0 AND 21),
            count BIGINT NOT NULL DEFAULT 0,
            PRIMARY KEY (collection_id, tag, bucket),
            FOREIGN KEY (collection_id, tag) REFERENCES collection_tags(collection_id, tag) ON DELETE CASCADE
        );

        UPDATE collections c
        SET object_count = (SELECT count(*) FROM objects o WHERE o.collection_id = c.id);

        INSERT INTO collection_tags (collection_id, tag, first_seen_at)
        SELECT collection_id, tag, min(updated_at)
        FROM object_tags
        GROUP BY collection_id, tag;

        INSERT INTO collection_tag_buckets (collection_id, tag, bucket, count)
        SELECT collection_id, tag, tagona_score_bucket(score), count(*)
        FROM object_tags
        GROUP BY collection_id, tag, tagona_score_bucket(score);
    END IF;
END
$migrate$;

-- Statement-level triggers with transition tables aggregate a statement's rows into one counter update per
-- (collection, tag, bucket). A tag's registry row is locked first, rows in sorted order, so concurrent writers of a
-- tag take its counters one after the other and cannot deadlock on them. Decrements only UPDATE existing rows, so
-- cascading deletes of a collection stay harmless.

CREATE OR REPLACE FUNCTION tagona_object_tags_inserted() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO collection_tags (collection_id, tag)
    SELECT DISTINCT collection_id, tag FROM new_rows
    ORDER BY collection_id, tag
    ON CONFLICT (collection_id, tag) DO NOTHING;

    PERFORM 1 FROM collection_tags ct
    WHERE (ct.collection_id, ct.tag) IN (SELECT collection_id, tag FROM new_rows)
    ORDER BY ct.collection_id, ct.tag
    FOR UPDATE;

    INSERT INTO collection_tag_buckets (collection_id, tag, bucket, count)
    SELECT collection_id, tag, tagona_score_bucket(score), count(*)
    FROM new_rows
    GROUP BY collection_id, tag, tagona_score_bucket(score)
    ORDER BY collection_id, tag, tagona_score_bucket(score)
    ON CONFLICT (collection_id, tag, bucket) DO UPDATE
    SET count = collection_tag_buckets.count + EXCLUDED.count;
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

    UPDATE collection_tag_buckets b
    SET count = b.count - d.n
    FROM (
        SELECT collection_id, tag, tagona_score_bucket(score) AS bucket, count(*) AS n
        FROM old_rows
        GROUP BY collection_id, tag, tagona_score_bucket(score)
    ) d
    WHERE b.collection_id = d.collection_id AND b.tag = d.tag AND b.bucket = d.bucket;

    INSERT INTO collection_tag_buckets (collection_id, tag, bucket, count)
    SELECT collection_id, tag, tagona_score_bucket(score), count(*)
    FROM new_rows
    GROUP BY collection_id, tag, tagona_score_bucket(score)
    ORDER BY collection_id, tag, tagona_score_bucket(score)
    ON CONFLICT (collection_id, tag, bucket) DO UPDATE
    SET count = collection_tag_buckets.count + EXCLUDED.count;
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

    UPDATE collection_tag_buckets b
    SET count = b.count - d.n
    FROM (
        SELECT collection_id, tag, tagona_score_bucket(score) AS bucket, count(*) AS n
        FROM old_rows
        GROUP BY collection_id, tag, tagona_score_bucket(score)
    ) d
    WHERE b.collection_id = d.collection_id AND b.tag = d.tag AND b.bucket = d.bucket;
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
