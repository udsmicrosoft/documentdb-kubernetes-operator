# Sampled PostgreSQL Activity with pgsentinel

pgsentinel records recent PostgreSQL session activity in bounded, instance-local
shared-memory buffers. It can also sample `pg_stat_statements` for active query
IDs. It does not require a Prometheus server or a separate pgsentinel sidecar.

This opt-in example adds extension packaging and restricted SQL readers. Its
Collector exposes current metrics over HTTP and writes received traces to
[bounded pod-local files](trace-files.md). The SQL readers are called directly,
not polled by the Collector. No MCP server, Prometheus server, or separate
telemetry history service is required.

## Build the Extension Image

From the repository root:

```bash
docker build --platform linux/amd64 \
  -f .github/dockerfiles/Dockerfile_pgsentinel \
  -t pgsentinel:1.5.1-pg18-trixie .github/dockerfiles
```

The Dockerfile pins pgsentinel 1.5.1 to upstream commit
`52b750b8775e787d7ef1f6214e52dfaf53ad3535`. It builds with PostgreSQL 18 development
headers on Debian Trixie and copies only the extension library, control file,
SQL migrations, and applicable licenses into a scratch image. It does not bundle
another PostgreSQL server or the standalone trace exporter.

The only change to upstream extension SQL is a permission restriction appended
to every installation and upgrade script. Raw activity, statement history, and
parsed-query functions are not executable by `PUBLIC`, so creating the
extension does not expose cross-session metadata before reader setup. The C
implementation is unchanged.

Use an image that matches the PostgreSQL major version, distribution, and
architecture of your database pods. Only the amd64 build is covered by this
example. Publish the image to your registry or load it into your local cluster
before deploying. Prefer an immutable digest reference. Building locally does
not publish the image.

## Configure the DocumentDB Resource

Install the operator, sidecar-injector plugin, and CRDs built from the checkout
containing `spec.postgres.extensions`, `spec.postgres.sharedPreloadLibraries`,
and `spec.monitoring.exporter.traceFile` before using this example. The extension
image alone does not add these API fields to an older operator installation.

The example is in `documentdb-playground/pgsentinel/documentdb.yaml`. Replace
`registry.example.com/pgsentinel:1.5.1-pg18-trixie` with your accessible image
reference and configure `spec.imagePullSecrets` if needed.

For gateway spans, select a trace-capable image through `spec.image.gateway`.
The Collector cannot create spans that an image does not emit. File mode sets
`OTEL_TRACES_ENABLED=true` on the gateway, but does not install the future
PostgreSQL trace-export worker or enable native recording. See
[Trace Files](trace-files.md) for file retrieval and storage limits.

`spec.postgres.extensions` accepts additional CNPG ImageVolume configurations.
The `documentdb` name is reserved for the required image configured through
`spec.image.documentDB`. `spec.postgres.sharedPreloadLibraries` adds library
names without removing required DocumentDB preloads.

Setting a non-empty list in either field opts into management of **both** CNPG
extension images and preload lists. First copy any manually configured additions from the CNPG
Cluster into the DocumentDB resource. Once opted in, edit the DocumentDB
resource, not those underlying CNPG fields. Removing both fields restores the
required images and preloads. Clusters that never opt in retain the prior
image-only reconciliation behavior.

CNPG mounts the extension artifacts; it does not compile them inside database
pods. Image changes and preloads require a rolling restart. The operator also
requests a restart for extension path or environment changes that do not
change an image.

The example uses one-second sampling, a 1,000-entry activity buffer, and a
10,000-entry statement-statistics buffer. These are the minimum sizes accepted
by the pinned source, even though the upstream README lists a smaller
statement-history minimum. Entry counts are **not** retention times: concurrency
and activity determine how quickly records are overwritten. Increasing these
values increases shared-memory and reader costs. No overhead guarantee has
been established.

`pg_stat_statements.track: all` lets CNPG manage pg_stat_statements. The
pgsentinel worker connects to a separate `documentdb_diagnostics` PostgreSQL
database on the same instance. It samples session activity across databases;
the restricted readers return only the application database named `postgres`.

The example includes a CNPG `Database` resource that creates the diagnostics
database and installs pgsentinel 1.5.1. Both resources must be in the same
namespace. Do not create a CNPG `Database` resource with `spec.name: postgres`,
because CNPG reserves that database name.

Do not install the DocumentDB engine in the diagnostics database. Its current
planner resolves application functions while planning ordinary SQL function
calls, requiring application-schema privileges. Separating the diagnostics
database avoids granting those privileges to diagnostic readers or changing
the engine.

## Install and Restrict the SQL Surface

For a **new cluster**, apply both example resources and wait for CNPG to create
the diagnostics database and its extension. The worker can report connection
errors until the database exists; it retries without blocking PostgreSQL.

For an **existing cluster**, create the diagnostics `Database` resource first,
then configure the extension image, preloads, and parameters through DocumentDB.
Wait for the rollout and extension installation. Do not use post-init SQL for
existing clusters: it does not replay.

For either path, execute `documentdb-playground/pgsentinel/setup.sql` against
the primary's `documentdb_diagnostics` database as an administrator before
granting a diagnostics login access:

```bash
psql -X -v ON_ERROR_STOP=1 --dbname="$ADMIN_DATABASE_URL" \
  -f documentdb-playground/pgsentinel/setup.sql
```

Here `ADMIN_DATABASE_URL` must select `documentdb_diagnostics`, not `postgres`.
The script is transactional and idempotent for this version. It restricts
database connection privileges and public access to raw pgsentinel functions.
It rejects unexpected PostgreSQL versions, extension versions, database
selection, and ownership conflicts. To upgrade an older pgsentinel installation, first make
an explicit administrative upgrade to 1.5.1, then reapply the setup script.
Reapply access restrictions after future extension upgrades.

It creates two non-login roles:

| Role | Purpose |
|------|---------|
| `documentdb_diagnostics_owner` | Owns fixed security-definer functions and has `pg_read_all_stats` for resolving query IDs |
| `documentdb_diagnostics_reader` | Can execute only the approved diagnostic functions; receives no broad monitoring membership |

Grant the reader role to a dedicated diagnostics login managed by your
deployment. Do not grant the owner role or `pg_read_all_stats` to the Collector
or agent. The existing `otel_monitor` role and its `SELECT 1` health query are
unchanged by this example.

The two approved calls are:

```sql
SELECT * FROM documentdb_diagnostics.active_session_history(
    statement_timestamp() - interval '5 minutes', 128);
SELECT * FROM documentdb_diagnostics.statement_history(
    statement_timestamp() - interval '5 minutes', 128);
```

Readers require a time within the previous hour and a row limit from 1 through
256. They return only the `postgres` application database's records, without query
text, literals, application names, usernames, or client addresses. The
statement-history call fails explicitly if `pgsentinel_pgssh.enable` is off.
Apply a short statement timeout and controlled polling cadence in the calling
service. The output limit does not eliminate the cost of scanning the bounded
upstream buffer.

An agent can call these functions through its existing PostgreSQL tool or
`psql`, connected to `documentdb_diagnostics` using the dedicated diagnostics
login. Keep credentials in the tool's protected configuration, not its prompt.
These wrappers bound and filter their own results; role membership is not a
general SQL firewall. Database scoping is not logical document-namespace scoping.

## Check the Image and Readers

Run this only in a disposable test instance. Set `APP_DATABASE_URL` to the
administrator connection for `postgres`, `OTHER_DATABASE_URL` to the same
instance's `pgsentinel_other_test` database, and `DIAGNOSTICS_DATABASE_URL` to
its `documentdb_diagnostics` database. Run `setup.sql` first.

First verify that the image restricts raw access on installation and upgrade,
without relying on `setup.sql`. The test changes are rolled back:

```bash
psql -X -v ON_ERROR_STOP=1 "$APP_DATABASE_URL" \
  -c 'CREATE DATABASE pgsentinel_image_test TEMPLATE template0' \
  -c '\connect pgsentinel_image_test' \
  -f documentdb-playground/pgsentinel/test-image.sql
```

Then generate activity and verify the restricted readers:

```bash
psql -X -v ON_ERROR_STOP=1 "$APP_DATABASE_URL" \
  -c 'CREATE DATABASE pgsentinel_other_test'
psql -X -v ON_ERROR_STOP=1 "$OTHER_DATABASE_URL" \
  -c 'SELECT pg_sleep(0.01), 987654321 AS foreign_database_fixture' \
  -c 'SELECT pg_sleep(3.0), 987654321 AS foreign_database_fixture'
psql -X -v ON_ERROR_STOP=1 "$APP_DATABASE_URL" \
  -c "SELECT pg_sleep(0.01), 'DO_NOT_EXPORT_SENTINEL'::text" \
  -c "SELECT pg_sleep(3.0), 'DO_NOT_EXPORT_SENTINEL'::text"
psql -X -v ON_ERROR_STOP=1 "$DIAGNOSTICS_DATABASE_URL" \
  -f documentdb-playground/pgsentinel/test-readers.sql
```

The short call seeds statement statistics using the same parameter types as
the sampled call. The checks cover samples and query IDs, database isolation,
output bounds, text exclusion, invalid parameters, and restricted execution
under non-administrative session identities. The SQL test rolls back its own
temporary objects and test role; discard the test instance afterward.

## Interpretation and Removal

ASH is sampled activity and wait evidence. It can miss short operations and is
not a complete slow-query log. A long-running sample shows query age at sampling
time, not the completed operation's duration. An active session without a wait
event is not an exact measurement of query CPU.

Statement history contains cumulative PostgreSQL statistics snapshots, not
per-request measurements. PostgreSQL wrapper query IDs can combine different
document query shapes. Shared memory is lost on restart; collect each instance
separately and treat failover as a change of evidence source.

To remove the example, stop its readers, drop the `documentdb_diagnostics`
schema and the pgsentinel SQL extension through an approved database migration,
updating the CNPG `Database` resource to stop managing that extension first.
Remove its parameters, then remove its image and preload
entry from the DocumentDB resource. Do not remove an image while still
preloading its library. Do not drop pg_stat_statements if other monitoring
uses it.

Sources: [pgsentinel](https://github.com/pgsentinel/pgsentinel),
[CNPG ImageVolume extensions](https://cloudnative-pg.io/docs/1.29/imagevolume_extensions/),
[CNPG PostgreSQL configuration](https://cloudnative-pg.io/docs/1.29/postgresql_conf/).
