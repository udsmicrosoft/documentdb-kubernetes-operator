// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package demo

import (
	"time"

	"github.com/documentdb/documentdb-kubernetes-operator/documentdb-playground/performance-advisor/internal/capture"
)

// The client can hide an entire oversized tool result, even below the capture's
// HTTP limit. Project selected evidence instead of repeating the full proof.
const maxObserverResponseBytes = 12 << 10

type traceCandidate struct {
	ID             string    `json:"trace_id"`
	Operation      string    `json:"operation,omitempty"`
	Start          time.Time `json:"request_start"`
	End            time.Time `json:"request_end"`
	ReceivedSpans  int       `json:"received_spans"`
	InCurrentTrial bool      `json:"in_current_trial"`
	Truncated      bool      `json:"truncated"`
}

type readEvidence struct {
	MetricRead *MetricRead      `json:"metric_read,omitempty"`
	Metrics    []MetricEvidence `json:"metrics,omitempty"`
	Traces     []TraceEvidence  `json:"traces,omitempty"`
}

type observerView struct {
	CaptureID  string           `json:"capture_session_id"`
	Namespace  string           `json:"namespace"`
	Cluster    string           `json:"cluster"`
	AsOf       time.Time        `json:"as_of"`
	Since      *time.Time       `json:"since,omitempty"`
	Until      *time.Time       `json:"until,omitempty"`
	Warnings   []string         `json:"warnings"`
	Truncated  bool             `json:"upstream_truncated"`
	Omitted    int              `json:"upstream_omitted_items"`
	Status     *capture.Status  `json:"status,omitempty"`
	Candidates []traceCandidate `json:"trace_candidates,omitempty"`
	Evidence   *readEvidence    `json:"live_reads,omitempty"`
	Selection  string           `json:"selection"`
}

func display(name, metric string, run Run, reply capture.Reply, observation *Observation) observerView {
	view := observerView{
		CaptureID: reply.CaptureID, Namespace: reply.Namespace, Cluster: reply.Cluster, AsOf: reply.Now,
		Since: reply.Since, Until: reply.Until, Warnings: reply.Warnings, Truncated: reply.Truncated, Omitted: reply.Omitted,
		Selection: "Bounded examples, not a complete population. Earlier evidence remains in prior tool results; original read_at timestamps are preserved.",
	}
	switch name {
	case "get_capture_status":
		view.Status = reply.Status
	case "get_recent_traces":
		for _, trace := range reply.Traces {
			candidate := traceCandidate{ID: trace.ID, ReceivedSpans: trace.ReceivedSpans, Truncated: trace.ResponseTruncated}
			for _, span := range trace.Spans {
				if span.Name == "gateway.request" && span.Source.Resource["service.name"] == "documentdb_gateway" {
					candidate.Operation = text(span.Attributes, "db.operation.name")
					candidate.Start, candidate.End = span.Start, span.End
					candidate.InCurrentTrial = run.Trial != nil && !span.Start.Before(run.Trial.Start) && inWindow(span.End, run.Trial)
					break
				}
			}
			view.Candidates = append(view.Candidates, candidate)
		}
	case "get_metric_window":
		view.Selection = "At most two selected intervals per visible metric variant, without joining identities. Empty evidence is unavailable, not zero."
		if observation != nil {
			view.Evidence = &readEvidence{}
			for i := range observation.MetricReads {
				if observation.MetricReads[i].Name == metric {
					view.Evidence.MetricRead = &observation.MetricReads[i]
				}
			}
			for _, sample := range observation.Metrics {
				if sample.Name == metric {
					view.Evidence.Metrics = append(view.Evidence.Metrics, sample)
				}
			}
		}
	case "get_trace":
		view.Selection = "One received timeline, never asserted complete. A receipt requires a current Find request and its descendant execute span; retry a fresh ID if empty."
		if observation != nil {
			view.Evidence = &readEvidence{}
			for _, trace := range reply.Traces {
				for _, sample := range observation.Traces {
					if sample.TraceID == trace.ID {
						view.Evidence.Traces = append(view.Evidence.Traces, sample)
					}
				}
			}
		}
	}
	return view
}
