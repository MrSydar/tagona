-- Gives each service that touches the database its own role with access to nothing else. Run by the
-- db-init compose service as the bootstrap superuser, before the services start, and on every
-- `up`, so it is idempotent. It also upgrades a database created before keystorage existed.
--
--   psql -v storage_password=... -v keys_password=... -f roles.sql
--
--   tagona_storage  owns everything in the public schema (collections, objects, tags, ...). The
--                   storage service migrates its own tables on start, so it must own them. It has
--                   no privilege on the keys schema.
--   tagona_keys     owns the keys schema, where the api_keys table lives, and nothing else. The
--                   keystorage service is the only way to create, list, delete or check keys.
--
-- The compose database is a demo: the bootstrap superuser "tagona" and its published port exist
-- for convenience. In a real deployment, do not use the superuser for anything but this script.

SELECT format('CREATE ROLE tagona_storage LOGIN PASSWORD %L', :'storage_password')
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tagona_storage') \gexec
SELECT format('ALTER ROLE tagona_storage PASSWORD %L', :'storage_password') \gexec

SELECT format('CREATE ROLE tagona_keys LOGIN PASSWORD %L', :'keys_password')
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tagona_keys') \gexec
SELECT format('ALTER ROLE tagona_keys PASSWORD %L', :'keys_password') \gexec

-- uuid_generate_v4() is used by the storage tables; creating an extension needs more than the
-- storage role should have.
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- The keys schema, owned by the keystorage role.
CREATE SCHEMA IF NOT EXISTS keys AUTHORIZATION tagona_keys;
ALTER SCHEMA keys OWNER TO tagona_keys;

-- Databases created before keystorage have api_keys in the public schema: move it, keys included.
DO $$
BEGIN
    IF to_regclass('public.api_keys') IS NOT NULL AND to_regclass('keys.api_keys') IS NULL THEN
        ALTER TABLE public.api_keys SET SCHEMA keys;
    END IF;
    IF to_regclass('keys.api_keys') IS NOT NULL THEN
        ALTER TABLE keys.api_keys OWNER TO tagona_keys;
    END IF;
END
$$;

-- Nobody but the owner gets in.
REVOKE ALL ON SCHEMA keys FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA keys FROM PUBLIC;

-- The public schema belongs to the storage role: the tables and functions that exist already (from
-- a database created while the superuser ran the storage service) move to it, so its migrations
-- can keep altering them. Objects that belong to an extension stay with the extension.
GRANT USAGE, CREATE ON SCHEMA public TO tagona_storage;
DO $$
DECLARE
    r record;
BEGIN
    FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' LOOP
        EXECUTE format('ALTER TABLE public.%I OWNER TO tagona_storage', r.tablename);
    END LOOP;
    FOR r IN
        SELECT p.oid::regprocedure AS signature
          FROM pg_proc p
          JOIN pg_namespace n ON n.oid = p.pronamespace
         WHERE n.nspname = 'public'
           AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
    LOOP
        EXECUTE format('ALTER FUNCTION %s OWNER TO tagona_storage', r.signature);
    END LOOP;
END
$$;
