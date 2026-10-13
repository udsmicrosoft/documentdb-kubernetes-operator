// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package demo

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/documentdb/documentdb-kubernetes-operator/documentdb-playground/performance-advisor/internal/capture"
)

const (
	requiredTraces = 3
	maxMetricProof = 48
	maxTraceSpans  = 24
)

type instrument struct {
	name string
	kind string
}

type signal struct {
	name        string
	instruments []instrument
	required    bool
}

func counters(suffix string) []instrument {
	return []instrument{{"db.client." + suffix, "sum"}, {"docdb.gateway." + suffix, "sum"}}
}

var durationInstruments = []instrument{
	{"db.client.operation.duration.total", "sum"},
	{"docdb.gateway.operation.duration", "histogram"},
	{"db.client.operation.duration", "histogram"},
}

var signals = []signal{
	{"operations", counters("operations"), true},
	{"request_duration", durationInstruments, true},
	{"backend_phases", durationInstruments, false},
	{"request_bytes", counters("request.size.total"), true},
	{"response_bytes", counters("response.size.total"), true},
	{"returned_documents", counters("documents.returned"), true},
	{"health", []instrument{{"documentdb.postgres.up", "gauge"}}, false},
	{"gateway_resources", []instrument{
		{"docdb.gateway.session.active", "gauge"},
		{"docdb.gateway.session.opened", "sum"},
		{"docdb.gateway.session.closed", "sum"},
		{"docdb.gateway.session.expired", "sum"},
		{"docdb.gateway.transaction.active", "gauge"},
		{"docdb.gateway.transaction.started", "sum"},
		{"docdb.gateway.transaction.ended", "sum"},
		{"docdb.gateway.cursor.active", "gauge"},
		{"docdb.gateway.cursor.opened", "sum"},
		{"docdb.gateway.cursor.ended", "sum"},
		{"gateway.starts", "sum"},
		{"gateway_startup_delay_ms", "histogram"},
	}, false},
}

// IsOperationMetric recognizes the two explicitly supported counter schemas.
func IsOperationMetric(name, kind string) bool {
	return slices.Contains(signals[0].instruments, instrument{name, kind})
}

// MetricEvidence is one received interval or a safe same-stream cumulative difference.
// Metric-specific series identities do not establish a cross-instrument join.
type MetricEvidence struct {
	ID               string             `json:"id"`
	Signal           string             `json:"signal"`
	ReadAt           time.Time          `json:"read_at"`
	Name             string             `json:"name"`
	Kind             string             `json:"type"`
	Unit             string             `json:"unit"`
	Temporality      string             `json:"temporality"`
	SeriesID         string             `json:"series_id"`
	InstanceKnown    bool               `json:"instance_identity_known"`
	Operation        string             `json:"operation,omitempty"`
	Phase            string             `json:"phase,omitempty"`
	Error            string             `json:"error_type,omitempty"`
	Outcome          string             `json:"resource_outcome,omitempty"`
	Start            *time.Time         `json:"start,omitempty"`
	End              time.Time          `json:"end"`
	Arrival          time.Time          `json:"arrival"`
	Value            *capture.Number    `json:"value,omitempty"`
	Histogram        *Distribution      `json:"histogram,omitempty"`
	Exemplars        []capture.Exemplar `json:"exemplars,omitempty"`
	OmittedExemplars int                `json:"omitted_exemplars,omitempty"`
}

// Distribution preserves a single population's summary, never an inferred percentile.
type Distribution struct {
	Count uint64   `json:"count,string"`
	Sum   *float64 `json:"sum,omitempty"`
	Min   *float64 `json:"min,omitempty"`
	Max   *float64 `json:"max,omitempty"`
}

// MetricRead distinguishes a checked but unavailable signal from an ignored one.
type MetricRead struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	At        time.Time `json:"at"`
	Warnings  []string  `json:"warnings"`
	Truncated bool      `json:"truncated"`
}

// SpanTiming retains parent relationships and offsets, including overlapping phases.
type SpanTiming struct {
	Name     string `json:"name"`
	ID       string `json:"span_id"`
	ParentID string `json:"parent_span_id,omitempty"`
	StartNS  int64  `json:"start_offset_ns"`
	EndNS    int64  `json:"end_offset_ns"`
	Status   string `json:"status"`
}

// TraceEvidence is a received example, not a representative sample or a complete trace.
type TraceEvidence struct {
	ID               string       `json:"id"`
	ReadAt           time.Time    `json:"read_at"`
	TraceID          string       `json:"trace_id"`
	RequestSpanID    string       `json:"request_span_id"`
	Start            time.Time    `json:"request_start"`
	End              time.Time    `json:"request_end"`
	Spans            []SpanTiming `json:"timeline"`
	BeforeExecuteNS  int64        `json:"request_before_first_execute_ns"`
	AfterExecuteNS   int64        `json:"request_after_last_execute_ns"`
	UnreceivedPhases []string     `json:"phases_not_received_in_example"`
	Warnings         []string     `json:"warnings"`
	Sampled          bool         `json:"sampled"`
	MissingParents   []string     `json:"missing_parent_ids"`
	ConflictingSpans int          `json:"conflicting_duplicates"`
}

// Coverage describes exactly what a report must address, using adapter-issued IDs.
type Coverage struct {
	Signal      string   `json:"signal"`
	Status      string   `json:"status"`
	Metrics     []string `json:"metrics,omitempty"`
	EvidenceIDs []string `json:"evidence_ids"`
	Detail      string   `json:"detail,omitempty"`
}

func text(attrs capture.Attributes, keys ...string) string {
	for _, key := range keys {
		if value, ok := attrs[key]; ok {
			return fmt.Sprint(value)
		}
	}
	return ""
}

func positive(value *capture.Number) bool {
	return value != nil && ((value.Int != nil && *value.Int > 0) || (value.Double != nil && *value.Double > 0))
}

func metricSignal(point capture.Point) string {
	for _, spec := range signals {
		if !slices.Contains(spec.instruments, instrument{point.Name, point.Kind}) {
			continue
		}
		phase := text(point.Attributes, "db.operation.phase", "db.client.operation.phase", "phase")
		if spec.name == "request_duration" && phase != "" {
			return "backend_phases"
		}
		return spec.name
	}
	return ""
}

func (o *Observation) recordMetrics(name string, trial *Trial, reply capture.Reply, at time.Time) {
	known := false
	for _, spec := range signals {
		for _, item := range spec.instruments {
			known = known || item.name == name
		}
	}
	if !known {
		return
	}
	read := MetricRead{Name: name, At: at, Warnings: reply.Warnings, Truncated: reply.Truncated}
	if slices.ContainsFunc(reply.Points, func(point capture.Point) bool {
		family := metricSignal(point)
		return point.Name == name && point.Unit == "" &&
			(family == "request_duration" || family == "backend_phases" || family == "request_bytes" || family == "response_bytes")
	}) {
		read.Warnings = append(read.Warnings, "duration_or_size_unit_not_exported; report_unscaled_values_without_inferred_units")
	}
	for _, aggregate := range reply.Aggregates {
		for _, warning := range aggregate.Warnings {
			if !slices.Contains(read.Warnings, warning) {
				read.Warnings = append(read.Warnings, warning)
			}
		}
	}
	index := slices.IndexFunc(o.MetricReads, func(r MetricRead) bool { return r.Name == name })
	if index < 0 {
		read.ID = fmt.Sprintf("read%d", len(o.MetricReads)+1)
		o.MetricReads = append(o.MetricReads, read)
	} else {
		read.ID = o.MetricReads[index].ID
		o.MetricReads[index] = read
	}
	for _, point := range reply.Points {
		family := metricSignal(point)
		if point.Name != name || family == "" || point.NoRecorded ||
			!inWindow(point.Time, trial) || point.Time.After(at) || point.Arrival.After(at) ||
			at.Sub(point.Time) > 45*time.Second || at.Sub(point.Arrival) > 45*time.Second {
			continue
		}
		if family != "health" && point.Source.Resource["service.name"] != "documentdb_gateway" {
			continue
		}
		operation := text(point.Attributes, "db.operation.name")
		if family != "operations" && family != "health" && family != "gateway_resources" &&
			!strings.EqualFold(operation, "find") {
			continue
		}
		value, distribution, start, end := point.Number, point.Histogram, point.Start, point.Time
		temporality := point.Temporality
		if point.Kind != "gauge" {
			if point.Temporality == "cumulative" {
				value, distribution, start = nil, nil, nil
				for _, aggregate := range reply.Aggregates {
					if aggregate.SeriesID == point.SeriesID && aggregate.Start != nil && aggregate.End != nil &&
						point.Source.InstanceKnown && aggregate.End.Equal(point.Time) {
						value, distribution, start, end = aggregate.Value, aggregate.Histogram, aggregate.Start, *aggregate.End
						temporality = "cumulative_difference"
					}
				}
			} else if point.Temporality != "delta" {
				continue
			}
			if start == nil || start.Before(trial.Start) || !start.Before(end) {
				continue
			}
		}
		if value == nil && distribution == nil {
			continue
		}
		evidence := MetricEvidence{
			Signal: family, ReadAt: at, Name: point.Name, Kind: point.Kind, Unit: point.Unit,
			Temporality: temporality, SeriesID: point.SeriesID, InstanceKnown: point.Source.InstanceKnown,
			Operation: operation, Phase: text(point.Attributes, "db.operation.phase", "db.client.operation.phase", "phase"),
			Error:   text(point.Attributes, "error.type"),
			Outcome: text(point.Attributes, "docdb.transaction.end.outcome", "docdb.cursor.end.reason"),
			Start:   start, End: end, Arrival: point.Arrival, Value: value,
		}
		if distribution != nil {
			evidence.Histogram = &Distribution{distribution.Count, distribution.Sum, distribution.Min, distribution.Max}
		}
		evidence.Exemplars = slices.Clone(point.Exemplars[:min(2, len(point.Exemplars))])
		evidence.OmittedExemplars = point.OmittedExemplars + len(point.Exemplars) - len(evidence.Exemplars)
		sameVariant, duplicate := 0, false
		for _, prior := range o.Metrics {
			if prior.Name == evidence.Name && prior.Operation == evidence.Operation &&
				prior.Phase == evidence.Phase && prior.Error == evidence.Error && prior.Outcome == evidence.Outcome {
				sameVariant++
			}
			duplicate = duplicate || (prior.SeriesID == evidence.SeriesID && prior.End.Equal(evidence.End))
		}
		if duplicate || sameVariant >= 2 {
			continue
		}
		if len(o.Metrics) == maxMetricProof {
			o.EvidenceTruncated = true
			continue
		}
		evidence.ID = fmt.Sprintf("m%d", len(o.Metrics)+1)
		o.Metrics = append(o.Metrics, evidence)
	}
}

func (o *Observation) recordTraces(trial *Trial, reply capture.Reply, at time.Time) {
	for _, trace := range reply.Traces {
		if len(o.Traces) >= requiredTraces || trace.ResponseTruncated || len(trace.Spans) > maxTraceSpans ||
			slices.ContainsFunc(o.Traces, func(t TraceEvidence) bool { return t.TraceID == trace.ID }) {
			continue
		}
		for _, root := range trace.Spans {
			if root.Name != "gateway.request" || root.Source.Resource["service.name"] != "documentdb_gateway" ||
				!strings.EqualFold(text(root.Attributes, "db.operation.name"), "find") ||
				root.Start.Before(trial.Start) || !inWindow(root.End, trial) ||
				root.End.After(at) || root.Arrival.After(at) {
				continue
			}
			evidence := TraceEvidence{
				ID: fmt.Sprintf("t%d", len(o.Traces)+1), ReadAt: at, TraceID: trace.ID,
				RequestSpanID: root.SpanID, Start: root.Start, End: root.End, Warnings: reply.Warnings,
				Sampled: root.Sampled, MissingParents: trace.MissingParents, ConflictingSpans: trace.ConflictingSpans,
			}
			var first, last time.Time
			for _, span := range trace.Spans {
				evidence.Spans = append(evidence.Spans, SpanTiming{
					Name: span.Name, ID: span.SpanID, ParentID: span.ParentID,
					StartNS: span.Start.Sub(root.Start).Nanoseconds(), EndNS: span.End.Sub(root.Start).Nanoseconds(), Status: span.Status,
				})
				if span.Name == "postgres.execute" && descendantOf(span, root.SpanID, trace.Spans) &&
					!span.Start.Before(root.Start) && !span.End.After(root.End) {
					if first.IsZero() || span.Start.Before(first) {
						first = span.Start
					}
					if last.IsZero() || span.End.After(last) {
						last = span.End
					}
				}
			}
			if first.IsZero() {
				continue
			}
			evidence.BeforeExecuteNS, evidence.AfterExecuteNS = first.Sub(root.Start).Nanoseconds(), root.End.Sub(last).Nanoseconds()
			for _, name := range []string{"gateway.read_request", "gateway.format_request", "gateway.auth",
				"postgres.acquire_connection", "postgres.transaction", "gateway.process_request", "gateway.write_response"} {
				if !slices.ContainsFunc(trace.Spans, func(s capture.Span) bool { return s.Name == name }) {
					evidence.UnreceivedPhases = append(evidence.UnreceivedPhases, name)
				}
			}
			o.Traces = append(o.Traces, evidence)
			break
		}
	}
}

func descendantOf(span capture.Span, parent string, spans []capture.Span) bool {
	for range len(spans) {
		if span.ParentID == parent {
			return true
		}
		index := slices.IndexFunc(spans, func(s capture.Span) bool { return s.SpanID == span.ParentID })
		if index < 0 {
			return false
		}
		span = spans[index]
	}
	return false
}

func (o Observation) coverage() []Coverage {
	var rows []Coverage
	for _, spec := range signals {
		row := Coverage{Signal: spec.name, Status: "pending", EvidenceIDs: []string{"capture"}}
		for _, info := range o.Inventory {
			if slices.ContainsFunc(spec.instruments, func(i instrument) bool { return i.name == info.Name }) &&
				!slices.Contains(row.Metrics, info.Name) {
				row.Metrics = append(row.Metrics, info.Name)
			}
		}
		row.Detail = "Read each inventoried instrument and explain its types, dimensions, intervals, and coverage."
		if !o.StatusAt.IsZero() && !o.InventoryTruncated {
			if len(row.Metrics) == 0 {
				row.Status, row.Detail = "unavailable", "Not present in the retained inventory; not a zero measurement."
			} else {
				allRead, allObserved := true, true
				row.EvidenceIDs = nil
				for _, name := range row.Metrics {
					index := slices.IndexFunc(o.MetricReads, func(r MetricRead) bool { return r.Name == name })
					if index < 0 {
						allRead = false
						continue
					}
					row.EvidenceIDs = append(row.EvidenceIDs, o.MetricReads[index].ID)
					observed := false
					for _, sample := range o.Metrics {
						if sample.Name == name && sample.Signal == spec.name {
							row.EvidenceIDs = append(row.EvidenceIDs, sample.ID)
							observed = true
						}
					}
					allObserved = allObserved && observed
				}
				if allRead && allObserved {
					row.Status = "observed"
				} else if allRead && !spec.required {
					row.Status, row.Detail = "unavailable", "Inspected, but no admissible in-window sample for every instrument; consult read warnings and types."
				}
			}
		}
		rows = append(rows, row)
	}
	quality := Coverage{Signal: "capture_quality", Status: "pending", EvidenceIDs: []string{"capture"},
		Detail: "Check event/arrival freshness, source identity, receiver losses, retention, and truncation; upstream losses remain unknown."}
	if !o.StatusAt.IsZero() && !o.InventoryTruncated {
		quality.Status = "observed"
	}
	rows = append(rows, quality)
	timeline := Coverage{Signal: "trace_timeline", Status: "pending",
		Detail: "Three distinct received examples: parent/child offsets, pre-execute, acquisition, execute, response, overlap, and unreceived phases. Do not add nested durations."}
	for _, trace := range o.Traces {
		timeline.EvidenceIDs = append(timeline.EvidenceIDs, trace.ID)
	}
	if len(o.Traces) == requiredTraces {
		timeline.Status = "observed"
	}
	rows = append(rows, timeline, Coverage{Signal: "write_documents", Status: "not_applicable",
		EvidenceIDs: []string{"capture"}, Detail: "The measured workload is read-only; seed writes are outside these trial windows. Returned documents are not examined documents."})
	return rows
}
