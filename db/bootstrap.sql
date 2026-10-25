-- Cluster roles for aether. Run once per Postgres cluster, as a superuser,
-- before any migration: roles are cluster-wide, so they cannot live in
-- per-database migrations (parallel test databases would race on them).
--
--   psql "$SUPERUSER_URL" -v app_password=... -v worker_password=... -f db/bootstrap.sql
--
-- The migrator (owner of the schema) is whichever role runs `aether migrate`.

SELECT 'CREATE ROLE aether_app LOGIN NOSUPERUSER NOBYPASSRLS'
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'aether_app') \gexec
SELECT 'CREATE ROLE aether_worker LOGIN NOSUPERUSER BYPASSRLS'
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'aether_worker') \gexec

ALTER ROLE aether_app PASSWORD :'app_password';
ALTER ROLE aether_worker PASSWORD :'worker_password';

-- The worker finds due work across sites (BYPASSRLS), then switches to
-- aether_app for the site it is working on, so RLS still guards the work.
GRANT aether_app TO aether_worker;
