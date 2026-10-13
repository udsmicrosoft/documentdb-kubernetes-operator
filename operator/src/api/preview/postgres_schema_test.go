// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package preview

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

type schemaCase struct {
	name    string
	value   string
	wantErr bool
}

func TestPostgresExtensionSchema(t *testing.T) {
	validateSchemaCases(t, "postgres", []schemaCase{
		{"omitted", `{}`, false},
		{"pgsentinel", `{"extensions":[{"name":"pgsentinel","image":{"reference":"pgsentinel:1.5.1"}}],"sharedPreloadLibraries":["pgsentinel"]}`, false},
		{"reserved image", `{"extensions":[{"name":"documentdb","image":{"reference":"other:1"}}]}`, true},
		{"missing image", `{"extensions":[{"name":"pgsentinel"}]}`, true},
		{"empty reference", `{"extensions":[{"name":"pgsentinel","image":{"reference":""}}]}`, true},
		{"library path", `{"sharedPreloadLibraries":["/tmp/library"]}`, true},
		{"library list", `{"sharedPreloadLibraries":["first,second"]}`, true},
		{"empty library", `{"sharedPreloadLibraries":[""]}`, true},
	})
}

func TestNativeTraceExporterSchema(t *testing.T) {
	validateSchemaCases(t, "monitoring", []schemaCase{
		{"omitted", `{}`, false},
		{"trace file", `{"enabled":true,"exporter":{"traceFile":true},"nativeTraceExporter":{"image":"native:test","connectionSecretName":"reader"}}`, false},
		{"OTLP", `{"enabled":true,"exporter":{"otlp":{"endpoint":"collector:4317"}},"nativeTraceExporter":{"image":"native:test","connectionSecretName":"reader"}}`, false},
		{"disabled", `{"enabled":false,"exporter":{"traceFile":true},"nativeTraceExporter":{"image":"native:test","connectionSecretName":"reader"}}`, true},
		{"no destination", `{"enabled":true,"nativeTraceExporter":{"image":"native:test","connectionSecretName":"reader"}}`, true},
		{"metrics only", `{"enabled":true,"exporter":{"prometheus":{"port":8888}},"nativeTraceExporter":{"image":"native:test","connectionSecretName":"reader"}}`, true},
		{"missing secret", `{"enabled":true,"exporter":{"traceFile":true},"nativeTraceExporter":{"image":"native:test"}}`, true},
		{"missing image", `{"enabled":true,"exporter":{"traceFile":true},"nativeTraceExporter":{"connectionSecretName":"reader"}}`, true},
	})
}

func validateSchemaCases(t *testing.T, property string, cases []schemaCase) {
	t.Helper()
	data, err := os.ReadFile("../../config/crd/bases/documentdb.io_dbs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	pg := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties[property]
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&pg, &internal, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := schema.NewStructural(&internal)
	if err != nil {
		t.Fatal(err)
	}
	openAPI, _, err := validation.NewSchemaValidator(&internal)
	if err != nil {
		t.Fatal(err)
	}
	celValidator := cel.NewValidator(structural, false, celconfig.PerCallLimit)

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal([]byte(test.value), &value); err != nil {
				t.Fatal(err)
			}
			path := field.NewPath("spec", property)
			errs := validation.ValidateCustomResource(path, value, openAPI)
			if celValidator != nil {
				celErrs, _ := celValidator.Validate(context.Background(), path, structural, value, nil, celconfig.RuntimeCELCostBudget)
				errs = append(errs, celErrs...)
			}
			if (len(errs) > 0) != test.wantErr {
				t.Fatalf("validation errors = %v, want error = %v", errs, test.wantErr)
			}
		})
	}
}
