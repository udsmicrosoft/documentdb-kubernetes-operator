// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package lifecycle implements the lifecycle hooks
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"

	"github.com/cloudnative-pg/cnpg-i-machinery/pkg/pluginhelper/common"
	"github.com/cloudnative-pg/cnpg-i-machinery/pkg/pluginhelper/decoder"
	"github.com/cloudnative-pg/cnpg-i-machinery/pkg/pluginhelper/object"
	"github.com/cloudnative-pg/cnpg-i/pkg/lifecycle"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"

	"github.com/documentdb/cnpg-i-sidecar-injector/internal/config"
	"github.com/documentdb/cnpg-i-sidecar-injector/internal/utils"
	"github.com/documentdb/cnpg-i-sidecar-injector/pkg/metadata"
)

// Implementation is the implementation of the lifecycle handler
type Implementation struct {
	lifecycle.UnimplementedOperatorLifecycleServer
}

// GetCapabilities exposes the lifecycle capabilities
func (impl Implementation) GetCapabilities(
	_ context.Context,
	_ *lifecycle.OperatorLifecycleCapabilitiesRequest,
) (*lifecycle.OperatorLifecycleCapabilitiesResponse, error) {
	return &lifecycle.OperatorLifecycleCapabilitiesResponse{
		LifecycleCapabilities: []*lifecycle.OperatorLifecycleCapabilities{
			{
				Group: "",
				Kind:  "Pod",
				OperationTypes: []*lifecycle.OperatorOperationType{
					{
						Type: lifecycle.OperatorOperationType_TYPE_CREATE,
					},
				},
			},
		},
	}, nil
}

// LifecycleHook is called by CNPG for Pod CREATE operations.
func (impl Implementation) LifecycleHook(
	ctx context.Context,
	request *lifecycle.OperatorLifecycleRequest,
) (*lifecycle.OperatorLifecycleResponse, error) {
	kind, err := utils.GetKind(request.GetObjectDefinition())
	if err != nil {
		return nil, err
	}
	operation := request.GetOperationType().GetType().Enum()
	if operation == nil {
		return nil, errors.New("no operation set")
	}

	//nolint: gocritic
	switch kind {
	case "Pod":
		switch *operation {
		case lifecycle.OperatorOperationType_TYPE_CREATE:
			return impl.reconcileMetadata(ctx, request)
		}
		// add any other custom logic to execute based on the operation
	}

	return &lifecycle.OperatorLifecycleResponse{}, nil
}

// reconcileMetadata mutates Pod metadata, injects sidecars, and applies labels/annotations.
func (impl Implementation) reconcileMetadata(
	ctx context.Context,
	request *lifecycle.OperatorLifecycleRequest,
) (*lifecycle.OperatorLifecycleResponse, error) {
	cluster, err := decoder.DecodeClusterLenient(request.GetClusterDefinition())
	if err != nil {
		return nil, err
	}

	// Initialize standard logger for debugging
	log.SetPrefix("[DocumentDB Sidecar Injector] ")

	// Debug: Log the full cluster definition to see what plugins are configured
	log.Printf("Cluster plugins configuration: %+v", cluster.Spec.Plugins)

	helper := common.NewPlugin(
		*cluster,
		metadata.PluginName,
	)

	// Debug logging for plugin parameters and metadata
	log.Printf("Plugin name being used: %s", metadata.PluginName)
	log.Printf("Plugin parameters received: %v, cluster: %s/%s",
		helper.Parameters, cluster.Namespace, cluster.Name)

	// Debug: Check if our plugin is found in the cluster's plugin list
	for i, plugin := range cluster.Spec.Plugins {
		log.Printf("Plugin[%d]: Name=%s, Enabled=%t, Parameters=%v",
			i, plugin.Name, *plugin.Enabled, plugin.Parameters)
	}

	configuration, valErrs := config.FromParameters(helper)
	if len(valErrs) > 0 {
		return nil, valErrs[0]
	}

	// Log the gateway image being used
	gatewayImageParam := helper.Parameters["gatewayImage"]
	if gatewayImageParam == "" {
		log.Printf("Using default gateway image: %s (no gatewayImage parameter provided)", configuration.GatewayImage)
	} else {
		log.Printf("Using configured gateway image: %s (parameter value: %s)", configuration.GatewayImage, gatewayImageParam)
	}

	pod, err := decoder.DecodePodJSON(request.GetObjectDefinition())
	if err != nil {
		return nil, err
	}

	mutatedPod := pod.DeepCopy()

	// Initialize environment variables for the gateway container
	envVars := []corev1.EnvVar{}

	// Add USERNAME and PASSWORD environment variables from secret defined in configuration
	credentialSecretName := configuration.DocumentDbCredentialSecret
	log.Printf("Adding USERNAME and PASSWORD environment variables from secret '%s'", credentialSecretName)
	envVars = append(envVars,
		corev1.EnvVar{
			Name: "USERNAME",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: credentialSecretName,
					},
					Key: "username",
				},
			},
		},
		corev1.EnvVar{
			Name: "PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: credentialSecretName,
					},
					Key: "password",
				},
			},
		},
	)

	// Initialize the sidecar container with configurable gateway image
	sidecar := &corev1.Container{
		Name:            "documentdb-gateway",
		Image:           configuration.GatewayImage,
		ImagePullPolicy: configuration.GatewayImagePullPolicy,
		Ports: []corev1.ContainerPort{
			{
				ContainerPort: 10260,
			},
		},
		Env:             envVars,
		SecurityContext: gatewaySecurityContext(),
	}
	if resources := buildResources(
		configuration.GatewayCPURequest,
		configuration.GatewayCPULimit,
		configuration.GatewayMemoryRequest,
		configuration.GatewayMemoryLimit,
	); hasResourceRequirements(resources) {
		sidecar.Resources = resources
	}

	// If TLS secret parameter provided, mount it at /tls
	// Track whether TLS secret is configured to augment container args later
	hasTLSSecret := false
	if tlsSecret, ok := helper.Parameters["gatewayTLSSecret"]; ok && tlsSecret != "" {
		// Append volume only if not already present
		found := false
		for _, v := range mutatedPod.Spec.Volumes {
			if v.Name == "gateway-tls" {
				found = true
				break
			}
		}
		if !found {
			mutatedPod.Spec.Volumes = append(mutatedPod.Spec.Volumes, corev1.Volume{
				Name: "gateway-tls",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: tlsSecret},
				},
			})
		}
		// Add mount to sidecar container
		sidecar.VolumeMounts = append(sidecar.VolumeMounts, corev1.VolumeMount{Name: "gateway-tls", MountPath: "/tls", ReadOnly: true})
		// Provide env vars for gateway to load the mounted certificate and key
		// Most gateway images respect CERT_PATH and KEY_FILE; keep TLS_CERT_DIR for backward-compat
		sidecar.Env = append(sidecar.Env,
			corev1.EnvVar{Name: "TLS_CERT_DIR", Value: "/tls"},
			corev1.EnvVar{Name: "CERT_PATH", Value: "/tls/tls.crt"},
			corev1.EnvVar{Name: "KEY_FILE", Value: "/tls/tls.key"},
		)
		// Mark that TLS secret is present so we can also pass explicit CLI args
		hasTLSSecret = true
		log.Printf("Injected TLS secret volume for gateway: %s", tlsSecret)
	}

	// Build base args and append TLS file args if a TLS secret is configured
	args := []string{"--start-pg", "false", "--pg-port", "5432"}
	// Check if the pod has the label replication_cluster_type=replica

	// Check if the pod has the label replication_cluster_type=replica or is not a local primary
	if mutatedPod.Labels["replication_cluster_type"] == "replica" || cluster.Status.TargetPrimary != mutatedPod.Name {
		args = append([]string{"--create-user", "false"}, args...)
	} else {
		args = append([]string{"--create-user", "true"}, args...)
	}
	if hasTLSSecret {
		// Pass cert and key via CLI args to align with emulator_entrypoint.sh interface
		args = append(args, "--cert-path", "/tls/tls.crt", "--key-file", "/tls/tls.key")
	}
	sidecar.Args = args

	// Inject the sidecar container
	err = object.InjectPluginSidecar(mutatedPod, sidecar, false)
	if err != nil {
		return nil, err
	}

	// Inject OTel Collector sidecar when monitoring is enabled.
	// The sidecar is only injected when the operator passes otelCollectorImage
	// and otelConfigMapName parameters (i.e., monitoring.enabled is true).
	if configuration.OtelCollectorImage != "" && configuration.OtelConfigMapName != "" {
		log.Printf("Injecting OTel Collector sidecar with image: %s", configuration.OtelCollectorImage)

		// Add ConfigMap volume for operator-generated config files (static.yaml + dynamic.yaml)
		// Check for an existing volume so pod construction remains idempotent.
		otelVolFound := false
		for _, v := range mutatedPod.Spec.Volumes {
			if v.Name == "otel-config" {
				otelVolFound = true
				break
			}
		}
		if !otelVolFound {
			mutatedPod.Spec.Volumes = append(mutatedPod.Spec.Volumes, corev1.Volume{
				Name: "otel-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: configuration.OtelConfigMapName,
						},
					},
				},
			})
		}

		otelSidecar := newOtelCollectorSidecar(configuration.OtelCollectorImage, configuration.PrometheusPort)
		if resources := buildResources(
			configuration.OTelCPURequest,
			configuration.OTelCPULimit,
			configuration.OTelMemoryRequest,
			configuration.OTelMemoryLimit,
		); hasResourceRequirements(resources) {
			otelSidecar.Resources = resources
		}
		if goMemLimitEnv, ok := buildGoMemLimitEnv(configuration.OTelMemoryLimit, configuration.OTelTraceFile); ok {
			otelSidecar.Env = append(otelSidecar.Env, goMemLimitEnv)
		}
		if configuration.OTelTraceFile {
			if err := injectOtelTraceFiles(mutatedPod, otelSidecar); err != nil {
				return nil, err
			}
		}

		// Expose Prometheus metrics port when configured
		if configuration.PrometheusPort > 0 {
			otelSidecar.Ports = append(otelSidecar.Ports, corev1.ContainerPort{
				Name:          "prom-metrics",
				ContainerPort: configuration.PrometheusPort,
				Protocol:      corev1.ProtocolTCP,
			})
			// Add Prometheus scrape annotations for auto-discovery
			if mutatedPod.Annotations == nil {
				mutatedPod.Annotations = map[string]string{}
			}
			mutatedPod.Annotations["prometheus.io/scrape"] = "true"
			mutatedPod.Annotations["prometheus.io/port"] = fmt.Sprintf("%d", configuration.PrometheusPort)
			mutatedPod.Annotations["prometheus.io/path"] = "/metrics"

		}

		err = object.InjectPluginSidecar(mutatedPod, otelSidecar, false)
		if err != nil {
			return nil, err
		}

		metricsEnabled := configuration.OTelMetricsEnabled == nil || *configuration.OTelMetricsEnabled
		injectGatewayOTelEnv(mutatedPod, metricsEnabled,
			configuration.OTelTraceFile || configuration.NativeTraceExporterImage != "")

		if configuration.NativeTraceExporterImage != "" {
			if err := injectNativeTraceExporter(mutatedPod, configuration, cluster.Namespace); err != nil {
				return nil, err
			}
		}

		log.Printf("OTel Collector sidecar injected successfully")
	}

	for key, value := range configuration.Labels {
		mutatedPod.Labels[key] = value
	}
	for key, value := range configuration.Annotations {
		mutatedPod.Annotations[key] = value
	}

	patch, err := object.CreatePatch(mutatedPod, pod)
	if err != nil {
		return nil, err
	}

	log.Printf("Generated patch: %s", string(patch))

	return &lifecycle.OperatorLifecycleResponse{
		JsonPatch: patch,
	}, nil
}

// gatewayContainerName is the name of the documentdb gateway container that
// pushes OTLP metrics and traces to the co-located OTel Collector sidecar.
// Kept as a package-level constant so tests can reference it.
const gatewayContainerName = "documentdb-gateway"

// Keep this mount path aligned with the operator's file/traces exporter.
const otelTraceMountPath = "/var/lib/otel/traces"

func injectNativeTraceExporter(pod *corev1.Pod, cfg *config.Configuration, namespace string) error {
	const name = "native-trace-exporter"
	restart := corev1.ContainerRestartPolicyAlways
	exporter := corev1.Container{
		Name:            name,
		Image:           cfg.NativeTraceExporterImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"serve-native", namespace, cfg.NativeTraceExporterCluster},
		RestartPolicy:   &restart,
		SecurityContext: hardenedSecurityContext(),
		Resources: buildResources(cfg.NativeTraceExporterCPURequest, cfg.NativeTraceExporterCPULimit,
			cfg.NativeTraceExporterMemoryRequest, cfg.NativeTraceExporterMemoryLimit),
		Env: []corev1.EnvVar{
			{
				Name: "DOCUMENTDB_TELEMETRY_PG_URI",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cfg.NativeTraceExporterSecret},
					Key:                  "uri",
				}},
			},
			{Name: "DOCUMENTDB_TELEMETRY_OTLP_ENDPOINT", Value: "http://127.0.0.1:4317"},
		},
	}
	exporter.SecurityContext.ReadOnlyRootFilesystem = pointer.Bool(true)
	for _, container := range pod.Spec.Containers {
		if container.Name == name {
			return fmt.Errorf("container %q conflicts with the native trace exporter", name)
		}
	}
	for _, container := range pod.Spec.InitContainers {
		if container.Name == name {
			if !apiequality.Semantic.DeepEqual(container, exporter) {
				return fmt.Errorf("init container %q conflicts with the native trace exporter", name)
			}
			return nil
		}
	}
	// Start the process without waiting for PostgreSQL or export delivery.
	// Its native-sidecar lifecycle and absent readiness probe keep outages
	// from gating database readiness.
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, exporter)
	return nil
}

func injectOtelTraceFiles(pod *corev1.Pod, collector *corev1.Container) error {
	traceVolume := corev1.Volume{
		Name: "otel-traces",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: resource.NewQuantity(64*1024*1024, resource.BinarySI),
			},
		},
	}
	found := false
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == traceVolume.Name {
			if !apiequality.Semantic.DeepEqual(volume, traceVolume) {
				return fmt.Errorf("volume %q conflicts with Collector trace storage", traceVolume.Name)
			}
			found = true
			break
		}
	}
	if !found {
		pod.Spec.Volumes = append(pod.Spec.Volumes, traceVolume)
	}

	mount := corev1.VolumeMount{Name: traceVolume.Name, MountPath: otelTraceMountPath}
	collector.VolumeMounts = append(collector.VolumeMounts, mount)
	collector.SecurityContext.ReadOnlyRootFilesystem = pointer.Bool(true)
	if collector.Resources.Requests == nil {
		collector.Resources.Requests = corev1.ResourceList{}
	}
	// A container storage limit also contributes to the pod-wide eviction
	// limit, which would unexpectedly cap database and gateway logs.
	collector.Resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("64Mi")

	// CNPG supplies the shared pod identity. Its postgres container has the
	// file-reading tools that the scratch-based Collector image lacks.
	mount.ReadOnly = true
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		if container.Name != "postgres" {
			continue
		}
		found = false
		for _, existing := range container.VolumeMounts {
			if existing.Name == mount.Name || existing.MountPath == mount.MountPath {
				if !apiequality.Semantic.DeepEqual(existing, mount) {
					return fmt.Errorf("postgres mount %q conflicts with Collector trace storage", mount.MountPath)
				}
				found = true
			}
		}
		if !found {
			container.VolumeMounts = append(container.VolumeMounts, mount)
		}
	}
	return nil
}

func buildResources(cpuReq, cpuLim, memReq, memLim string) corev1.ResourceRequirements {
	var requests corev1.ResourceList
	var limits corev1.ResourceList

	addQuantity := func(list corev1.ResourceList, name corev1.ResourceName, value string) corev1.ResourceList {
		if value == "" {
			return list
		}
		quantity, err := resource.ParseQuantity(value)
		if err != nil {
			return list
		}
		if list == nil {
			list = corev1.ResourceList{}
		}
		list[name] = quantity
		return list
	}

	requests = addQuantity(requests, corev1.ResourceCPU, cpuReq)
	requests = addQuantity(requests, corev1.ResourceMemory, memReq)
	limits = addQuantity(limits, corev1.ResourceCPU, cpuLim)
	limits = addQuantity(limits, corev1.ResourceMemory, memLim)

	return corev1.ResourceRequirements{
		Requests: requests,
		Limits:   limits,
	}
}

func hasResourceRequirements(resources corev1.ResourceRequirements) bool {
	return len(resources.Requests) > 0 || len(resources.Limits) > 0
}

func buildGoMemLimitEnv(memoryLimit string, traceFiles bool) (corev1.EnvVar, bool) {
	if memoryLimit == "" {
		return corev1.EnvVar{}, false
	}

	quantity, err := resource.ParseQuantity(memoryLimit)
	if err != nil {
		return corev1.EnvVar{}, false
	}

	percentage := int64(80)
	if traceFiles {
		// Keep JSON marshaling below the memory_limiter's 55% soft threshold
		// so GC reclaims batches before the receiver starts refusing data.
		percentage = 50
	}
	return corev1.EnvVar{
		Name:  "GOMEMLIMIT",
		Value: strconv.FormatInt(quantity.Value()*percentage/100, 10),
	}, true
}

// hardenedSecurityContext returns a container SecurityContext that satisfies
// the Kubernetes Pod Security Admission (PSA) "restricted" profile. It is
// applied to every sidecar this plugin injects (documentdb-gateway and
// otel-collector) so that DocumentDB cluster pods are admitted on clusters
// that enforce `pod-security.kubernetes.io/enforce=restricted` (e.g. GKE
// Autopilot, OpenShift, AKS with the Azure Policy security baseline).
//
// PSA requires these fields to be set per-container; pod-level inheritance
// does not satisfy the checks. This mirrors how CloudNativePG hardens its own
// built-in containers (see pkg/specs GetSecurityContext) and how the CNPG
// barman-cloud plugin hardens its injected sidecar.
//
// It deliberately does NOT pin RunAsUser/RunAsGroup: PSA "restricted" only
// requires RunAsNonRoot=true, not a specific UID. Each caller adds an explicit
// UID only where the image needs one (the gateway runs as 1000). Otherwise
// the container inherits CNPG's pod identity, or falls back to its image user.
//
// The default does not require a read-only root filesystem. Trace file mode
// enables it for the Collector and supplies a writable emptyDir mount.
func hardenedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             pointer.Bool(true),
		Privileged:               pointer.Bool(false),
		AllowPrivilegeEscalation: pointer.Bool(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// gatewayOTelEnvVars returns the OTel-related env vars that the sidecar
// injector adds to the gateway container so it can push metrics to the
// co-located OTel Collector sidecar.
//
// Per-pod attribution (k8s.pod.name) is added by the collector's resource
// processor on every exported metric, so we don't need to set
// OTEL_RESOURCE_ATTRIBUTES / service.instance.id here.
func gatewayOTelEnvVars(metricsEnabled, tracesEnabled bool) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{
			Name:  "OTEL_EXPORTER_OTLP_ENDPOINT",
			Value: "http://127.0.0.1:4317",
		},
		{
			// Required to enable the gateway's OTLP metrics exporter; the
			// pgmongo gateway gates OTel init off by default and checks this
			// env var (or a JSON TelemetryOptions block) at startup.
			Name:  "OTEL_METRICS_ENABLED",
			Value: strconv.FormatBool(metricsEnabled),
		},
	}
	if tracesEnabled {
		env = append(env, corev1.EnvVar{Name: "OTEL_TRACES_ENABLED", Value: "true"})
	}
	return env
}

// injectGatewayOTelEnv mutates `pod` to append OTel env vars to the gateway
// container, idempotently. Existing env vars with the same name are preserved
// (we don't overwrite) and missing ones are appended in declaration order.
//
// Idempotency keeps pod construction stable if a caller passes an already
// mutated pod. Container injection itself runs only for CREATE because
// Kubernetes forbids adding or removing containers from an existing pod.
func injectGatewayOTelEnv(pod *corev1.Pod, metricsEnabled, tracesEnabled bool) {
	envs := gatewayOTelEnvVars(metricsEnabled, tracesEnabled)
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name != gatewayContainerName {
			continue
		}
		existing := make(map[string]bool, len(pod.Spec.Containers[i].Env))
		for _, e := range pod.Spec.Containers[i].Env {
			existing[e.Name] = true
		}
		for _, e := range envs {
			if !existing[e.Name] {
				pod.Spec.Containers[i].Env = append(pod.Spec.Containers[i].Env, e)
			}
		}
		return
	}
}

// otelCollectorContainerName is the name of the injected OpenTelemetry
// Collector sidecar.
const otelCollectorContainerName = "otel-collector"
const otelMonitorRoleName = "otel_monitor"

// gatewaySecurityContext returns the SecurityContext for the documentdb-gateway
// sidecar: the shared PSA-restricted hardening plus an explicit UID/GID of
// 1000, the non-root user the gateway image is built to run as.
func gatewaySecurityContext() *corev1.SecurityContext {
	sc := hardenedSecurityContext()
	sc.RunAsUser = pointer.Int64(1000)
	sc.RunAsGroup = pointer.Int64(1000)
	return sc
}

// newOtelCollectorSidecar builds the base OpenTelemetry Collector sidecar
// container with readiness independent of exporters. Prometheus ports and
// scrape annotations are added by the caller.
// It carries the shared PSA-restricted SecurityContext without an
// explicit UID so it inherits CNPG's pod identity. Without a pod UID it uses
// the image's non-root user.
func newOtelCollectorSidecar(image string, prometheusPort int32) *corev1.Container {
	// Keep the default aligned with operator/src/internal/otel/base_config.yaml.
	healthPort := int32(13133)
	if healthPort == prometheusPort {
		healthPort++
	}

	return &corev1.Container{
		Name:  otelCollectorContainerName,
		Image: image,
		Args: []string{
			"--config=file:/config/static.yaml",
			"--config=file:/config/dynamic.yaml",
		},
		Ports: []corev1.ContainerPort{{
			Name:          "otel-health",
			ContainerPort: healthPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromString("otel-health"),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		// PostgreSQL currently uses trust authentication, so injecting a password
		// would not enforce access control. Use the dedicated password-disabled
		// identity directly until database authentication is tightened.
		Env: []corev1.EnvVar{
			{
				Name: "POD_NAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.name",
					},
				},
			},
			{
				Name:  "PGUSER",
				Value: otelMonitorRoleName,
			},
			{
				Name:  "OTEL_HEALTH_CHECK_PORT",
				Value: strconv.FormatInt(int64(healthPort), 10),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "otel-config",
				MountPath: "/config",
				ReadOnly:  true,
			},
		},
		SecurityContext: hardenedSecurityContext(),
	}
}
