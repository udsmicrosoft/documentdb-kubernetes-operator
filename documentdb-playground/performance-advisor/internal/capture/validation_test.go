// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package capture

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/documentdb/documentdb-kubernetes-operator/documentdb-playground/performance-advisor/internal/testutil"
)

func TestDeploymentNamespaceBindings(t *testing.T) {
	for _, test := range []struct {
		name  string
		attrs []*commonpb.KeyValue
		valid bool
	}{
		{"kubernetes", []*commonpb.KeyValue{testutil.Attribute("k8s.namespace.name", testutil.Namespace)}, true},
		{"local", []*commonpb.KeyValue{testutil.Attribute("service.namespace", testutil.Namespace)}, true},
		{"consistent", []*commonpb.KeyValue{
			testutil.Attribute("service.namespace", testutil.Namespace),
			testutil.Attribute("k8s.namespace.name", testutil.Namespace),
		}, true},
		{"missing", nil, false},
		{"wrong-local", []*commonpb.KeyValue{testutil.Attribute("service.namespace", "other")}, false},
		{"conflicting", []*commonpb.KeyValue{
			testutil.Attribute("service.namespace", testutil.Namespace),
			testutil.Attribute("k8s.namespace.name", "other"),
		}, false},
		{"redacted-conflict", []*commonpb.KeyValue{
			testutil.Attribute("service.namespace", "invalid namespace"),
			testutil.Attribute("k8s.namespace.name", testutil.Namespace),
		}, false},
		{"duplicate", []*commonpb.KeyValue{
			testutil.Attribute("service.namespace", testutil.Namespace),
			testutil.Attribute("service.namespace", testutil.Namespace),
		}, false},
		{"wrong-type", []*commonpb.KeyValue{{
			Key: "service.namespace", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1}},
		}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newStore(t, nil)
			attrs := append([]*commonpb.KeyValue{
				testutil.Attribute("documentdb.cluster", testutil.Cluster),
				testutil.Attribute("service.instance.id", "local-instance"),
			}, test.attrs...)
			metrics := testutil.Metrics("test.requests", delta, testutil.Number(testNow.Add(-time.Second), testNow, 1))
			metrics.ResourceMetrics[0].Resource.Attributes = attrs
			out, err := store.AddMetrics(metrics)
			if err != nil || (out.Accepted == 1) != test.valid || (!test.valid && out.Rejected != 1) {
				t.Fatalf("metric scope admission: %+v %v", out, err)
			}
			traces := testutil.Traces(testutil.Span(testNow, 1, 1, 0))
			traces.ResourceSpans[0].Resource.Attributes = attrs
			out, err = store.AddTraces(traces)
			if err != nil || (out.Accepted == 1) != test.valid || (!test.valid && out.Rejected != 1) {
				t.Fatalf("trace scope admission: %+v %v", out, err)
			}
			if test.valid {
				reply := metricReply(t, store)
				if !reply.Points[0].Source.InstanceKnown {
					t.Fatal("local instance identity was lost")
				}
				if test.name == "local" {
					resource := reply.Points[0].Source.Resource
					if resource["service.namespace"] != testutil.Namespace || resource["k8s.namespace.name"] != nil {
						t.Fatalf("local scope must not invent Kubernetes metadata: %+v", resource)
					}
				}
			}
		})
	}
}

func TestUnknownDiagnosticNamesArePseudonymized(t *testing.T) {
	store := newStore(t, nil)
	span := testutil.Span(testNow, 1, 1, 0)
	span.Name = "find/SECRET_ACCOUNT_123"
	request := testutil.Traces(span)
	request.ResourceSpans[0].ScopeSpans[0].Scope.Name = "reader/SECRET_SCOPE_123"
	request.ResourceSpans[0].ScopeSpans[0].Scope.Version = "v1-SECRET_VERSION_123"
	out, err := store.AddTraces(request)
	if err != nil || out.Accepted != 1 {
		t.Fatalf("admit diagnostic names: %+v %v", out, err)
	}

	reply, err := store.TraceByID(hex.EncodeToString(span.TraceId), 10)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(reply)
	if err != nil || strings.Contains(string(data), "SECRET") {
		t.Fatalf("identifier-shaped sensitive name leaked: %v", err)
	}
	got := reply.Traces[0].Spans[0]
	if !strings.HasPrefix(got.Name, "redacted-") || got.Redacted != 1 || got.Source.RedactedFields != 2 {
		t.Fatalf("name redaction was not explicit: %+v", got)
	}
	if out, err = store.AddTraces(request); err != nil || out.Duplicates != 1 {
		t.Fatalf("name pseudonymization broke deduplication: %+v %v", out, err)
	}
}

func TestKnownGatewayPreparationSpansRetainNamesButNotSensitiveAttributes(t *testing.T) {
	for _, name := range []string{"gateway.read_request", "gateway.format_request", "gateway.auth"} {
		t.Run(name, func(t *testing.T) {
			store := newStore(t, nil)
			span := testutil.Span(testNow, 1, 1, 0)
			span.Name = name
			span.Attributes = append(span.Attributes, testutil.Attribute("db.query.text", "SECRET_REQUEST"))
			if out, err := store.AddTraces(testutil.Traces(span)); err != nil || out.Accepted != 1 {
				t.Fatalf("admit known preparation span: %+v %v", out, err)
			}
			reply, err := store.TraceByID(hex.EncodeToString(span.TraceId), 10)
			if err != nil || reply.Traces[0].Spans[0].Name != name {
				t.Fatalf("known phase was pseudonymized: %v %+v", err, reply)
			}
			data, err := json.Marshal(reply)
			if err != nil || strings.Contains(string(data), "SECRET_REQUEST") {
				t.Fatalf("preparation span leaked request content: %v", err)
			}
		})
	}
}

func TestMetricExemplarCorrelationAndRedaction(t *testing.T) {
	for _, kind := range []string{"sum", "histogram"} {
		t.Run(kind, func(t *testing.T) {
			store := newStore(t, nil)
			span := testutil.Span(testNow.Add(-time.Second), 1, 1, 0)
			if _, err := store.AddTraces(testutil.Traces(span)); err != nil {
				t.Fatal(err)
			}
			sample := &metricspb.Exemplar{
				TimeUnixNano: span.EndTimeUnixNano, TraceId: span.TraceId, SpanId: span.SpanId,
				Value: &metricspb.Exemplar_AsInt{AsInt: 3},
				FilteredAttributes: []*commonpb.KeyValue{
					testutil.Attribute("db.query.text", "SECRET_EXEMPLAR_CONTENT"),
					testutil.Attribute("db.operation.name", "find"),
				},
			}
			point := testutil.Number(testNow.Add(-30*time.Second), testNow, 3)
			point.Exemplars = []*metricspb.Exemplar{sample}
			request := testutil.Metrics("test.requests", delta, point)
			if kind == "histogram" {
				sum := 3.0
				request.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Data = &metricspb.Metric_Histogram{
					Histogram: &metricspb.Histogram{
						AggregationTemporality: delta,
						DataPoints: []*metricspb.HistogramDataPoint{{
							StartTimeUnixNano: point.StartTimeUnixNano, TimeUnixNano: point.TimeUnixNano,
							Count: 1, Sum: &sum, Exemplars: []*metricspb.Exemplar{sample},
						}},
					},
				}
			}
			admitMetrics(t, store, request)
			reply := metricReply(t, store)
			if len(reply.Points) != 1 || len(reply.Points[0].Exemplars) != 1 {
				t.Fatalf("metric exemplar not preserved: %+v", reply.Points)
			}
			exemplar := reply.Points[0].Exemplars[0]
			if exemplar.TraceID != hex.EncodeToString(span.TraceId) || exemplar.SpanID != hex.EncodeToString(span.SpanId) ||
				integer(t, &exemplar.Number) != 3 || exemplar.Redacted != 1 {
				t.Fatalf("exemplar association changed: %+v", exemplar)
			}
			trace, err := store.TraceByID(exemplar.TraceID, 10)
			if err != nil || len(trace.Traces) != 1 || trace.Traces[0].Spans[0].SpanID != exemplar.SpanID {
				t.Fatalf("exemplar cannot be followed to its received span: %+v %v", trace, err)
			}
			data, err := json.Marshal(reply)
			if err != nil || strings.Contains(string(data), "SECRET") {
				t.Fatalf("exemplar attributes leaked: %v", err)
			}
		})
	}
}

func TestExemplarLimitsAndMalformedSamplesAreReported(t *testing.T) {
	store := newStore(t, nil)
	point := testutil.Number(testNow.Add(-time.Second), testNow, 3)
	for range MaxExemplars + 2 {
		point.Exemplars = append(point.Exemplars, &metricspb.Exemplar{
			TimeUnixNano: uint64(testNow.UnixNano()), Value: &metricspb.Exemplar_AsInt{AsInt: 1},
		})
	}
	point.Exemplars[0].TraceId = []byte{1}
	out, err := store.AddMetrics(testutil.Metrics("test.requests", delta, point))
	if err != nil || out.Accepted != 1 || out.Rejected != 0 || out.Reasons["metric_exemplars_omitted"] != 3 {
		t.Fatalf("exemplar omissions not acknowledged correctly: %+v %v", out, err)
	}
	reply := metricReply(t, store)
	if len(reply.Points[0].Exemplars) != MaxExemplars-1 || reply.Points[0].OmittedExemplars != 3 ||
		!warningContains(reply.Warnings, "exemplars_omitted") ||
		store.CaptureStatus().Status.Counters.OmittedExemplars != 3 {
		t.Fatalf("exemplar limit or omission counter incorrect: %+v", reply)
	}
}

func TestHistogramSumInvariants(t *testing.T) {
	zero, one, negative := 0.0, 1.0, -1.0
	for _, sum := range []*float64{&one, &negative} {
		if _, reason := histogram(&metricspb.HistogramDataPoint{Count: 0, Sum: sum}); reason == "" {
			t.Fatal("nonzero empty-population histogram sum accepted")
		}
	}
	if _, reason := histogram(&metricspb.HistogramDataPoint{Count: 0, Sum: &zero}); reason != "" {
		t.Fatalf("valid empty histogram rejected: %s", reason)
	}
	if _, reason := histogram(&metricspb.HistogramDataPoint{Count: 1, Min: &negative}); reason != "" {
		t.Fatalf("negative population without a sum should remain supported: %s", reason)
	}
	before := 10.0
	for _, after := range []float64{9, 11} {
		if _, reason := histogramArithmetic(
			&Histogram{Count: 2, Sum: &after, Bounds: []float64{10}, BucketCounts: []uint64{2, 0}},
			&Histogram{Count: 2, Sum: &before, Bounds: []float64{10}, BucketCounts: []uint64{2, 0}}, true); reason == "" {
			t.Fatal("zero-count difference returned a nonzero sum")
		}
	}
}

func TestFreshMetricDoesNotConcealStaleGauge(t *testing.T) {
	store := newStore(t, nil)
	now := testNow.Add(-3 * time.Minute)
	store.now = func() time.Time { return now }
	store.started = now
	request := testutil.Metrics("test.requests", delta)
	request.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Data = &metricspb.Metric_Gauge{
		Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{testutil.Number(time.Time{}, now, 42)}},
	}
	admitMetrics(t, store, request)
	now = testNow
	admitMetrics(t, store, testutil.Metrics("test.active", delta, testutil.Number(now.Add(-time.Second), now, 1)))
	if warningContains(store.CaptureStatus().Warnings, "metrics_event_time_stale") {
		t.Fatal("fixture must have a fresh global metrics signal")
	}
	reply, err := store.MetricWindow("test.requests", 5*time.Minute, 100)
	if err != nil || len(reply.Aggregates) != 1 {
		t.Fatalf("query stale gauge: %+v %v", reply, err)
	}
	if integer(t, reply.Aggregates[0].Value) != 42 ||
		!warningContains(reply.Aggregates[0].Warnings, "event_time_stale") ||
		!warningContains(reply.Aggregates[0].Warnings, "arrival_stale") ||
		!warningContains(reply.Warnings, "stale_event_times") {
		t.Fatalf("fresh traffic concealed an old selected gauge: %+v", reply)
	}
}
