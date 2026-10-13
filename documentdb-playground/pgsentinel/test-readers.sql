-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

\set ON_ERROR_STOP on

-- Run in documentdb_diagnostics on a disposable test cluster after generating
-- activity in postgres and pgsentinel_other_test from separate connections.
BEGIN;
SET LOCAL statement_timeout = '5s';

CREATE TEMP TABLE excluded_backends AS
SELECT DISTINCT pid FROM public.pg_active_session_history()
WHERE datname = 'pgsentinel_other_test';
GRANT SELECT ON excluded_backends TO documentdb_diagnostics_reader;
-- Normalized query IDs can be shared across databases, so only foreign-only
-- query IDs prove that the reader exposed another database.
CREATE TEMP TABLE excluded_queries AS
SELECT DISTINCT queryid FROM public.pg_stat_statements_history()
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = 'pgsentinel_other_test')
EXCEPT
SELECT queryid FROM public.pg_stat_statements_history()
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = 'postgres');
GRANT SELECT ON excluded_queries TO documentdb_diagnostics_reader;
DO $test$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM excluded_backends)
       OR NOT EXISTS (SELECT 1 FROM excluded_queries) THEN
        RAISE EXCEPTION 'The cross-database activity and statistics fixtures must be sampled';
    END IF;
END;
$test$;

CREATE ROLE pgsentinel_untrusted_test NOLOGIN;
SET LOCAL SESSION AUTHORIZATION pgsentinel_untrusted_test;
DO $test$
BEGIN
    BEGIN
        PERFORM public.get_parsedinfo(0);
        RAISE EXCEPTION 'Untrusted role could read parsed query text';
    EXCEPTION WHEN insufficient_privilege THEN
        NULL;
    END;
    BEGIN
        PERFORM public.pg_active_session_history();
        RAISE EXCEPTION 'Untrusted role could read raw activity';
    EXCEPTION WHEN insufficient_privilege THEN
        NULL;
    END;
    BEGIN
        PERFORM documentdb_diagnostics.active_session_history();
        RAISE EXCEPTION 'Untrusted role could use the diagnostic reader';
    EXCEPTION WHEN insufficient_privilege THEN
        NULL;
    END;
END;
$test$;
RESET SESSION AUTHORIZATION;

SET LOCAL SESSION AUTHORIZATION documentdb_diagnostics_reader;
CREATE TEMP TABLE pg_database (oid oid, datname name);
SET LOCAL search_path = pg_temp, public;
DO $test$
DECLARE
    row_count integer;
BEGIN
    SELECT count(*) INTO row_count
    FROM documentdb_diagnostics.active_session_history(statement_timestamp() - interval '5 minutes', 2);
    IF row_count < 1 OR row_count > 2 THEN
        RAISE EXCEPTION 'Activity reader is empty or exceeded the requested bound: %', row_count;
    END IF;
    IF EXISTS (
        SELECT 1 FROM documentdb_diagnostics.active_session_history() AS h
        JOIN excluded_backends AS x ON x.pid = h.backend_pid
    ) THEN
        RAISE EXCEPTION 'Activity reader exposed another database';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM documentdb_diagnostics.active_session_history() AS h
        WHERE h.query_id IS NOT NULL AND h.wait_event = 'PgSleep'
    ) THEN
        RAISE EXCEPTION 'Expected sampled wait and query identity are missing';
    END IF;
    IF EXISTS (
        SELECT 1 FROM documentdb_diagnostics.active_session_history() AS h
        WHERE to_jsonb(h) ?| ARRAY['query', 'top_level_query', 'application_name', 'client_addr', 'usename']
           OR to_jsonb(h)::text LIKE '%DO_NOT_EXPORT_SENTINEL%'
    ) THEN
        RAISE EXCEPTION 'Activity reader exposed unapproved text';
    END IF;
    SELECT count(*) INTO row_count
    FROM documentdb_diagnostics.statement_history(statement_timestamp() - interval '5 minutes', 2);
    IF row_count < 1 OR row_count > 2 THEN
        RAISE EXCEPTION 'Statement history is empty or exceeded the requested bound: %', row_count;
    END IF;
    IF EXISTS (
        SELECT 1 FROM documentdb_diagnostics.statement_history() AS h
        JOIN excluded_queries AS x ON x.queryid = h.query_id
    ) THEN
        RAISE EXCEPTION 'Statement history exposed another database';
    END IF;
    BEGIN
        PERFORM documentdb_diagnostics.active_session_history(statement_timestamp(), 257);
        RAISE EXCEPTION 'Oversized activity read was accepted';
    EXCEPTION WHEN invalid_parameter_value THEN
        NULL;
    END;
    BEGIN
        PERFORM documentdb_diagnostics.statement_history(statement_timestamp(), NULL);
        RAISE EXCEPTION 'Unbounded statement history read was accepted';
    EXCEPTION WHEN invalid_parameter_value THEN
        NULL;
    END;
    BEGIN
        PERFORM documentdb_diagnostics.active_session_history('-infinity', 1);
        RAISE EXCEPTION 'Unbounded lookback was accepted';
    EXCEPTION WHEN invalid_parameter_value THEN
        NULL;
    END;
    BEGIN
        PERFORM public.get_parsedinfo(0);
        RAISE EXCEPTION 'Reader could bypass the approved projection';
    EXCEPTION WHEN insufficient_privilege THEN
        NULL;
    END;
    BEGIN
        PERFORM public.pg_stat_statements_history();
        RAISE EXCEPTION 'Reader could bypass database scoping';
    EXCEPTION WHEN insufficient_privilege THEN
        NULL;
    END;
    BEGIN
        EXECUTE 'SET ROLE documentdb_diagnostics_owner';
        RAISE EXCEPTION 'Reader could assume the privileged owner role';
    EXCEPTION WHEN insufficient_privilege THEN
        NULL;
    END;
END;
$test$;
ROLLBACK;
