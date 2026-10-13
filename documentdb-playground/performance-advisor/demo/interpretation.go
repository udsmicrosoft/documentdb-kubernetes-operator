// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package demo

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const reportPrefix = "DEMO_REPORT "
const maxReportBytes = 32 << 10
const maxTranscriptBytes = 512 << 10

// AnalysisRow links the observer's explanation to specific, verified read receipts.
type AnalysisRow struct {
	Signal         string   `json:"signal"`
	Status         string   `json:"status"`
	EvidenceIDs    []string `json:"evidence_ids"`
	Observation    string   `json:"observation"`
	Interpretation string   `json:"interpretation"`
	Limitation     string   `json:"limitation"`
}

// Analysis is emitted by the agent as one bounded JSON line, not through a new tool.
type Analysis struct {
	RunID            string        `json:"run_id"`
	CaptureID        string        `json:"capture_session_id"`
	TrialID          string        `json:"trial_id"`
	Summary          string        `json:"summary"`
	Coverage         []AnalysisRow `json:"coverage"`
	NextVerification string        `json:"next_verification"`
}

// Interpretation freezes the exact receipts available when the report was received.
type Interpretation struct {
	ReceivedAt time.Time   `json:"received_at"`
	Analysis   Analysis    `json:"analysis"`
	Evidence   Observation `json:"evidence"`
}

func reportPath(dir, trialID string) (string, error) {
	if trialID != "trial-1" && trialID != "trial-2" && trialID != "trial-3" {
		return "", fmt.Errorf("invalid report trial ID")
	}
	return filepath.Join(dir, "analysis-"+trialID+".json"), nil
}

func validText(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 1200
}

func validateInterpretation(report Interpretation, run Run, trial *Trial) error {
	analysis, evidence := report.Analysis, report.Evidence
	if analysis.RunID != run.ID || analysis.CaptureID != run.CaptureID || analysis.TrialID != trial.ID ||
		evidence.TrialID != trial.ID || !inWindow(report.ReceivedAt, trial) {
		return fmt.Errorf("report identity or receive time does not match the active trial")
	}
	if !validText(analysis.Summary) || !validText(analysis.NextVerification) {
		return fmt.Errorf("report requires a bounded summary and next verification")
	}
	if !inWindow(evidence.StatusAt, trial) || evidence.StatusAt.After(report.ReceivedAt) ||
		evidence.StatusAt.Sub(trial.Start) < 30*time.Second || evidence.InventoryTruncated || evidence.EvidenceTruncated {
		return fmt.Errorf("report requires an untruncated in-window inventory read after the first 30 seconds")
	}
	for _, read := range evidence.MetricReads {
		if !inWindow(read.At, trial) || read.At.After(report.ReceivedAt) {
			return fmt.Errorf("metric read is not live evidence for this report")
		}
	}
	foundRequest := false
	for _, sample := range evidence.Metrics {
		if !inWindow(sample.ReadAt, trial) || sample.ReadAt.After(report.ReceivedAt) ||
			!inWindow(sample.End, trial) || sample.End.After(sample.ReadAt) || sample.Arrival.After(sample.ReadAt) ||
			(sample.Kind != "gauge" && (sample.Start == nil || sample.Start.Before(trial.Start) || !sample.Start.Before(sample.End))) {
			return fmt.Errorf("metric interval is not live evidence for this report")
		}
		if sample.Signal == "operations" && strings.EqualFold(sample.Operation, "find") && positive(sample.Value) {
			foundRequest = true
		}
	}
	if !foundRequest || len(evidence.Traces) != requiredTraces {
		return fmt.Errorf("report requires a positive Find interval and three distinct detailed request timelines")
	}
	seenTraces := map[string]bool{}
	for _, trace := range evidence.Traces {
		if seenTraces[trace.TraceID] || !inWindow(trace.ReadAt, trial) || trace.ReadAt.After(report.ReceivedAt) ||
			trace.Start.Before(trial.Start) || !inWindow(trace.End, trial) || trace.End.After(trace.ReadAt) {
			return fmt.Errorf("trace examples must be distinct and received within this trial")
		}
		seenTraces[trace.TraceID] = true
	}
	expected := evidence.coverage()
	if len(analysis.Coverage) != len(expected) {
		return fmt.Errorf("report must address all %d signal families", len(expected))
	}
	seen := map[string]bool{}
	for _, row := range analysis.Coverage {
		index := slices.IndexFunc(expected, func(c Coverage) bool { return c.Signal == row.Signal })
		if seen[row.Signal] || index < 0 {
			return fmt.Errorf("duplicate or unknown report family %q", row.Signal)
		}
		seen[row.Signal] = true
		required := expected[index]
		if required.Status == "pending" || row.Status != required.Status {
			return fmt.Errorf("%s coverage is %s, not %s; inspect its current receipts", row.Signal, required.Status, row.Status)
		}
		for _, spec := range signals {
			if spec.name == row.Signal && spec.required && required.Status != "observed" {
				return fmt.Errorf("the packaged demo requires usable %s evidence", row.Signal)
			}
		}
		actualIDs, requiredIDs := slices.Clone(row.EvidenceIDs), slices.Clone(required.EvidenceIDs)
		slices.Sort(actualIDs)
		slices.Sort(requiredIDs)
		if !slices.Equal(actualIDs, requiredIDs) {
			return fmt.Errorf("%s must cite its exact current evidence_ids; reads alone do not pass", row.Signal)
		}
		if !validText(row.Observation) || !validText(row.Interpretation) || !validText(row.Limitation) {
			return fmt.Errorf("%s needs an observation, interpretation, and limitation, each at most 1200 bytes", row.Signal)
		}
	}
	return nil
}

// SubmitReport accepts one live, evidence-linked report through local harness IPC.
// It checks coverage and timing, not the semantic correctness of model prose.
func SubmitReport(dir string, data []byte) (Interpretation, error) {
	report := Interpretation{ReceivedAt: time.Now().UTC()}
	if len(data) > maxReportBytes {
		return report, fmt.Errorf("report exceeds %d bytes", maxReportBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report.Analysis); err != nil {
		return report, fmt.Errorf("decode observer report: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return report, fmt.Errorf("observer report contains trailing data")
	}
	run, err := LoadRun(dir)
	if err != nil {
		return report, err
	}
	if run.Phase != "active" || run.Trial == nil {
		return report, fmt.Errorf("reports must be received while their trial is active")
	}
	proof, err := readProof(dir, run)
	if err != nil {
		return report, err
	}
	if proof.Disconnected {
		return report, fmt.Errorf("cannot submit a report after the observer disconnects")
	}
	index := slices.IndexFunc(proof.Observations, func(o Observation) bool { return o.TrialID == run.Trial.ID })
	if index < 0 {
		return report, fmt.Errorf("no live evidence has been read for this trial")
	}
	report.Evidence = proof.Observations[index]
	if err := validateInterpretation(report, run, run.Trial); err != nil {
		return report, err
	}
	path, err := reportPath(dir, run.Trial.ID)
	if err != nil {
		return report, err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return report, fmt.Errorf("this trial already has a report or its report cannot be inspected")
	}
	if err := writeJSON(path, report); err != nil {
		return report, err
	}
	return report, nil
}

func loadInterpretation(dir string, run Run, trial *Trial) (Interpretation, error) {
	var report Interpretation
	path, err := reportPath(dir, trial.ID)
	if err != nil {
		return report, err
	}
	if err := readJSON(path, &report); err != nil {
		return report, err
	}
	return report, validateInterpretation(report, run, trial)
}

// StreamOutput timestamps a bounded transcript and validates report lines as they arrive.
func StreamOutput(dir string, input io.Reader, output io.Writer) (err error) {
	if _, err := LoadRun(dir); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, "observer.txt"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			err = errors.Join(err, os.WriteFile(filepath.Join(dir, "cancel"), nil, 0600))
		}
	}()
	scanner := bufio.NewScanner(io.LimitReader(input, maxTranscriptBytes+1))
	scanner.Buffer(make([]byte, 4096), maxReportBytes+len(reportPrefix)+1)
	total := 0
	for scanner.Scan() {
		line := scanner.Text()
		stamp := time.Now().UTC().Format(time.RFC3339Nano)
		total += len(stamp) + len(line) + 2
		if total > maxTranscriptBytes {
			return fmt.Errorf("observer transcript exceeded its byte limit")
		}
		if _, err := fmt.Fprintf(file, "%s\t%s\n", stamp, line); err != nil {
			return err
		}
		if strings.HasPrefix(line, reportPrefix) {
			report, err := SubmitReport(dir, []byte(strings.TrimPrefix(line, reportPrefix)))
			if err != nil {
				return fmt.Errorf("live report rejected: %w", err)
			}
			if _, err := fmt.Fprintf(output, "\n%s: %s\n", report.Analysis.TrialID, report.Analysis.Summary); err != nil {
				return err
			}
			for _, row := range report.Analysis.Coverage {
				if _, err := fmt.Fprintf(output, "  %s [%s; %s]: %s %s Limitation: %s\n",
					row.Signal, row.Status, strings.Join(row.EvidenceIDs, ","), row.Observation, row.Interpretation, row.Limitation); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(output, "  Next verification: %s\n  Live report accepted at %s.\n\n",
				report.Analysis.NextVerification, report.ReceivedAt.Format(time.RFC3339Nano)); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintln(output, line); err != nil {
			return err
		}
	}
	return scanner.Err()
}
