---
title: Native Backend Traces
description: Export completed native backend spans through an opt-in per-pod sidecar.
tags:
  - monitoring
  - tracing
  - opentelemetry
---

# Native Backend Traces

`spec.monitoring.nativeTraceExporter` adds a native trace exporter to each
PostgreSQL pod. Recording remains in the DocumentDB extension. The separate
exporter reads its restricted SQL API and sends completed spans to the pod's
Collector over OTLP. It does not run application queries or install a
PostgreSQL background worker.

This option requires a compatible native recorder, SQL schema, and exporter
image. It does not add native recording to an existing engine image. The image
must be supplied explicitly; no default or released exporter image is assumed.
The exporter supports the source's existing native Find coverage, not arbitrary
commands, complete execution plans, or measured per-query CPU.

## Prerequisites

- Monitoring must be enabled with an OTLP or [trace-file](trace-files.md)
  destination. Prometheus alone cannot receive native traces.
- The engine and schema must provide
  `documentdb_api_internal.backend_telemetry_capabilities()` and
  `documentdb_api_internal.backend_telemetry_read(uuid,bigint,integer)`.
- Enable the native producer's storage, context propagation, and recording
  settings explicitly. Storage allocation requires a PostgreSQL restart.
- Provision a restricted PostgreSQL reader and a namespaced connection Secret.
  The exporter option creates neither database roles nor grants.
- Use a gateway image that honors `OTEL_TRACES_ENABLED` and propagates sampled
  context into supported backend commands. The operator enables gateway tracing
  when native export is configured; it does not override an explicit gateway
  setting already present in the pod.

The connection is pod-local. The current exporter requires an explicit loopback
host, PostgreSQL database, user, and `sslmode=disable`. Do not use a Service
address or expose PostgreSQL for this feature. A separate exporter reads each
instance's own buffer, including replicas where the source supports recording.

## Configure the reader

Apply these grants in the PostgreSQL database used by the gateway, with an
administrator connection and a compatible installed schema:

```sql
CREATE ROLE native_trace_reader LOGIN;
GRANT CONNECT ON DATABASE postgres TO native_trace_reader;
GRANT USAGE ON SCHEMA documentdb_api_internal, documentdb_core
  TO native_trace_reader;
GRANT EXECUTE ON FUNCTION
  documentdb_api_internal.backend_telemetry_capabilities(),
  documentdb_api_internal.backend_telemetry_read(uuid, bigint, integer)
  TO native_trace_reader;
```

Review existing role memberships and public privileges rather than giving the
exporter administrative credentials. The native API isolates PostgreSQL
databases, not individual logical document databases or collections within one
PostgreSQL database.

Create a Secret in the DocumentDB resource's namespace with a `uri` key:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: native-trace-reader
type: Opaque
stringData:
  uri: "host=127.0.0.1 port=5432 dbname=postgres user=native_trace_reader sslmode=disable"
```

This example uses the operator's trusted pod-loopback connection path. If your
local authentication requires a password, keep it in this Secret, never in
image arguments, annotations, or logs.

## Enable export

Add the following settings to a DocumentDB resource with compatible images:

```yaml
spec:
  monitoring:
    enabled: true
    nativeTraceExporter:
      image: registry.example.com/documentdb/native-trace-exporter:compatible
      connectionSecretName: native-trace-reader
    exporter:
      traceFile: true
  postgres:
    parameters:
      documentdb.enable_backend_telemetry_storage: "on"
      documentdb.enable_udf_trace_context: "on"
      documentdb.enable_backend_telemetry: "on"
```

Replace the example image with your built, compatible artifact. The exporter
image uses `IfNotPresent`; use immutable tags or digests. An image pull failure
or a missing connection Secret can prevent pod startup.

The injected `native-trace-exporter` is a restartable init container. It starts
without waiting for PostgreSQL and has no startup or readiness probe. Missing
source capabilities, denied SQL reads, and unavailable OTLP delivery are
reported while the process retries; they do not gate database readiness.
The Collector retains its separate readiness probe.

Image, Secret-reference, or resource-setting changes trigger the operator's
normal pod replacement. Updating the contents of the same Secret does not
refresh an existing environment variable; restart the pod to pick it up.

## Resources and recovery

Defaults are a 32 MiB memory request, 128 MiB memory limit, 25m CPU request,
and 200m CPU limit. The memory limit and CPU request are reserved from the pod
envelope before calculating PostgreSQL's share. Override them with
`spec.resource.nativeTraceExporter.memory` and `.cpu`; explicit values set
request and limit equally.

The exporter holds at most one pending batch of 128 records / 256 KiB and
uses two-second SQL/export deadlines. It retries failed delivery with bounded
backoff, retaining the same batch and cursor. Recovery preserves acknowledged
progress when possible, or resumes at the oldest retained records with explicit
gap/source-change reporting.

Cursors and status counters are not durable. Exporter restarts can replay
retained spans; receivers must deduplicate original span identities.
PostgreSQL restarts replace the buffer and its source identity. Outages longer
than retention can lose data. Shutdown is best effort, particularly when the
Collector or pod is also stopping. No exactly-once or durable-delivery guarantee
is provided.

## Verify and diagnose

```bash
kubectl get pod "$POD" -n "$NS" \
  -o jsonpath='{range .spec.initContainers[*]}{.name}{"\n"}{end}'
kubectl logs "$POD" -n "$NS" -c native-trace-exporter --tail=20
```

JSON status has origin `native_exporter_status`. Inspect `state`, `error`,
`last_export_unix_ms`, `exported_records`, `unconfirmed_records`, `gap_events`,
`source_changes`, and the last resume/collection reports. These are process
diagnostics, not a SQL health API or additional metrics pipeline.
An empty successful poll does not prove that the workload emitted spans.

In the destination, native spans use service `documentdb-backend`, scope
`documentdb.backend`, and name `documentdb.command`. Match their trace and
parent IDs against the gateway spans, and verify source-instance identity.
For local capture, retrieve the Collector files as described in
[Trace Files](trace-files.md). Missing gateway spans, sampling, retention, or
delivery gaps must remain visible rather than being treated as complete traces.

Remove `spec.monitoring.nativeTraceExporter` to stop injecting the exporter
without disabling the Collector or its other sources. Native recording settings
and reader grants are separate and remain unchanged.
