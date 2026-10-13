---
title: Trace Files
description: Capture and retrieve bounded pod-local OTLP trace files without a telemetry backend or MCP server.
tags:
  - monitoring
  - tracing
  - opentelemetry
---

# Trace Files

Enable `spec.monitoring.exporter.traceFile` to write received OTLP traces to
rotating JSON-line files in each database pod. This reuses the standard
Collector image. It does not add an MCP server, HTTP trace-query API, or
separate history service.

This is experimental diagnostics, not durable trace storage. The
[upstream file exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/v0.149.0/exporter/fileexporter)
is alpha in the pinned Collector version, and its exact output fields are not
guaranteed stable across upgrades.

## Configure

Install the operator, sidecar-injector plugin, and CRDs built with this option.
An older plugin does not inject the writable volume. The Collector image itself
does not need to be rebuilt.

Merge this fragment into the DocumentDB resource:

```yaml
spec:
  monitoring:
    enabled: true
    exporter:
      prometheus:
        port: 8888
      traceFile: true
```

The Prometheus exporter is an HTTP metrics endpoint, not a Prometheus server.
It is optional: with only `traceFile: true`, the operator generates only a
traces pipeline and disables gateway metric export. No SQL receiver is started
in that mode. When a metrics destination is configured, the existing
`SELECT 1` health query remains unchanged; there is no new statistics or
pgsentinel polling.

An OTLP destination can also be configured. Traces then go to both that
destination and the file exporter. Metrics are never written to the trace file.

Changing the option causes the operator to roll the database pods so the
sidecar and mounts match the new configuration. Copy any needed trace files
before changing monitoring settings.

## Trace sources

The Collector receives OTLP/gRPC at `127.0.0.1:4317` within each pod. File mode
sets `OTEL_TRACES_ENABLED=true` on the gateway, in addition to its existing
OTLP endpoint. Use an image that supports that setting and emits traces.
Explicit gateway telemetry configuration still takes precedence. The Collector
does not add instrumentation to older images.

The PostgreSQL trace-export worker is separate work and is not installed by
this option. Native records that have no OTLP producer are not automatically
exported to these files.

## Storage and access

| Setting | Behavior |
|---------|----------|
| Current file | `/var/lib/otel/traces/traces.jsonl` |
| Encoding | Uncompressed OTLP JSON, one exported batch per line |
| Rotation | 10 MiB per file, up to three rotated backups plus the current file |
| Trace batching | Target 128 spans, at most 256 spans per exported batch, with a one-second timeout |
| Pod storage | Disk-backed `emptyDir` with a 64 MiB size limit, separate from database storage |
| Scheduling | Collector requests 64 MiB ephemeral storage; no new pod-wide storage limit is imposed on database or gateway logs |
| Access | Collector mount is writable; the existing `postgres` container has a read-only mount |
| Lifetime | Lost on pod deletion or replacement; not replicated or backed up |

Rotation bounds retained file data, not a time window. Cleanup can briefly
leave an extra rotated file. Kubernetes storage limits are eviction safeguards,
not filesystem quotas; node storage pressure can still evict the pod. A
Collector restart in the same pod can preserve files, but unexported batches
can be lost. Oversized batches, full storage, and write errors can drop traces;
check the Collector logs.

The mount relies on CNPG's shared pod identity so PostgreSQL can read files
written by the Collector. It does not grant the Collector access to the
database's data volume. The Collector keeps a read-only root filesystem in
file mode.

File mode sets the Collector's Go memory target to 50% of its configured memory
limit, below the receiver's memory-limiter threshold. It does not increase the
container memory limit or disable memory protection. This is a soft GC target,
not a guarantee against backpressure or lost traces under load.

Trace payloads can contain sensitive attributes. The file exporter does not
redact them. Limit Kubernetes access, copy only what is needed, and protect or
delete local copies after use.

## Retrieve files

An existing agent can use its Kubernetes or terminal tool. No new agent
protocol is required. Choose the pod explicitly; each pod has independent
files, so do not assume the current primary has all traces.

```bash
NS=my-namespace
CLUSTER=my-cluster

kubectl get pods -n "$NS" -l "cnpg.io/cluster=$CLUSTER"
POD=my-cluster-1
```

List files or read the current file through the PostgreSQL container:

```bash
kubectl exec -n "$NS" "$POD" -c postgres -- ls -lh /var/lib/otel/traces
kubectl exec -n "$NS" "$POD" -c postgres -- \
  cat /var/lib/otel/traces/traces.jsonl
```

Copy the directory, including rotated files, before a rollout:

```bash
umask 077
kubectl cp -c postgres \
  "$NS/$POD:/var/lib/otel/traces" "./${POD}-traces"
```

Use `postgres`, not `otel-collector`: the standard Collector image has no
shell, `cat`, or `tar`. These commands require the usual tools in the
PostgreSQL image. Copying while files are being written or rotated is
best-effort, not an atomic snapshot; an in-flight final line may be incomplete.
The file might not exist until the first batch of spans arrives.

`pods/exec` is powerful administrative access, not a read-only diagnostics
permission. The read-only mount protects these files from writes through that
mount; it does not restrict what other commands an agent could execute.
Use a Kubernetes tool with appropriately scoped permissions, or have an
administrator copy the files for an agent with narrower access.

For live metrics, independently forward the configured metrics port:

```bash
kubectl port-forward -n "$NS" "pod/$POD" 18888:8888
```

In another terminal:

```bash
curl --fail --silent --show-error http://127.0.0.1:18888/metrics
```

The endpoint returns current metrics, not traces or historical SQL rows.
Use the [pgsentinel SQL readers](pgsentinel.md) directly for sampled database
activity.

## Disable

Set `spec.monitoring.exporter.traceFile: false` to remove file output while
keeping another configured exporter. If files were the only destination,
disable `spec.monitoring.enabled` as well. The resulting pod replacement
removes the files. Download needed evidence first.
