// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package demo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/documentdb/documentdb-kubernetes-operator/documentdb-playground/performance-advisor/internal/capture"
)

func reportFixture(run Run, trial *Trial, at time.Time, modern bool) Interpretation {
	readAt := at.Add(-time.Second)
	o := Observation{TrialID: trial.ID, StatusAt: readAt,
		Warnings: []string{"source_instance_unknown; exports_not_joined"}}
	for i, spec := range signals {
		if !spec.required {
			continue
		}
		instrument := spec.instruments[0]
		if modern {
			instrument = spec.instruments[1]
		}
		start, end, value := readAt.Add(-20*time.Second), readAt.Add(-5*time.Second), int64(150)
		point := capture.Point{
			Name: instrument.name, Kind: instrument.kind, Temporality: "delta",
			SeriesID: fmt.Sprintf("series-%d", i), Start: &start, Time: end, Arrival: readAt,
			Number: &capture.Number{Int: &value}, Unit: "{operation}",
			Source:     capture.Source{Resource: capture.Attributes{"service.name": "documentdb_gateway"}},
			Attributes: capture.Attributes{"db.operation.name": "Find"},
		}
		if spec.name == "request_duration" {
			sum := 0.15
			point.Unit, point.Number = "s", &capture.Number{Double: &sum}
			if modern {
				point.Number = nil
				point.Histogram = &capture.Histogram{Count: 150, Sum: &sum}
			}
		}
		o.Inventory = append(o.Inventory, capture.MetricInfo{Name: point.Name, Kind: point.Kind, Unit: point.Unit, Temporality: "delta"})
		reply := capture.Reply{Points: []capture.Point{point}}
		if spec.name == "request_duration" {
			phase := point
			phase.SeriesID += "-phase"
			phase.Attributes = capture.Attributes{"db.operation.name": "Find", "db.operation.phase": "postgres_execution"}
			reply.Points = append(reply.Points, phase)
		}
		o.recordMetrics(point.Name, trial, reply, readAt)
	}
	for i := range requiredTraces {
		start := readAt.Add(-2 * time.Second)
		root := capture.Span{
			Name: "gateway.request", TraceID: fmt.Sprintf("%032x", i+1), SpanID: "0000000000000001",
			Start: start, End: start.Add(10 * time.Millisecond), Arrival: readAt,
			Source:     capture.Source{Resource: capture.Attributes{"service.name": "documentdb_gateway"}},
			Attributes: capture.Attributes{"db.operation.name": "Find"},
		}
		execute := root
		execute.Name, execute.SpanID, execute.ParentID = "postgres.execute", "0000000000000002", root.SpanID
		execute.Start, execute.End = start.Add(2*time.Millisecond), start.Add(8*time.Millisecond)
		acquire := execute
		acquire.Name, acquire.SpanID, acquire.ParentID = "postgres.acquire_connection", "0000000000000003", execute.SpanID
		acquire.Start, acquire.End = start.Add(3*time.Millisecond), start.Add(4*time.Millisecond)
		o.recordTraces(trial, capture.Reply{Traces: []capture.Trace{{
			ID: root.TraceID, Spans: []capture.Span{root, execute, acquire},
		}}, Warnings: []string{"trace_completeness_unknown; no_end_of_trace_marker"}}, readAt)
	}
	report := Interpretation{ReceivedAt: at, Evidence: o, Analysis: Analysis{
		RunID: run.ID, CaptureID: run.CaptureID, TrialID: trial.ID,
		Summary:          "Selected handling intervals and received timelines support a limited operation-level comparison.",
		NextVerification: "Read another bounded window before attributing the difference to a database cause.",
	}}
	for _, row := range o.coverage() {
		report.Analysis.Coverage = append(report.Analysis.Coverage, AnalysisRow{
			Signal: row.Signal, Status: row.Status, EvidenceIDs: row.EvidenceIDs,
			Observation:    "The referenced receipts contain selected intervals or explicitly unavailable evidence.",
			Interpretation: "This signal provides context for the received request examples.",
			Limitation:     "This is not a complete population, safe cross-instrument join, or CPU measurement.",
		})
	}
	return report
}

func TestCoverageRecognizesBothDurationSchemasWithoutJoining(t *testing.T) {
	now := time.Now()
	trial := &Trial{ID: "trial-1", Start: now.Add(-60 * time.Second), Deadline: now.Add(time.Minute)}
	run := Run{ID: strings.Repeat("a", 32), CaptureID: "capture"}
	for _, modern := range []bool{false, true} {
		report := reportFixture(run, trial, now, modern)
		if err := validateInterpretation(report, run, trial); err != nil {
			t.Fatalf("modern=%t: %v", modern, err)
		}
		for _, sample := range report.Evidence.Metrics {
			if sample.Signal == "request_duration" || sample.Signal == "backend_phases" {
				if (sample.Histogram != nil) != modern || (sample.Value == nil) != modern {
					t.Fatalf("histogram was confused with a duration sum: %+v", sample)
				}
			}
			if sample.InstanceKnown {
				t.Fatal("coverage invented source identity")
			}
		}
		for _, trace := range report.Evidence.Traces {
			if trace.BeforeExecuteNS != int64(2*time.Millisecond) || trace.AfterExecuteNS != int64(2*time.Millisecond) ||
				trace.Spans[2].StartNS <= trace.Spans[1].StartNS {
				t.Fatalf("nested acquisition was moved before execution: %+v", trace)
			}
			if !slices.Contains(trace.UnreceivedPhases, "gateway.auth") {
				t.Fatal("unreceived pre-execute phase was not reported")
			}
		}
	}
}

func TestLateTraceListingDoesNotRepeatAccumulatedEvidence(t *testing.T) {
	now := time.Now()
	run := Run{ID: strings.Repeat("a", 32), CaptureID: "capture",
		Trial: &Trial{ID: "trial-1", Start: now.Add(-time.Minute), Deadline: now.Add(time.Minute)}}
	report := reportFixture(run, run.Trial, now, false)
	for len(report.Evidence.Metrics) < maxMetricProof {
		sample := report.Evidence.Metrics[0]
		sample.SeriesID = strings.Repeat("b", 32)
		report.Evidence.Metrics = append(report.Evidence.Metrics, sample)
	}
	reply := capture.Reply{CaptureID: run.CaptureID, Namespace: Namespace, Cluster: Cluster, Now: now}
	for i := range 5 {
		reply.Traces = append(reply.Traces, capture.Trace{ID: fmt.Sprintf("%032x", i+1), ReceivedSpans: 6,
			Spans: []capture.Span{{Name: "gateway.request", Source: capture.Source{
				Resource: capture.Attributes{"service.name": "documentdb_gateway"}},
				Attributes: capture.Attributes{"db.operation.name": "Find"},
				Start:      now.Add(-time.Second), End: now,
			}},
		})
	}
	view := display("get_recent_traces", "", run, reply, &report.Evidence)
	data, err := json.Marshal(view)
	if err != nil || len(data) > 2500 || len(view.Candidates) != 5 || view.Evidence != nil {
		t.Fatalf("late trace IDs were hidden by accumulated proof: %d bytes, %+v %v", len(data), view, err)
	}
}

func TestMetricProjectionPreservesBoundedExemplarLinks(t *testing.T) {
	now := time.Now()
	trial := &Trial{ID: "trial-1", Start: now.Add(-time.Minute), Deadline: now.Add(time.Minute)}
	start, value := now.Add(-20*time.Second), int64(10)
	point := capture.Point{Name: "docdb.gateway.operations", Kind: "sum", Temporality: "delta",
		Start: &start, Time: now.Add(-time.Second), Arrival: now, Number: &capture.Number{Int: &value},
		Source:     capture.Source{Resource: capture.Attributes{"service.name": "documentdb_gateway"}},
		Attributes: capture.Attributes{"db.operation.name": "Find"}, OmittedExemplars: 1,
	}
	for i := range 3 {
		point.Exemplars = append(point.Exemplars, capture.Exemplar{
			Time: point.Time, Number: *point.Number, TraceID: fmt.Sprintf("%032x", i+1), SpanID: "0000000000000001",
		})
	}
	o := Observation{}
	o.recordMetrics(point.Name, trial, capture.Reply{Points: []capture.Point{point}}, now)
	if len(o.Metrics) != 1 || len(o.Metrics[0].Exemplars) != 2 || o.Metrics[0].OmittedExemplars != 2 ||
		o.Metrics[0].Exemplars[0].TraceID != point.Exemplars[0].TraceID {
		t.Fatalf("metric-to-trace links were discarded or unbounded: %+v", o.Metrics)
	}
}

func TestLocalDurationHistogramDoesNotInventMissingUnits(t *testing.T) {
	now := time.Now()
	trial := &Trial{ID: "trial-1", Start: now.Add(-time.Minute), Deadline: now.Add(time.Minute)}
	start, sum := now.Add(-20*time.Second), 0.25
	point := capture.Point{
		Name: "db.client.operation.duration", Kind: "histogram", Temporality: "delta",
		Start: &start, Time: now.Add(-time.Second), Arrival: now,
		Histogram:  &capture.Histogram{Count: 150, Sum: &sum},
		Source:     capture.Source{Resource: capture.Attributes{"service.name": "documentdb_gateway"}},
		Attributes: capture.Attributes{"db.operation.name": "Find"},
	}
	o := Observation{}
	o.recordMetrics(point.Name, trial, capture.Reply{Points: []capture.Point{point}}, now)
	if len(o.Metrics) != 1 || o.Metrics[0].Histogram.Count != 150 ||
		*o.Metrics[0].Histogram.Sum != sum || o.Metrics[0].Unit != "" ||
		!slices.Contains(o.MetricReads[0].Warnings, "duration_or_size_unit_not_exported; report_unscaled_values_without_inferred_units") {
		t.Fatalf("local histogram units were inferred or missing-unit warning lost: %+v", o)
	}
	point.Kind = "sum"
	o = Observation{}
	o.recordMetrics(point.Name, trial, capture.Reply{Points: []capture.Point{point}}, now)
	if len(o.Metrics) != 0 {
		t.Fatal("local histogram instrument was accepted as a duration sum")
	}
}

func TestReportsRejectUnderuseAndFabricatedEvidence(t *testing.T) {
	now := time.Now()
	run := Run{ID: strings.Repeat("a", 32), CaptureID: "capture"}
	trial := &Trial{ID: "trial-1", Start: now.Add(-60 * time.Second), Deadline: now.Add(time.Minute)}
	for _, name := range []string{
		"ignored-duration", "ignored-bytes", "ignored-documents", "missing-row", "duplicate-row",
		"no-interpretation", "no-limitation", "invented-ref", "wrong-family-ref", "missing-ref",
		"one-trace", "duplicate-trace", "late-report", "foreign-capture", "early-inventory",
		"truncated-inventory", "truncated-evidence", "cross-boundary", "no-request-count",
	} {
		t.Run(name, func(t *testing.T) {
			report := reportFixture(run, trial, now, false)
			switch name {
			case "ignored-duration", "ignored-bytes", "ignored-documents":
				family := map[string]string{"ignored-duration": "request_duration", "ignored-bytes": "response_bytes", "ignored-documents": "returned_documents"}[name]
				report.Evidence.Metrics = slices.DeleteFunc(report.Evidence.Metrics, func(m MetricEvidence) bool { return m.Signal == family })
			case "missing-row":
				report.Analysis.Coverage = report.Analysis.Coverage[1:]
			case "duplicate-row":
				report.Analysis.Coverage[1] = report.Analysis.Coverage[0]
			case "no-interpretation":
				report.Analysis.Coverage[0].Interpretation = ""
			case "no-limitation":
				report.Analysis.Coverage[0].Limitation = " "
			case "invented-ref":
				report.Analysis.Coverage[0].EvidenceIDs = []string{"invented"}
			case "wrong-family-ref":
				report.Analysis.Coverage[0].EvidenceIDs = report.Analysis.Coverage[1].EvidenceIDs
			case "missing-ref":
				report.Analysis.Coverage[0].EvidenceIDs = report.Analysis.Coverage[0].EvidenceIDs[:1]
			case "one-trace":
				report.Evidence.Traces = report.Evidence.Traces[:1]
			case "duplicate-trace":
				report.Evidence.Traces[1] = report.Evidence.Traces[0]
			case "late-report":
				report.ReceivedAt = trial.Deadline
			case "foreign-capture":
				report.Analysis.CaptureID = "another"
			case "early-inventory":
				report.Evidence.StatusAt = trial.Start.Add(time.Second)
			case "truncated-inventory":
				report.Evidence.InventoryTruncated = true
			case "truncated-evidence":
				report.Evidence.EvidenceTruncated = true
			case "cross-boundary":
				start := trial.Start.Add(-time.Second)
				report.Evidence.Metrics[0].Start = &start
			case "no-request-count":
				value := int64(0)
				report.Evidence.Metrics[0].Value = &capture.Number{Int: &value}
			}
			if err := validateInterpretation(report, run, trial); err == nil {
				t.Fatalf("%s passed: %+v", name, report.Analysis)
			}
		})
	}
}

func TestOptionalSignalsMustBeInspectedWhenInventoried(t *testing.T) {
	now := time.Now()
	run := Run{ID: strings.Repeat("a", 32), CaptureID: "capture"}
	trial := &Trial{ID: "trial-1", Start: now.Add(-60 * time.Second), Deadline: now.Add(time.Minute)}
	report := reportFixture(run, trial, now, false)
	name := "docdb.gateway.session.active"
	report.Evidence.Inventory = append(report.Evidence.Inventory, capture.MetricInfo{Name: name, Kind: "gauge"})
	if err := validateInterpretation(report, run, trial); err == nil {
		t.Fatal("available optional metric was silently ignored")
	}
	report.Evidence.recordMetrics(name, trial, capture.Reply{Warnings: []string{"metric_not_observed_in_requested_window"}}, now)
	for i, row := range report.Evidence.coverage() {
		report.Analysis.Coverage[i].Status = row.Status
		report.Analysis.Coverage[i].EvidenceIDs = row.EvidenceIDs
	}
	if err := validateInterpretation(report, run, trial); err != nil {
		t.Fatalf("explicit checked absence should be reportable: %v", err)
	}
}

func TestDurationTypeMigrationIsNotARename(t *testing.T) {
	now := time.Now()
	trial := &Trial{ID: "trial-1", Start: now.Add(-time.Minute), Deadline: now.Add(time.Minute)}
	start, value := now.Add(-20*time.Second), int64(1)
	for _, kind := range []string{"sum", "exponential_histogram", "summary"} {
		o := Observation{}
		point := capture.Point{
			Name: "docdb.gateway.operation.duration", Kind: kind, Temporality: "delta", Start: &start,
			Time: now.Add(-time.Second), Arrival: now, Number: &capture.Number{Int: &value},
			Source:     capture.Source{Resource: capture.Attributes{"service.name": "documentdb_gateway"}},
			Attributes: capture.Attributes{"db.operation.name": "Find"},
		}
		o.recordMetrics(point.Name, trial, capture.Reply{Points: []capture.Point{point}}, now)
		if len(o.Metrics) != 0 {
			t.Fatalf("%s was presented as the supported histogram: %+v", kind, o.Metrics)
		}
	}
}

func TestCumulativeEvidenceRequiresKnownStreamAndCoveredDifference(t *testing.T) {
	now := time.Now()
	trial := &Trial{ID: "trial-1", Start: now.Add(-time.Minute), Deadline: now.Add(time.Minute)}
	epoch, start, end, value := now.Add(-time.Hour), now.Add(-30*time.Second), now.Add(-time.Second), int64(12)
	for _, name := range []string{"safe", "unknown-instance", "no-baseline", "crossing"} {
		t.Run(name, func(t *testing.T) {
			point := capture.Point{
				Name: "docdb.gateway.operations", Kind: "sum", Temporality: "cumulative", SeriesID: "series",
				Start: &epoch, Time: end, Arrival: now, Number: &capture.Number{Int: &value},
				Source:     capture.Source{Resource: capture.Attributes{"service.name": "documentdb_gateway"}, InstanceKnown: true},
				Attributes: capture.Attributes{"db.operation.name": "Find"},
			}
			reply := capture.Reply{Points: []capture.Point{point}, Aggregates: []capture.Aggregate{{
				SeriesID: "series", Kind: "sum", Start: &start, End: &end, Value: &capture.Number{Int: &value},
			}}}
			switch name {
			case "unknown-instance":
				reply.Points[0].Source.InstanceKnown = false
			case "no-baseline":
				reply.Aggregates = nil
			case "crossing":
				reply.Aggregates[0].Start = &epoch
			}
			o := Observation{}
			o.recordMetrics(point.Name, trial, reply, now)
			if (len(o.Metrics) == 1) != (name == "safe") {
				t.Fatalf("unsafe cumulative evidence: %+v", o.Metrics)
			}
		})
	}
}

func TestStreamAcceptsLiveReportAndPreservesTimestampedEvidence(t *testing.T) {
	dir, run, _ := preparedRun(t)
	gate := coordinator(t, dir, run.CaptureID)
	now := time.Now()
	if err := gate.BeginTrial("trial-1", now.Add(-60*time.Second)); err != nil {
		t.Fatal(err)
	}
	report := reportFixture(run, gate.run.Trial, now, true)
	saveProof(t, dir, Proof{RunID: run.ID, CaptureID: run.CaptureID, Ready: now, LastRead: now, Calls: 12,
		Observations: []Observation{report.Evidence}})
	data, err := json.Marshal(report.Analysis)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := StreamOutput(dir, strings.NewReader("Beginning analysis.\n"+reportPrefix+string(data)+"\n"), &output); err != nil {
		t.Fatal(err)
	}
	stored, err := loadInterpretation(dir, run, gate.run.Trial)
	if err != nil || stored.ReceivedAt.Before(now) || !strings.Contains(output.String(), "Live report accepted") {
		t.Fatalf("report was not validated live: %v %s", err, output.String())
	}
	log, err := os.ReadFile(filepath.Join(dir, "observer.txt"))
	if err != nil || !bytes.Contains(log, []byte("\t"+reportPrefix)) {
		t.Fatalf("timestamped report missing: %v", err)
	}
	if _, err := SubmitReport(dir, data); err == nil {
		t.Fatal("second report overwrote the first accepted report")
	}
}

func TestInvalidOrOversizedTranscriptCancelsInsteadOfPassing(t *testing.T) {
	for _, input := range []string{
		reportPrefix + "{\"summary\":\"unlinked\"}\n",
		reportPrefix + strings.Repeat("x", maxReportBytes+1),
		strings.Repeat("short line\n", maxTranscriptBytes/10),
	} {
		dir, _, _ := preparedRun(t)
		if err := StreamOutput(dir, strings.NewReader(input), &bytes.Buffer{}); err == nil {
			t.Fatal("invalid observer output passed")
		}
		if _, err := os.Stat(filepath.Join(dir, "cancel")); err != nil {
			t.Fatalf("invalid report did not stop the workload: %v", err)
		}
	}
}
