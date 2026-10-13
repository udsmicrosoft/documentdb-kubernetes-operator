// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package performance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/documentdb/documentdb-kubernetes-operator/documentdb-playground/performance-advisor/demo"
	"github.com/documentdb/documentdb-kubernetes-operator/documentdb-playground/performance-advisor/workload"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/documentdb/documentdb-operator/test/e2e"
	"github.com/documentdb/documentdb-operator/test/e2e/pkg/e2eutils/fixtures"
	wire "github.com/documentdb/documentdb-operator/test/e2e/pkg/e2eutils/mongo"
	sharedwire "github.com/documentdb/documentdb-operator/test/shared/mongo"
)

var _ = Describe("Live telemetry observation trials",
	Label(e2e.PerformanceLabel, e2e.SlowLabel, "live-telemetry-trials"), Serial, func() {
		It("records three neutral operation-level observation windows", func(ctx SpecContext) {
			requireTelemetryEnvironment(ctx)
			session, err := openTelemetryClient(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(session.Close()).To(Succeed()) })
			initial, err := telemetryTool(ctx, session, "get_capture_status", nil)
			Expect(err).NotTo(HaveOccurred())
			var gate *demo.Coordinator
			var workCtx context.Context = ctx
			if dir := os.Getenv("E2E_LIVE_TELEMETRY_DEMO_DIR"); dir != "" {
				gate, err = demo.OpenCoordinator(ctx, dir, initial.CaptureID)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { Expect(gate.Close()).To(Succeed()) })
				Expect(gate.WaitReady()).To(Succeed(), "a real observer must read status before workload setup")
				workCtx = gate.Context()
			}
			handle, err := wire.NewFromDocumentDB(workCtx, e2e.SuiteEnv(), telemetryNamespace, telemetryCluster)
			Expect(err).NotTo(HaveOccurred())
			database := fixtures.DBNameFor("live-telemetry-observation-trials")
			DeferCleanup(func(cleanupCtx SpecContext) {
				Expect(sharedwire.DropDatabase(cleanupCtx, handle.Client(), database)).To(Succeed())
				Expect(handle.Close(cleanupCtx)).To(Succeed())
			})
			collection := handle.Database(database).Collection("observations")
			Expect(workload.Run(workCtx, collection, gate, func(trial demo.Trial) error {
				id, start, ended, operations := trial.ID, trial.Start, trial.End, trial.Operations
				fmt.Fprintf(GinkgoWriter, "live-telemetry-trial id=%s start=%s nominal_duration=%s capture=%s\n",
					id, start.UTC().Format(time.RFC3339Nano), trial.Deadline.Sub(start), initial.CaptureID)
				view, err := telemetryTool(ctx, session, "get_recent_traces", map[string]any{
					"lookback_seconds": 30, "limit": 20,
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(view.CaptureID).To(Equal(initial.CaptureID))
				type evidence struct {
					Trial       string             `json:"trial"`
					CaptureID   string             `json:"capture_session_id"`
					Start       time.Time          `json:"start"`
					End         time.Time          `json:"end"`
					Operations  int                `json:"harness_operations"`
					TraceID     string             `json:"example_trace_id"`
					PhaseMillis map[string]float64 `json:"example_span_elapsed_ms"`
					Warnings    []string           `json:"coverage_warnings"`
				}
				summary := evidence{
					Trial: id, CaptureID: initial.CaptureID, Start: start.UTC(), End: ended.UTC(),
					Operations: operations, Warnings: view.Warnings,
				}
				for _, trace := range view.Traces {
					for _, span := range trace.Spans {
						operation, _ := span.Attributes["db.operation.name"].(string)
						if span.Name == "gateway.request" && strings.EqualFold(operation, "find") &&
							!span.Start.Before(start) && !span.End.After(ended) {
							detail, err := telemetryTool(ctx, session, "get_trace", map[string]any{"trace_id": trace.ID, "limit": 100})
							Expect(err).NotTo(HaveOccurred())
							Expect(detail.Traces).To(HaveLen(1))
							summary.TraceID = trace.ID
							summary.PhaseMillis = make(map[string]float64)
							for _, observed := range detail.Traces[0].Spans {
								summary.PhaseMillis[observed.Name] = float64(observed.End.Sub(observed.Start)) / float64(time.Millisecond)
							}
							break
						}
					}
					if summary.TraceID != "" {
						break
					}
				}
				Expect(summary.TraceID).NotTo(BeEmpty(), "each trial needs a received request trace")
				data, err := json.Marshal(summary)
				Expect(err).NotTo(HaveOccurred())
				fmt.Fprintf(GinkgoWriter, "live-telemetry-evidence %s\n", data)
				return nil
			})).To(Succeed())
		}, SpecTimeout(16*time.Minute))
	})
