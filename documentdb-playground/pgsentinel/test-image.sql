-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

\set ON_ERROR_STOP on

BEGIN;
SET LOCAL statement_timeout = '5s';

DO $test$
BEGIN
    IF current_database() <> 'pgsentinel_image_test' THEN
        RAISE EXCEPTION 'Run the image test in a fresh pgsentinel_image_test database';
    END IF;
END;
$test$;

CREATE ROLE pgsentinel_image_test NOLOGIN;
DO $test$
DECLARE
    command text;
BEGIN
    FOREACH command IN ARRAY ARRAY[
        'CREATE EXTENSION pgsentinel WITH SCHEMA public VERSION ''1.0''',
        'ALTER EXTENSION pgsentinel UPDATE TO ''1.5.1'''
    ]
    LOOP
        EXECUTE command;
        IF EXISTS (
            SELECT 1
            FROM unnest(ARRAY['public.pg_active_session_history()',
                              'public.pg_stat_statements_history()',
                              'public.get_parsedinfo(integer)']) AS f(name)
            WHERE has_function_privilege('pgsentinel_image_test', f.name, 'EXECUTE')
        ) OR EXISTS (
            SELECT 1
            FROM unnest(ARRAY['public.pg_active_session_history',
                              'public.pg_stat_statements_history']) AS v(name)
            WHERE has_table_privilege('pgsentinel_image_test', v.name, 'SELECT')
        ) THEN
            RAISE EXCEPTION 'Raw evidence became public after %', command;
        END IF;
    END LOOP;
END;
$test$;
ROLLBACK;
