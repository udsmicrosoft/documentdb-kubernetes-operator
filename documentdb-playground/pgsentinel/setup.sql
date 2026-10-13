-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

\set ON_ERROR_STOP on

BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

DO $setup$
BEGIN
    IF current_database() <> 'documentdb_diagnostics' OR pg_is_in_recovery() THEN
        RAISE EXCEPTION 'Run this setup in the documentdb_diagnostics database on the primary';
    END IF;
    IF current_setting('server_version_num')::integer / 10000 <> 18 THEN
        RAISE EXCEPTION 'This setup requires PostgreSQL 18';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname IN ('documentdb', 'documentdb_core')) THEN
        RAISE EXCEPTION 'Do not install application extensions in the diagnostics database';
    END IF;
END;
$setup$;

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
CREATE EXTENSION IF NOT EXISTS pgsentinel WITH SCHEMA public VERSION '1.5.1';

DO $setup$
DECLARE
    role_name text;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_extension
        WHERE extname = 'pgsentinel' AND extversion = '1.5.1'
          AND extnamespace = 'public'::regnamespace
    ) THEN
        RAISE EXCEPTION 'Install pgsentinel 1.5.1 in public before running this setup';
    END IF;

    FOREACH role_name IN ARRAY ARRAY['documentdb_diagnostics_owner', 'documentdb_diagnostics_reader']
    LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = role_name) THEN
            EXECUTE format('CREATE ROLE %I NOLOGIN', role_name);
        END IF;
        IF EXISTS (
            SELECT 1 FROM pg_roles
            WHERE rolname = role_name
              AND (rolcanlogin OR rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)
        ) THEN
            RAISE EXCEPTION 'Role % must be an unprivileged NOLOGIN role', role_name;
        END IF;
    END LOOP;
END;
$setup$;

-- The non-login owner can resolve cross-session query IDs. Readers receive
-- only the fixed application-database projections below, not this membership.
GRANT pg_read_all_stats TO documentdb_diagnostics_owner;
GRANT USAGE ON SCHEMA public TO documentdb_diagnostics_owner;
REVOKE ALL ON FUNCTION public.pg_active_session_history(),
    public.pg_stat_statements_history(), public.get_parsedinfo(integer) FROM PUBLIC;
REVOKE ALL ON public.pg_active_session_history, public.pg_stat_statements_history FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.pg_active_session_history(),
    public.pg_stat_statements_history() TO documentdb_diagnostics_owner;

CREATE SCHEMA IF NOT EXISTS documentdb_diagnostics AUTHORIZATION documentdb_diagnostics_owner;
DO $setup$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_namespace
        WHERE nspname = 'documentdb_diagnostics'
          AND nspowner = 'documentdb_diagnostics_owner'::regrole
    ) THEN
        RAISE EXCEPTION 'The documentdb_diagnostics schema must be owned by documentdb_diagnostics_owner';
    END IF;
END;
$setup$;
REVOKE ALL ON SCHEMA documentdb_diagnostics FROM PUBLIC;
GRANT USAGE ON SCHEMA documentdb_diagnostics TO documentdb_diagnostics_reader;
REVOKE CONNECT ON DATABASE documentdb_diagnostics FROM PUBLIC;
GRANT CONNECT ON DATABASE documentdb_diagnostics TO documentdb_diagnostics_reader;

CREATE OR REPLACE FUNCTION documentdb_diagnostics.active_session_history(
    since timestamptz DEFAULT statement_timestamp() - interval '5 minutes',
    row_limit integer DEFAULT 128
)
RETURNS TABLE (
    sample_time timestamptz,
    backend_pid integer,
    leader_pid integer,
    backend_start timestamptz,
    query_start timestamptz,
    query_id bigint,
    nested_query_id bigint,
    state text,
    wait_event_type text,
    wait_event text,
    backend_type text,
    blockers integer,
    blocker_pid integer
)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $function$
BEGIN
    IF row_limit IS NULL OR row_limit < 1 OR row_limit > 256 THEN
        RAISE EXCEPTION 'row_limit must be between 1 and 256' USING ERRCODE = '22023';
    END IF;
    IF since IS NULL OR NOT isfinite(since)
       OR since < statement_timestamp() - interval '1 hour'
       OR since > statement_timestamp() THEN
        RAISE EXCEPTION 'since must be within the previous hour' USING ERRCODE = '22023';
    END IF;

    RETURN QUERY
    SELECT h.ash_time, h.pid, h.leader_pid, h.backend_start, h.query_start,
           h.queryid, h.nested_queryid, h.state, h.wait_event_type, h.wait_event,
           h.backend_type, h.blockers, h.blockerpid
    FROM public.pg_active_session_history() AS h
    WHERE h.datid = (SELECT oid FROM pg_database WHERE datname = 'postgres')
      AND h.ash_time >= since
      AND h.pid <> pg_backend_pid()
    ORDER BY h.ash_time DESC, h.pid
    LIMIT row_limit;
END;
$function$;

CREATE OR REPLACE FUNCTION documentdb_diagnostics.statement_history(
    since timestamptz DEFAULT statement_timestamp() - interval '5 minutes',
    row_limit integer DEFAULT 128
)
RETURNS TABLE (
    sample_time timestamptz,
    role_id oid,
    query_id bigint,
    calls bigint,
    total_exec_time_ms double precision,
    rows bigint,
    shared_blocks_hit bigint,
    shared_blocks_read bigint,
    shared_blocks_written bigint,
    temp_blocks_read bigint,
    temp_blocks_written bigint,
    block_read_time_ms double precision,
    block_write_time_ms double precision
)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $function$
BEGIN
    IF row_limit IS NULL OR row_limit < 1 OR row_limit > 256 THEN
        RAISE EXCEPTION 'row_limit must be between 1 and 256' USING ERRCODE = '22023';
    END IF;
    IF since IS NULL OR NOT isfinite(since)
       OR since < statement_timestamp() - interval '1 hour'
       OR since > statement_timestamp() THEN
        RAISE EXCEPTION 'since must be within the previous hour' USING ERRCODE = '22023';
    END IF;

    RETURN QUERY
    SELECT h.ash_time, h.userid, h.queryid, h.calls, h.total_exec_time, h.rows,
           h.shared_blks_hit, h.shared_blks_read, h.shared_blks_written,
           h.temp_blks_read, h.temp_blks_written, h.blk_read_time, h.blk_write_time
    FROM public.pg_stat_statements_history() AS h
    WHERE h.dbid = (SELECT oid FROM pg_database WHERE datname = 'postgres')
      AND h.ash_time >= since
    ORDER BY h.ash_time DESC, h.queryid, h.userid
    LIMIT row_limit;
END;
$function$;

ALTER FUNCTION documentdb_diagnostics.active_session_history(timestamptz, integer)
    OWNER TO documentdb_diagnostics_owner;
ALTER FUNCTION documentdb_diagnostics.statement_history(timestamptz, integer)
    OWNER TO documentdb_diagnostics_owner;
REVOKE ALL ON FUNCTION documentdb_diagnostics.active_session_history(timestamptz, integer),
    documentdb_diagnostics.statement_history(timestamptz, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION documentdb_diagnostics.active_session_history(timestamptz, integer),
    documentdb_diagnostics.statement_history(timestamptz, integer) TO documentdb_diagnostics_reader;

COMMIT;
