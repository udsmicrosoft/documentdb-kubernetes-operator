// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package demo

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/documentdb/documentdb-kubernetes-operator/documentdb-playground/performance-advisor/internal/capture"
	"github.com/documentdb/documentdb-kubernetes-operator/documentdb-playground/performance-advisor/internal/testutil"
)

func startObserver(t *testing.T, dir string) *mcp.ClientSession {
	t.Helper()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, dir, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "demo-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	return session
}

func callObserver(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || result.IsError || len(result.Content) != 2 {
		t.Fatalf("observer tool %s: %v %+v", name, err, result)
	}
	data, err := json.Marshal(result)
	if err != nil || len(data) > maxObserverResponseBytes {
		t.Fatalf("observer result exceeds the client-facing budget: %d bytes, %v", len(data), err)
	}
	return result
}

func gatewayResource() []*commonpb.KeyValue {
	return []*commonpb.KeyValue{
		testutil.Attribute("k8s.namespace.name", Namespace),
		testutil.Attribute("documentdb.cluster", Cluster),
		testutil.Attribute("service.name", "documentdb_gateway"),
		testutil.Attribute("service.instance.id", "demo-test-instance"),
	}
}

func TestAdapterUsesRealMCPReadsAndNeutralMetadata(t *testing.T) {
	dir, run, store := preparedRun(t)
	gate := coordinator(t, dir, run.CaptureID)
	session := startObserver(t, dir)
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 4 {
		t.Fatalf("expected exactly four tools: %v %+v", err, tools)
	}
	for _, tool := range tools.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("adapter lost read-only annotation: %+v", tool)
		}
	}
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "get_recent_traces", Arguments: map[string]any{"limit": 3},
	})
	if err == nil && !result.IsError {
		t.Fatal("observation before initial status was permitted")
	}
	callObserver(t, session, "get_capture_status", nil)
	if err := gate.WaitReady(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := gate.BeginTrial("trial-1", now.Add(-50*time.Second)); err != nil {
		t.Fatal(err)
	}
	metrics := testutil.Metrics("db.client.operations", metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
		testutil.Number(now.Add(-30*time.Second), now.Add(-10*time.Second), 7))
	metrics.ResourceMetrics[0].Resource.Attributes = gatewayResource()
	if outcome, err := store.AddMetrics(metrics); err != nil || outcome.Accepted != 1 {
		t.Fatalf("seed metrics: %v %+v", err, outcome)
	}
	span := testutil.Span(now.Add(-5*time.Second), 1, 1, 0)
	span.Name = "gateway.request"
	execute := testutil.Span(now.Add(-5*time.Second), 1, 2, 1)
	execute.Name = "postgres.execute"
	execute.StartTimeUnixNano += uint64(time.Millisecond)
	execute.EndTimeUnixNano -= uint64(time.Millisecond)
	traces := testutil.Traces(span, execute)
	traces.ResourceSpans[0].Resource.Attributes = gatewayResource()
	if outcome, err := store.AddTraces(traces); err != nil || outcome.Accepted != 2 {
		t.Fatalf("seed trace: %v %+v", err, outcome)
	}
	callObserver(t, session, "get_metric_window", map[string]any{"metric": "db.client.operations", "limit": 10})
	callObserver(t, session, "get_recent_traces", map[string]any{"limit": 3})
	proof, err := readProof(dir, run)
	if err != nil || len(proof.Observations) != 1 {
		t.Fatalf("missing proof: %v %+v", err, proof)
	}
	if len(proof.Observations[0].Metrics) != 1 || len(proof.Observations[0].Traces) != 0 {
		t.Fatalf("trace listing must not substitute for single-trace detail: %+v", proof)
	}
	detail := callObserver(t, session, "get_trace", map[string]any{"trace_id": hex.EncodeToString(span.TraceId)})
	proof, err = readProof(dir, run)
	if err != nil || len(proof.Observations[0].Traces) != 1 || *proof.Observations[0].Metrics[0].Value.Int != 7 {
		t.Fatalf("detailed read was not recorded: %v %+v", err, proof)
	}
	var captureReply observerView
	if err := json.Unmarshal([]byte(detail.Content[0].(*mcp.TextContent).Text), &captureReply); err != nil {
		t.Fatal(err)
	}
	if captureReply.Evidence == nil || len(captureReply.Evidence.Traces) != 1 {
		t.Fatal("adapter did not expose the detailed timeline")
	}
	if err := SaveReport(dir, filepath.Join(dir, "report.json")); err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := readJSON(filepath.Join(dir, "report.json"), &report); err != nil {
		t.Fatal(err)
	}
	if report.Completed {
		t.Fatal("an unfinished run was presented as a completed demo")
	}
}

func TestOversizedProjectionDoesNotCreateUnreadEvidence(t *testing.T) {
	dir, run, store := preparedRun(t)
	gate := coordinator(t, dir, run.CaptureID)
	session := startObserver(t, dir)
	callObserver(t, session, "get_capture_status", nil)
	now := time.Now()
	if err := gate.BeginTrial("trial-1", now.Add(-60*time.Second)); err != nil {
		t.Fatal(err)
	}
	var points []*metricspb.NumberDataPoint
	for i := range maxMetricProof {
		point := testutil.Number(now.Add(-30*time.Second), now.Add(-10*time.Second), 1)
		point.Attributes = append(point.Attributes, testutil.Attribute("error.type", fmt.Sprintf("error-%d", i)))
		points = append(points, point)
	}
	request := testutil.Metrics("db.client.operations", metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, points...)
	request.ResourceMetrics[0].Resource.Attributes = gatewayResource()
	if out, err := store.AddMetrics(request); err != nil || out.Accepted != maxMetricProof {
		t.Fatalf("seed high-cardinality counter: %+v %v", out, err)
	}
	callObserver(t, session, "get_capture_status", nil)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_metric_window",
		Arguments: map[string]any{"metric": "db.client.operations", "lookback_seconds": 45, "limit": maxMetricProof}})
	if err == nil || !strings.Contains(err.Error(), "exceeds its byte budget") {
		t.Fatalf("oversized result should give a small retryable error: %+v %v", result, err)
	}
	proof, err := readProof(dir, run)
	if err != nil || len(proof.Observations) != 1 || len(proof.Observations[0].Metrics) != 0 ||
		len(proof.Observations[0].MetricReads) != 0 {
		t.Fatalf("failed response created unread evidence: %+v %v", proof, err)
	}
	callObserver(t, session, "get_metric_window", map[string]any{"metric": "db.client.operations", "lookback_seconds": 45, "limit": 2})
}

func TestFullCoverageReportFromRealMCPAndOTLPTypes(t *testing.T) {
	for _, schema := range []string{"legacy", "current", "local"} {
		t.Run(schema, func(t *testing.T) {
			resource := gatewayResource()
			if schema == "local" {
				resource[0] = testutil.Attribute("service.namespace", Namespace)
			}
			dir, run, store := preparedRun(t)
			gate := coordinator(t, dir, run.CaptureID)
			session := startObserver(t, dir)
			callObserver(t, session, "get_capture_status", nil)
			if err := gate.WaitReady(); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if err := gate.BeginTrial("trial-1", now.Add(-60*time.Second)); err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, spec := range signals {
				if !spec.required {
					continue
				}
				instrument := spec.instruments[0]
				if schema == "current" {
					instrument = spec.instruments[1]
				} else if schema == "local" && spec.name == "request_duration" {
					instrument = spec.instruments[2]
				}
				name := instrument.name
				names = append(names, name)
				point := testutil.Number(now.Add(-30*time.Second), now.Add(-15*time.Second), 150)
				points := []*metricspb.NumberDataPoint{point}
				if spec.name == "request_duration" {
					phase := testutil.Number(now.Add(-30*time.Second), now.Add(-15*time.Second), 2)
					phase.Attributes = append(phase.Attributes, testutil.Attribute("db.operation.phase", "postgres_execution"))
					points = append(points, phase)
				}
				request := testutil.Metrics(name, metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, points...)
				request.ResourceMetrics[0].Resource.Attributes = resource
				metric := request.ResourceMetrics[0].ScopeMetrics[0].Metrics[0]
				if instrument.kind == "histogram" {
					sum := 0.15
					var distributions []*metricspb.HistogramDataPoint
					for _, p := range points {
						distributions = append(distributions, &metricspb.HistogramDataPoint{
							StartTimeUnixNano: p.StartTimeUnixNano, TimeUnixNano: p.TimeUnixNano,
							Attributes: p.Attributes, Count: 150, Sum: &sum,
							ExplicitBounds: []float64{0.001, 0.01}, BucketCounts: []uint64{150, 0, 0},
						})
					}
					if schema != "local" {
						metric.Unit = "s"
					} else {
						metric.Unit = ""
					}
					metric.Data = &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
						AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
						DataPoints:             distributions,
					}}
				}
				if out, err := store.AddMetrics(request); err != nil || out.Accepted != int64(len(points)) {
					t.Fatalf("ingest %s: %+v %v", name, out, err)
				}
			}
			var ids []string
			var spans []*tracepb.Span
			for i := uint64(1); i <= requiredTraces; i++ {
				root := testutil.Span(now.Add(-5*time.Second), i, 1, 0)
				root.Name = "gateway.request"
				execute := testutil.Span(now.Add(-5*time.Second), i, 2, 1)
				execute.Name = "postgres.execute"
				execute.StartTimeUnixNano += uint64(time.Millisecond)
				execute.EndTimeUnixNano -= uint64(time.Millisecond)
				spans = append(spans, root, execute)
				ids = append(ids, hex.EncodeToString(root.TraceId))
			}
			request := testutil.Traces(spans...)
			request.ResourceSpans[0].Resource.Attributes = resource
			if out, err := store.AddTraces(request); err != nil || out.Accepted != int64(len(spans)) {
				t.Fatalf("ingest timelines: %+v %v", out, err)
			}
			callObserver(t, session, "get_capture_status", nil)
			for _, name := range names {
				callObserver(t, session, "get_metric_window", map[string]any{"metric": name, "lookback_seconds": 45, "limit": 60})
			}
			for _, id := range ids {
				callObserver(t, session, "get_trace", map[string]any{"trace_id": id, "limit": maxTraceSpans})
			}
			proof, err := readProof(dir, run)
			if err != nil || len(proof.Observations) != 1 {
				t.Fatalf("load real receipts: %+v %v", proof, err)
			}
			analysis := Analysis{
				RunID: run.ID, CaptureID: run.CaptureID, TrialID: "trial-1",
				Summary:          "Evidence was read through the actual MCP adapter.",
				NextVerification: "Compare another bounded window.",
			}
			for _, row := range proof.Observations[0].coverage() {
				analysis.Coverage = append(analysis.Coverage, AnalysisRow{
					Signal: row.Signal, Status: row.Status, EvidenceIDs: row.EvidenceIDs,
					Observation:    "Selected live evidence is linked by ID.",
					Interpretation: "It supports an operation-level observation.",
					Limitation:     "No complete population or unique diagnosis is established.",
				})
			}
			data, err := json.Marshal(analysis)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := SubmitReport(dir, data); err != nil {
				t.Fatalf("real MCP receipts did not satisfy the report: %v", err)
			}
		})
	}
}

func TestInvalidObservationsDoNotSatisfyLiveReadGate(t *testing.T) {
	now := time.Now()
	trial := &Trial{ID: "trial-1", Start: now.Add(-30 * time.Second), Deadline: now.Add(30 * time.Second)}
	start := now.Add(-10 * time.Second)
	value := int64(7)
	for _, name := range []string{"crossing-interval", "health-gauge", "zero", "wrong-source", "future", "old-trace", "list-only"} {
		t.Run(name, func(t *testing.T) {
			o := &observer{}
			point := capture.Point{
				Name: "db.client.operations", Kind: "sum", Temporality: "delta",
				Source:     capture.Source{Resource: capture.Attributes{"service.name": "documentdb_gateway"}},
				Attributes: capture.Attributes{"db.operation.name": "Find"},
				Start:      &start, Time: now.Add(-time.Second), Arrival: now, Number: &capture.Number{Int: &value},
			}

			span := capture.Span{
				Name: "gateway.request", TraceID: "trace", SpanID: "span",
				Source: point.Source, Attributes: point.Attributes, Start: start, End: now.Add(-time.Second),
			}
			tool := "get_metric_window"
			switch name {
			case "crossing-interval":
				crossing := trial.Start.Add(-time.Second)
				point.Start = &crossing
			case "health-gauge":
				point.Name, point.Kind = "documentdb.postgres.up", "gauge"
			case "zero":
				zero := int64(0)
				point.Number = &capture.Number{Int: &zero}
			case "wrong-source":
				point.Source.Resource["service.name"] = "another-service"
			case "future":
				point.Time = now.Add(time.Second)
			case "old-trace":
				tool = "get_trace"
				span.Start = trial.Start.Add(-time.Second)
			case "list-only":
				tool = "get_recent_traces"
			}
			o.record(tool, "db.client.operations", trial, capture.Reply{
				Points: []capture.Point{point}, Traces: []capture.Trace{{ID: "trace", Spans: []capture.Span{span}}},
			}, now)
			observation := o.proof.Observations[0]
			if len(observation.Traces) != 0 {
				t.Fatalf("invalid observation passed: %+v", observation)
			}
			for _, sample := range observation.Metrics {
				if sample.Signal == "operations" && positive(sample.Value) {
					t.Fatalf("invalid count evidence passed: %+v", sample)
				}
			}
		})
	}
}
