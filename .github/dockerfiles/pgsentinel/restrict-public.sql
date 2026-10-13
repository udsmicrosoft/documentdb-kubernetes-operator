
-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- Restrict raw cross-session metadata before extension installation commits.
REVOKE ALL ON FUNCTION pg_active_session_history(),
    pg_stat_statements_history(), get_parsedinfo(integer) FROM PUBLIC;
REVOKE ALL ON pg_active_session_history, pg_stat_statements_history FROM PUBLIC;
