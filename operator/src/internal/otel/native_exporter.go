// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package otel

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	dbpreview "github.com/documentdb/documentdb-operator/api/preview"
)

// ValidateNativeTraceExporter validates the opt-in source and its trace destination.
func ValidateNativeTraceExporter(spec *dbpreview.MonitoringSpec) field.ErrorList {
	if spec == nil || spec.NativeTraceExporter == nil {
		return nil
	}
	base := field.NewPath("spec", "monitoring")
	var errs field.ErrorList
	if !spec.Enabled {
		errs = append(errs, field.Invalid(base.Child("enabled"), false, "native trace export requires monitoring"))
	}
	if spec.Exporter == nil || (!spec.Exporter.TraceFile &&
		(spec.Exporter.OTLP == nil || strings.TrimSpace(spec.Exporter.OTLP.Endpoint) == "")) {
		errs = append(errs, field.Required(base.Child("exporter"), "native trace export requires a traceFile or OTLP destination"))
	}
	native := spec.NativeTraceExporter
	if strings.TrimSpace(native.Image) == "" {
		errs = append(errs, field.Required(base.Child("nativeTraceExporter", "image"), "an explicit compatible exporter image is required"))
	}
	if problems := validation.IsDNS1123Subdomain(native.ConnectionSecretName); len(problems) > 0 {
		errs = append(errs, field.Invalid(base.Child("nativeTraceExporter", "connectionSecretName"),
			native.ConnectionSecretName, strings.Join(problems, "; ")))
	}
	return errs
}
