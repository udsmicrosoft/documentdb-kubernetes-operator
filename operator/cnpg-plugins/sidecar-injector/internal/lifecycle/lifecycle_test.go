// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lifecycle

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	cnpglifecycle "github.com/cloudnative-pg/cnpg-i/pkg/lifecycle"
	jsonpatch "github.com/evanphx/json-patch/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/documentdb/cnpg-i-sidecar-injector/internal/config"
	pluginmetadata "github.com/documentdb/cnpg-i-sidecar-injector/pkg/metadata"
)

func gatewayContainer(env ...corev1.EnvVar) *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "postgres"},
				{Name: gatewayContainerName, Env: env},
				{Name: "otel-collector"},
			},
		},
	}
}

func envNames(env []corev1.EnvVar) []string {
	out := make([]string, len(env))
	for i, e := range env {
		out[i] = e.Name
	}
	return out
}

func TestGetCapabilitiesOnlyHandlesPodCreate(t *testing.T) {
	response, err := Implementation{}.GetCapabilities(
		context.Background(),
		&cnpglifecycle.OperatorLifecycleCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("GetCapabilities() error: %v", err)
	}
	if len(response.LifecycleCapabilities) != 1 {
		t.Fatalf("capabilities count = %d, want 1", len(response.LifecycleCapabilities))
	}

	capability := response.LifecycleCapabilities[0]
	if capability.Group != "" || capability.Kind != "Pod" {
		t.Fatalf("capability = group %q kind %q, want core Pod", capability.Group, capability.Kind)
	}
	if len(capability.OperationTypes) != 1 {
		t.Fatalf("operation count = %d, want 1", len(capability.OperationTypes))
	}
	if capability.OperationTypes[0].Type != cnpglifecycle.OperatorOperationType_TYPE_CREATE {
		t.Fatalf("operation = %s, want TYPE_CREATE", capability.OperationTypes[0].Type)
	}
}

func TestLifecycleHookPatchDoesNotMutatePod(t *testing.T) {
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-1",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "postgres"}},
		},
	}

	response, err := Implementation{}.LifecycleHook(context.Background(), &cnpglifecycle.OperatorLifecycleRequest{
		OperationType: &cnpglifecycle.OperatorOperationType{
			Type: cnpglifecycle.OperatorOperationType_TYPE_PATCH,
		},
		ObjectDefinition: mustMarshal(t, pod),
	})
	if err != nil {
		t.Fatalf("LifecycleHook() error: %v", err)
	}
	if len(response.JsonPatch) != 0 {
		t.Fatalf("PATCH operation returned pod mutations: %s", string(response.JsonPatch))
	}
}

// TestInjectGatewayOTelEnv_AllAppended covers the CREATE case: empty gateway
// env, both OTel env vars appended in declaration order. Per-pod attribution
// (k8s.pod.name) is added by the collector's resource processor downstream,
// so we don't need POD_NAME / OTEL_RESOURCE_ATTRIBUTES on the gateway.
func TestInjectGatewayOTelEnv_AllAppended(t *testing.T) {
	pod := gatewayContainer()
	injectGatewayOTelEnv(pod, true, false)

	got := envNames(pod.Spec.Containers[1].Env)
	want := []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_METRICS_ENABLED"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envs got %v, want %v", got, want)
	}
}

// TestInjectGatewayOTelEnv_MetricsEnabledPresent guards against future
// removal of OTEL_METRICS_ENABLED — without this env, the pgmongo gateway
// silently disables OTLP metrics export.
func TestInjectGatewayOTelEnv_MetricsEnabledPresent(t *testing.T) {
	pod := gatewayContainer()
	injectGatewayOTelEnv(pod, true, false)
	for _, e := range pod.Spec.Containers[1].Env {
		if e.Name == "OTEL_METRICS_ENABLED" {
			if e.Value != "true" {
				t.Errorf("OTEL_METRICS_ENABLED = %q, want %q", e.Value, "true")
			}
			return
		}
	}
	t.Fatal("OTEL_METRICS_ENABLED env var missing — pgmongo gateway will not initialize OTel")
}

// TestInjectGatewayOTelEnv_PreservesExisting verifies the dedup logic: a
// pre-existing env var with the same name as one of the OTel envs is left
// untouched (we don't overwrite), and the others are still appended.
func TestInjectGatewayOTelEnv_PreservesExisting(t *testing.T) {
	pod := gatewayContainer(corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://custom:4317"})
	injectGatewayOTelEnv(pod, true, false)

	env := pod.Spec.Containers[1].Env
	if len(env) != 2 {
		t.Errorf("expected 2 env vars, got %d (%v)", len(env), envNames(env))
	}
	// Pre-existing endpoint must be unchanged.
	for _, e := range env {
		if e.Name == "OTEL_EXPORTER_OTLP_ENDPOINT" && e.Value != "http://custom:4317" {
			t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT was overwritten: got %q, want %q", e.Value, "http://custom:4317")
		}
	}
}

// TestInjectGatewayOTelEnv_Idempotent verifies that repeated construction does
// not duplicate environment variables on an already-mutated pod.
func TestInjectGatewayOTelEnv_Idempotent(t *testing.T) {
	pod := gatewayContainer()
	injectGatewayOTelEnv(pod, true, false)
	first := append([]corev1.EnvVar(nil), pod.Spec.Containers[1].Env...)

	injectGatewayOTelEnv(pod, true, false)
	second := pod.Spec.Containers[1].Env

	if !reflect.DeepEqual(first, second) {
		t.Errorf("second invocation changed Env slice:\nfirst:  %v\nsecond: %v", envNames(first), envNames(second))
	}
}

// TestInjectGatewayOTelEnv_NoGatewayContainer is a no-op safety check.
func TestInjectGatewayOTelEnv_NoGatewayContainer(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "postgres"}},
		},
	}
	injectGatewayOTelEnv(pod, true, false)
	if len(pod.Spec.Containers[0].Env) != 0 {
		t.Errorf("expected no envs on non-gateway container, got %v", envNames(pod.Spec.Containers[0].Env))
	}
}

func TestLifecycleHookMonitoring(t *testing.T) {
	for _, test := range []struct {
		name           string
		files          string
		metrics        string
		prometheusPort string
		healthPort     int32
		native         bool
	}{
		{"legacy OTLP monitoring", "", "", "", 13133, false},
		{"files and HTTP metrics", "true", "", "8888", 13133, false},
		{"files only", "true", "false", "", 13133, false},
		{"custom HTTP metrics", "", "", "9090", 13133, false},
		{"HTTP metrics on the default health port", "", "", "13133", 13134, false},
		{"native traces with files", "true", "false", "", 13133, true},
		{"native traces with OTLP", "", "", "", 13133, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			enabled := true
			uid, gid := int64(12345), int64(23456)
			cluster := &apiv1.Cluster{
				TypeMeta:   metav1.TypeMeta{APIVersion: apiv1.SchemeGroupVersion.String(), Kind: apiv1.ClusterKind},
				ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "default"},
				Spec: apiv1.ClusterSpec{
					Plugins: []apiv1.PluginConfiguration{{
						Name: pluginmetadata.PluginName, Enabled: &enabled,
						Parameters: map[string]string{
							"otelCollectorImage": "otel:test",
							"otelConfigMapName":  "otel-config",
							"otelTraceFile":      test.files,
							"otelMetricsEnabled": test.metrics,
							"otelMemoryLimit":    "128Mi",
							"prometheusPort":     test.prometheusPort,
						},
					}},
				},
			}
			if test.native {
				for name, value := range map[string]string{
					"nativeTraceExporterImage":         "native:test",
					"nativeTraceExporterSecret":        "native-reader",
					"nativeTraceExporterCluster":       "logical-cluster",
					"nativeTraceExporterMemoryRequest": "32Mi",
					"nativeTraceExporterMemoryLimit":   "128Mi",
					"nativeTraceExporterCpuRequest":    "25m",
					"nativeTraceExporterCpuLimit":      "200m",
				} {
					cluster.Spec.Plugins[0].Parameters[name] = value
				}
			}
			pod := &corev1.Pod{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1", Namespace: "default",
					Labels:      map[string]string{"cnpg.io/cluster": "cluster"},
					Annotations: map[string]string{"cnpg.io/operatorVersion": "test"},
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &gid, FSGroup: &gid},
					Containers:      []corev1.Container{{Name: "postgres"}},
				},
			}
			response, err := Implementation{}.LifecycleHook(context.Background(), &cnpglifecycle.OperatorLifecycleRequest{
				OperationType:     &cnpglifecycle.OperatorOperationType{Type: cnpglifecycle.OperatorOperationType_TYPE_CREATE},
				ClusterDefinition: mustMarshal(t, cluster),
				ObjectDefinition:  mustMarshal(t, pod),
			})
			if err != nil {
				t.Fatal(err)
			}
			patch, err := jsonpatch.DecodePatch(response.JsonPatch)
			if err != nil {
				t.Fatal(err)
			}
			mutated, err := patch.Apply(mustMarshal(t, pod))
			if err != nil {
				t.Fatal(err)
			}
			var result corev1.Pod
			if err := json.Unmarshal(mutated, &result); err != nil {
				t.Fatal(err)
			}
			if test.native {
				if len(result.Spec.InitContainers) != 1 {
					t.Fatalf("expected one native exporter, got %+v", result.Spec.InitContainers)
				}
				exporter := result.Spec.InitContainers[0]
				if exporter.Name != "native-trace-exporter" || exporter.Image != "native:test" ||
					exporter.RestartPolicy == nil || *exporter.RestartPolicy != corev1.ContainerRestartPolicyAlways ||
					exporter.ReadinessProbe != nil || exporter.StartupProbe != nil {
					t.Fatalf("native exporter must start independently of source/delivery readiness: %+v", exporter)
				}
				if !reflect.DeepEqual(exporter.Args, []string{"serve-native", "default", "logical-cluster"}) {
					t.Fatalf("wrong native source scope: %v", exporter.Args)
				}
				if len(exporter.VolumeMounts) != 0 || exporter.SecurityContext.ReadOnlyRootFilesystem == nil ||
					!*exporter.SecurityContext.ReadOnlyRootFilesystem {
					t.Fatal("exporter must not mount PostgreSQL data or require a writable root filesystem")
				}
				assertResourceQuantity(t, exporter.Resources.Limits, corev1.ResourceMemory, "128Mi")
				assertResourceQuantity(t, exporter.Resources.Requests, corev1.ResourceCPU, "25m")
				assertEnvValue(t, exporter.Env, "DOCUMENTDB_TELEMETRY_OTLP_ENDPOINT", "http://127.0.0.1:4317")
				ref := exporter.Env[0].ValueFrom.SecretKeyRef
				if exporter.Env[0].Value != "" || ref.Name != "native-reader" || ref.Key != "uri" {
					t.Fatal("native SQL connection must come only from the named Secret")
				}
			} else if len(result.Spec.InitContainers) != 0 {
				t.Fatal("native export must remain opt-in")
			}
			if !reflect.DeepEqual(result.Spec.SecurityContext, pod.Spec.SecurityContext) {
				t.Fatal("trace files must preserve the CNPG pod identity")
			}
			containers := map[string]corev1.Container{}
			for _, container := range result.Spec.Containers {
				containers[container.Name] = container
			}
			gateway := containers[gatewayContainerName]
			collector := containers[otelCollectorContainerName]
			probe := collector.ReadinessProbe
			if probe == nil || probe.HTTPGet == nil {
				t.Fatal("monitoring must have an HTTP readiness probe for every exporter mode")
			}
			if probe.HTTPGet.Path != "/healthz" || probe.HTTPGet.Port.String() != "otel-health" {
				t.Fatalf("readiness must use the dedicated health endpoint: %+v", probe.HTTPGet)
			}
			ports := map[string]int32{}
			for _, port := range collector.Ports {
				ports[port.Name] = port.ContainerPort
			}
			if ports["otel-health"] != test.healthPort {
				t.Fatalf("health port = %d, want %d", ports["otel-health"], test.healthPort)
			}
			assertEnvValue(t, collector.Env, "OTEL_HEALTH_CHECK_PORT", strconv.FormatInt(int64(test.healthPort), 10))
			if test.prometheusPort == "" {
				if _, ok := ports["prom-metrics"]; ok || result.Annotations["prometheus.io/scrape"] == "true" {
					t.Fatal("readiness must not enable Prometheus scraping")
				}
			} else if strconv.FormatInt(int64(ports["prom-metrics"]), 10) != test.prometheusPort ||
				result.Annotations["prometheus.io/port"] != test.prometheusPort ||
				result.Annotations["prometheus.io/path"] != "/metrics" {
				t.Fatal("readiness must preserve the configured Prometheus endpoint and scrape annotations")
			}
			wantMetrics := "true"
			if test.metrics != "" {
				wantMetrics = test.metrics
			}
			assertEnvValue(t, gateway.Env, "OTEL_METRICS_ENABLED", wantMetrics)
			fileMode := test.files == "true"
			traceVolumeFound := false
			for _, volume := range result.Spec.Volumes {
				if volume.Name == "otel-traces" {
					traceVolumeFound = true
					if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit == nil ||
						volume.EmptyDir.SizeLimit.Cmp(resource.MustParse("64Mi")) != 0 || volume.EmptyDir.Medium != "" {
						t.Fatalf("trace volume must be a bounded disk-backed emptyDir: %+v", volume)
					}
				}
			}
			if traceVolumeFound != fileMode {
				t.Fatalf("trace volume present = %v, want %v", traceVolumeFound, fileMode)
			}
			for _, name := range []string{"postgres", otelCollectorContainerName} {
				found := false
				for _, mount := range containers[name].VolumeMounts {
					if mount.Name == "otel-traces" {
						found = true
						if mount.MountPath != "/var/lib/otel/traces" || mount.ReadOnly != (name == "postgres") {
							t.Fatalf("unexpected trace mount in %s: %+v", name, mount)
						}
					}
				}
				if found != fileMode {
					t.Fatalf("%s trace mount present = %v, want %v", name, found, fileMode)
				}
			}
			if fileMode {
				assertEnvValue(t, gateway.Env, "OTEL_TRACES_ENABLED", "true")
				assertEnvValue(t, collector.Env, "GOMEMLIMIT", "67108864")
				assertResourceQuantity(t, collector.Resources.Requests, corev1.ResourceEphemeralStorage, "64Mi")
				if _, limited := collector.Resources.Limits[corev1.ResourceEphemeralStorage]; limited {
					t.Fatal("trace mode must not cap pod-wide storage for database and gateway logs")
				}
				if collector.SecurityContext.ReadOnlyRootFilesystem == nil || !*collector.SecurityContext.ReadOnlyRootFilesystem {
					t.Fatal("trace mode must keep the Collector root filesystem read-only")
				}
				if collector.SecurityContext.RunAsUser != nil || collector.SecurityContext.RunAsGroup != nil {
					t.Fatal("Collector must inherit the CNPG pod identity for shared file access")
				}
			} else {
				assertEnvValue(t, collector.Env, "GOMEMLIMIT", "107374182")
				for _, env := range gateway.Env {
					if env.Name == "OTEL_TRACES_ENABLED" && !test.native {
						t.Fatal("legacy monitoring must not enable tracing")
					}
				}
			}
			if test.native {
				assertEnvValue(t, gateway.Env, "OTEL_TRACES_ENABLED", "true")
			}
			if len(pod.Spec.Volumes) != 0 || len(pod.Spec.Containers[0].VolumeMounts) != 0 {
				t.Fatal("lifecycle hook mutated its input")
			}
		})
	}
}

func TestNativeTraceExporterInjectionIsIdempotentAndRejectsConflicts(t *testing.T) {
	pod := &corev1.Pod{}
	cfg := &config.Configuration{NativeTraceExporterImage: "native:test", NativeTraceExporterSecret: "reader",
		NativeTraceExporterCluster: "cluster"}
	if err := injectNativeTraceExporter(pod, cfg, "namespace"); err != nil {
		t.Fatal(err)
	}
	first := pod.DeepCopy()
	if err := injectNativeTraceExporter(pod, cfg, "namespace"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pod, first) {
		t.Fatal("repeated injection changed the pod")
	}
	pod.Spec.InitContainers[0].Image = "other:test"
	if err := injectNativeTraceExporter(pod, cfg, "namespace"); err == nil {
		t.Fatal("conflicting container must not be replaced")
	}
}

func TestOtelTraceFilesPreserveExistingStorage(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "postgres"}}}}
	if err := injectOtelTraceFiles(pod, newOtelCollectorSidecar("otel:test", 0)); err != nil {
		t.Fatal(err)
	}
	first := pod.DeepCopy()
	if err := injectOtelTraceFiles(pod, newOtelCollectorSidecar("otel:test", 0)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pod, first) {
		t.Fatal("repeated construction duplicated trace storage")
	}

	conflict := first.DeepCopy()
	conflict.Spec.Volumes[0].EmptyDir.SizeLimit = resource.NewQuantity(1024, resource.BinarySI)
	if err := injectOtelTraceFiles(conflict, newOtelCollectorSidecar("otel:test", 0)); err == nil {
		t.Fatal("conflicting trace volume must be rejected")
	}
	conflict = first.DeepCopy()
	conflict.Spec.Containers[0].VolumeMounts[0].ReadOnly = false
	if err := injectOtelTraceFiles(conflict, newOtelCollectorSidecar("otel:test", 0)); err == nil {
		t.Fatal("writable postgres trace mount must be rejected")
	}
}

func TestGatewayTraceEnvPreservesExplicitSettings(t *testing.T) {
	pod := gatewayContainer(corev1.EnvVar{Name: "OTEL_TRACES_ENABLED", Value: "false"})
	injectGatewayOTelEnv(pod, false, true)
	injectGatewayOTelEnv(pod, false, true)
	assertEnvValue(t, pod.Spec.Containers[1].Env, "OTEL_TRACES_ENABLED", "false")
	assertEnvValue(t, pod.Spec.Containers[1].Env, "OTEL_METRICS_ENABLED", "false")
	if len(pod.Spec.Containers[1].Env) != 3 {
		t.Fatal("gateway telemetry environment contains duplicate entries")
	}
}

func TestLifecycleHookInjectsContainerResourcesAndGoMemLimit(t *testing.T) {
	enabled := true
	cluster := &apiv1.Cluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.SchemeGroupVersion.String(),
			Kind:       apiv1.ClusterKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
		Spec: apiv1.ClusterSpec{
			Plugins: []apiv1.PluginConfiguration{
				{
					Name:    pluginmetadata.PluginName,
					Enabled: &enabled,
					Parameters: map[string]string{
						"gatewayImage":               "gateway:latest",
						"gatewayMemoryRequest":       "768Mi",
						"gatewayMemoryLimit":         "3Gi",
						"gatewayCpuRequest":          "500m",
						"gatewayCpuLimit":            "2",
						"otelCollectorImage":         "otel:latest",
						"otelConfigMapName":          "otel-config",
						"otelMemoryRequest":          "64Mi",
						"otelMemoryLimit":            "128Mi",
						"otelCpuRequest":             "100m",
						"otelCpuLimit":               "300m",
						"documentDbCredentialSecret": "documentdb-credentials",
					},
				},
			},
		},
		Status: apiv1.ClusterStatus{
			TargetPrimary: "cluster-1",
		},
	}
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "cluster-1",
			Namespace:   "default",
			Labels:      map[string]string{"cnpg.io/cluster": "cluster"},
			Annotations: map[string]string{"cnpg.io/operatorVersion": "test"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "postgres"},
			},
		},
	}

	response, err := Implementation{}.LifecycleHook(context.Background(), &cnpglifecycle.OperatorLifecycleRequest{
		OperationType: &cnpglifecycle.OperatorOperationType{
			Type: cnpglifecycle.OperatorOperationType_TYPE_CREATE,
		},
		ClusterDefinition: mustMarshal(t, cluster),
		ObjectDefinition:  mustMarshal(t, pod),
	})
	if err != nil {
		t.Fatalf("LifecycleHook() error: %v", err)
	}

	containers := addedContainersFromPatch(t, response.JsonPatch)
	gateway, ok := containers[gatewayContainerName]
	if !ok {
		t.Fatalf("gateway container missing from patch; patch=%s", string(response.JsonPatch))
	}
	assertResourceQuantity(t, gateway.Resources.Requests, corev1.ResourceCPU, "500m")
	assertResourceQuantity(t, gateway.Resources.Requests, corev1.ResourceMemory, "768Mi")
	assertResourceQuantity(t, gateway.Resources.Limits, corev1.ResourceCPU, "2")
	assertResourceQuantity(t, gateway.Resources.Limits, corev1.ResourceMemory, "3Gi")

	otel, ok := containers["otel-collector"]
	if !ok {
		t.Fatalf("otel container missing from patch; patch=%s", string(response.JsonPatch))
	}
	assertResourceQuantity(t, otel.Resources.Requests, corev1.ResourceCPU, "100m")
	assertResourceQuantity(t, otel.Resources.Requests, corev1.ResourceMemory, "64Mi")
	assertResourceQuantity(t, otel.Resources.Limits, corev1.ResourceMemory, "128Mi")
	assertResourceQuantity(t, otel.Resources.Limits, corev1.ResourceCPU, "300m")
	assertEnvValue(t, otel.Env, "GOMEMLIMIT", "107374182")
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error: %v", err)
	}
	return data
}

func addedContainersFromPatch(t *testing.T, patch []byte) map[string]corev1.Container {
	t.Helper()
	var operations []struct {
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(patch, &operations); err != nil {
		t.Fatalf("json.Unmarshal(patch) error: %v", err)
	}

	containers := map[string]corev1.Container{}
	for _, operation := range operations {
		if operation.Path == "/spec/containers" {
			var added []corev1.Container
			if err := json.Unmarshal(operation.Value, &added); err != nil {
				continue
			}
			for _, container := range added {
				containers[container.Name] = container
			}
			continue
		}
		if !strings.HasPrefix(operation.Path, "/spec/containers/") {
			continue
		}
		var container corev1.Container
		if err := json.Unmarshal(operation.Value, &container); err != nil {
			continue
		}
		if container.Name != "" {
			containers[container.Name] = container
		}
	}
	return containers
}

func assertResourceQuantity(t *testing.T, resources corev1.ResourceList, name corev1.ResourceName, want string) {
	t.Helper()
	got, ok := resources[name]
	if !ok {
		t.Fatalf("resource %s missing, want %s", name, want)
	}
	wantQuantity := resource.MustParse(want)
	if got.Cmp(wantQuantity) != 0 {
		t.Errorf("resource %s = %s, want %s", name, got.String(), want)
	}
}

func assertEnvValue(t *testing.T, env []corev1.EnvVar, name, want string) {
	t.Helper()
	for _, envVar := range env {
		if envVar.Name == name {
			if envVar.Value != want {
				t.Errorf("%s = %q, want %q", name, envVar.Value, want)
			}
			return
		}
	}
	t.Fatalf("%s env var missing", name)
}

// assertPSARestricted verifies a container SecurityContext sets every field
// required by the Kubernetes Pod Security Admission "restricted" profile.
// Pod-level inheritance does not satisfy PSA, so the injected sidecars must
// carry these per-container.
func assertPSARestricted(t *testing.T, name string, sc *corev1.SecurityContext) {
	t.Helper()
	if sc == nil {
		t.Fatalf("%s: SecurityContext is nil", name)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("%s: RunAsNonRoot must be true", name)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("%s: AllowPrivilegeEscalation must be false", name)
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Errorf("%s: Privileged must not be true", name)
	}
	if sc.Capabilities == nil {
		t.Errorf("%s: Capabilities must drop ALL", name)
	} else {
		dropsAll := false
		for _, c := range sc.Capabilities.Drop {
			if c == "ALL" {
				dropsAll = true
				break
			}
		}
		if !dropsAll {
			t.Errorf("%s: Capabilities.Drop must contain ALL, got %v", name, sc.Capabilities.Drop)
		}
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("%s: SeccompProfile.Type must be RuntimeDefault", name)
	}
}

// TestHardenedSecurityContext_PSARestricted guards the helper applied to both
// injected sidecars so PSA "restricted" compliance cannot regress silently.
func TestHardenedSecurityContext_PSARestricted(t *testing.T) {
	assertPSARestricted(t, "hardenedSecurityContext", hardenedSecurityContext())
}

// TestHardenedSecurityContext_NoForcedUID asserts the shared helper does not
// pin a UID/GID, so third-party images (e.g. otel-collector) keep their own
// baked-in non-root user.
func TestHardenedSecurityContext_NoForcedUID(t *testing.T) {
	sc := hardenedSecurityContext()
	if sc.RunAsUser != nil {
		t.Errorf("shared hardened context must not pin RunAsUser, got %d", *sc.RunAsUser)
	}
	if sc.RunAsGroup != nil {
		t.Errorf("shared hardened context must not pin RunAsGroup, got %d", *sc.RunAsGroup)
	}
}

// TestGatewaySecurityContext_PSARestrictedAsUID1000 asserts the gateway sidecar
// is PSA restricted and pinned to the non-root UID/GID 1000 its image expects.
func TestGatewaySecurityContext_PSARestrictedAsUID1000(t *testing.T) {
	sc := gatewaySecurityContext()
	assertPSARestricted(t, "gatewaySecurityContext", sc)
	if sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
		t.Errorf("gateway must run as UID 1000, got %v", sc.RunAsUser)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != 1000 {
		t.Errorf("gateway must run as GID 1000, got %v", sc.RunAsGroup)
	}
}

// TestNewOtelCollectorSidecar_Hardened is the injection-layer guard for the
// otel-collector sidecar (which the e2e suite does not exercise unless
// monitoring is enabled). It asserts the constructed container is PSA
// restricted and, unlike the gateway, does not force a UID so the collector
// image keeps its own non-root user (UID 10001).
func TestNewOtelCollectorSidecar_Hardened(t *testing.T) {
	c := newOtelCollectorSidecar("otel/opentelemetry-collector-contrib:test", 0)

	if c.Name != otelCollectorContainerName {
		t.Fatalf("container name = %q, want %q", c.Name, otelCollectorContainerName)
	}
	assertPSARestricted(t, "otel-collector", c.SecurityContext)
	if c.SecurityContext.RunAsUser != nil {
		t.Errorf("otel-collector must not force a UID (keeps image's 10001), got %d", *c.SecurityContext.RunAsUser)
	}
	if c.SecurityContext.RunAsGroup != nil {
		t.Errorf("otel-collector must not force a GID, got %d", *c.SecurityContext.RunAsGroup)
	}
}

// TestNewOtelCollectorSidecar_MonitorUser asserts the sidecar connects using
// the dedicated monitoring identity without injecting an unused password.
func TestNewOtelCollectorSidecar_MonitorUser(t *testing.T) {
	c := newOtelCollectorSidecar("otel/opentelemetry-collector-contrib:test", 0)

	for _, env := range c.Env {
		if env.Name == "PGPASSWORD" {
			t.Fatal("PGPASSWORD must not be injected while PostgreSQL uses trust authentication")
		}
		if env.Name == "PGUSER" {
			if env.Value != otelMonitorRoleName {
				t.Fatalf("PGUSER = %q, want %q", env.Value, otelMonitorRoleName)
			}
			return
		}
	}
	t.Fatal("missing PGUSER env var")
}
