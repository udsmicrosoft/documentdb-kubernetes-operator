// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package otel

import (
	"testing"

	dbpreview "github.com/documentdb/documentdb-operator/api/preview"
)

func TestNativeTraceExporterRequiresAnExplicitSourceAndTraceDestination(t *testing.T) {
	for _, test := range []struct {
		name      string
		enabled   bool
		image     string
		secret    string
		exporter  *dbpreview.ExporterSpec
		wantError bool
	}{
		{"trace files", true, "native:test", "native-reader", &dbpreview.ExporterSpec{TraceFile: true}, false},
		{"OTLP", true, "native:test", "native-reader", &dbpreview.ExporterSpec{OTLP: &dbpreview.OTLPExporterSpec{Endpoint: "collector:4317"}}, false},
		{"disabled", false, "native:test", "native-reader", &dbpreview.ExporterSpec{TraceFile: true}, true},
		{"no destination", true, "native:test", "native-reader", nil, true},
		{"metrics only", true, "native:test", "native-reader", &dbpreview.ExporterSpec{Prometheus: &dbpreview.PrometheusExporterSpec{}}, true},
		{"no image", true, "", "native-reader", &dbpreview.ExporterSpec{TraceFile: true}, true},
		{"invalid secret", true, "native:test", "../reader", &dbpreview.ExporterSpec{TraceFile: true}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := &dbpreview.MonitoringSpec{
				Enabled: test.enabled, Exporter: test.exporter,
				NativeTraceExporter: &dbpreview.NativeTraceExporterSpec{Image: test.image, ConnectionSecretName: test.secret},
			}
			_, err := GenerateConfigMapData("cluster", "namespace", spec)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, want error = %v", err, test.wantError)
			}
		})
	}
}
