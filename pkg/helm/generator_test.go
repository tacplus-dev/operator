package helm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protoKmaker "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/kmaker"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
)

func TestGenerateKernelProjectedTokensAndTokenReviewRBAC(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
		},
		Kmaker: &protoKmaker.Config{TokenAudience: "custom-kmaker"},
	})

	statefulSet := chartFile(t, charts, "templates/kernel/statefulset.yaml")
	for _, want := range []string{
		"serviceAccountName: kernel",
		"automountServiceAccountToken: false",
		"- name: KMAKER_K8S_TOKEN_FILE\n          value: /var/run/secrets/schematic/kmaker/token",
		"- name: KERNEL_K8S_TOKEN_AUDIENCE\n          value: schematic-kernel",
		"- name: kmaker-token\n          mountPath: /var/run/secrets/schematic/kmaker\n          readOnly: true",
		"- name: kmaker-token\n        projected:",
		"defaultMode: 0444",
		"path: token",
		"audience: custom-kmaker",
		"expirationSeconds: 600",
		"- name: kubernetes-api-token\n          mountPath: /var/run/secrets/kubernetes.io/serviceaccount\n          readOnly: true",
		"- name: kubernetes-api-token\n        projected:",
		"defaultMode: 0444",
		"expirationSeconds: 3600",
		"name: kube-root-ca.crt",
		"path: ca.crt",
		"- downwardAPI:",
		"path: namespace",
		"fieldPath: metadata.namespace",
	} {
		if !strings.Contains(statefulSet, want) {
			t.Fatalf("kernel StatefulSet missing %q\n--- statefulset ---\n%s", want, statefulSet)
		}
	}

	tokenReviewRBAC := chartFile(t, charts, "templates/kernel/tokenreview-rbac.yaml")
	for _, want := range []string{
		"resources: [\"tokenreviews\"]",
		"verbs: [\"create\"]",
		"name: kernel",
	} {
		if !strings.Contains(tokenReviewRBAC, want) {
			t.Fatalf("kernel TokenReview RBAC missing %q\n--- rbac ---\n%s", want, tokenReviewRBAC)
		}
	}
}

func TestGenerateKmakerProjectedKubernetesAPITokenAndRBAC(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KmakerNodes: 1,
		},
	})

	deployment := chartFile(t, charts, "templates/kmaker/deployment.yaml")
	for _, want := range []string{
		"serviceAccountName: kmaker",
		"automountServiceAccountToken: false",
		"- name: kubernetes-api-token\n          mountPath: /var/run/secrets/kubernetes.io/serviceaccount\n          readOnly: true",
		"- name: kubernetes-api-token\n        projected:",
		"defaultMode: 0444",
		"path: token",
		"expirationSeconds: 3600",
		"name: kube-root-ca.crt",
		"path: ca.crt",
		"- downwardAPI:",
		"path: namespace",
		"fieldPath: metadata.namespace",
	} {
		if !strings.Contains(deployment, want) {
			t.Fatalf("kmaker Deployment missing %q\n--- deployment ---\n%s", want, deployment)
		}
	}

	tokenReviewRBAC := chartFile(t, charts, "templates/kmaker/tokenreview-rbac.yaml")
	for _, want := range []string{
		"resources: [\"tokenreviews\"]",
		"verbs: [\"create\"]",
		"name: kmaker",
	} {
		if !strings.Contains(tokenReviewRBAC, want) {
			t.Fatalf("kmaker TokenReview RBAC missing %q\n--- rbac ---\n%s", want, tokenReviewRBAC)
		}
	}

	podRBAC := chartFile(t, charts, "templates/kmaker/pod-rbac.yaml")
	for _, want := range []string{
		"resources: [\"pods\"]",
		"verbs: [\"get\"]",
		"name: kmaker",
	} {
		if !strings.Contains(podRBAC, want) {
			t.Fatalf("kmaker pod RBAC missing %q\n--- rbac ---\n%s", want, podRBAC)
		}
	}
}

func TestGeneratedServiceWorkloadsUseProjectedKernelToken(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Tls:       &protoOperator.TLSConfig{Letsencrypt: true},
		Topology: &protoOperator.Topology{
			SentinelNodes:   1,
			EventbusNodes:   1,
			GatewayNodes:    1,
			OauthNodes:      1,
			EchoNodes:       1,
			OpaNodes:        1,
			StratumNodes:    1,
			SamlNodes:       1,
		},
		OptionalServices: &protoOperator.OptionalServices{
			Saml:  true,
			Oauth: true,
			Echo:  true,
		},
	})

	services := []string{
		"sentinel",
		"eventbus",
		"gateway",
		"opa",
		"stratum",
		"oauth",
		"saml",
		"letsencrypt",
		"echo",
	}
	for _, service := range services {
		t.Run(service, func(t *testing.T) {
			// stratum is a StatefulSet (its Hazelcast members need stable
			// per-pod DNS), so its workload file is named differently. The
			// projected-token assertions below apply to either shape.
			path := "templates/" + service + "/deployment.yaml"
			if _, ok := charts.Files[path]; !ok {
				path = "templates/" + service + "/statefulset.yaml"
			}
			deployment := chartFile(t, charts, path)
			for _, want := range []string{
				"serviceAccountName: " + service,
				"automountServiceAccountToken: false",
				"- name: KERNEL_K8S_TOKEN_FILE\n          value: /var/run/secrets/schematic/kernel/token",
				"- name: kernel-token\n          mountPath: /var/run/secrets/schematic/kernel\n          readOnly: true",
				"- name: kernel-token\n        projected:",
				"defaultMode: 0444",
				"path: token",
				"audience: schematic-kernel",
				"expirationSeconds: 3600",
			} {
				if !strings.Contains(deployment, want) {
					t.Fatalf("%s Deployment missing %q\n--- deployment ---\n%s", service, want, deployment)
				}
			}
		})
	}
	for _, service := range services {
		rbac := chartFile(t, charts, "templates/"+service+"/serviceaccount.yaml")
		if !strings.Contains(rbac, "name: "+service) {
			t.Fatalf("%s ServiceAccount missing service name\n--- serviceaccount ---\n%s", service, rbac)
		}
	}
}

func TestOperatorDeploymentUsesExplicitKubernetesAPITokenProjection(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "helm", "templates", "deployment.yaml"))
	if err != nil {
		t.Fatalf("read operator Deployment template: %v", err)
	}
	deployment := string(data)
	for _, want := range []string{
		"serviceAccountName: schematic-operator",
		"automountServiceAccountToken: false",
		"- name: kubernetes-api-token\n          mountPath: /var/run/secrets/kubernetes.io/serviceaccount\n          readOnly: true",
		"- name: kubernetes-api-token\n        projected:",
		"defaultMode: 0444",
		"path: token",
		"expirationSeconds: 3600",
		"name: kube-root-ca.crt",
		"path: ca.crt",
		"- downwardAPI:",
		"path: namespace",
		"fieldPath: metadata.namespace",
	} {
		if !strings.Contains(deployment, want) {
			t.Fatalf("operator Deployment missing %q\n--- deployment ---\n%s", want, deployment)
		}
	}
}

// stripYAMLComments drops comment lines so a "must NOT grant" assertion cannot
// be satisfied by a resource name that only appears in prose.
func stripYAMLComments(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func TestOperatorChartRBACCanManageGeneratedServiceRBAC(t *testing.T) {
	readTemplate := func(name string) string {
		data, err := os.ReadFile(filepath.Join("..", "..", "helm", "templates", name))
		if err != nil {
			t.Fatalf("read operator %s template: %v", name, err)
		}
		return stripYAMLComments(string(data))
	}
	clusterRole := readTemplate("clusterrole.yaml")
	role := readTemplate("role.yaml")

	// Cluster-scoped objects the operator genuinely has to reach: the
	// <service>-tokenreview ClusterRole/ClusterRoleBinding pair the generated
	// service chart emits, plus the TokenReview permission RBAC escalation
	// prevention requires it to hold before it can mint that ClusterRole.
	for _, want := range []string{
		"- clusterroles",
		"- clusterrolebindings",
		"- tokenreviews",
		`resources: ["nodes"]`,
		`resources: ["namespaces"]`,
	} {
		if !strings.Contains(clusterRole, want) {
			t.Fatalf("operator ClusterRole missing %q\n--- clusterrole ---\n%s", want, clusterRole)
		}
	}

	// Everything else is namespaced, including the <service>-pod-reader Role the
	// generated chart emits and the pods/get the operator must hold to mint it.
	for _, want := range []string{
		"- roles",
		"- rolebindings",
		"- pods",
		"- deployments",
		"- statefulsets",
		"- services",
		"- serviceaccounts",
		"- configmaps",
		"- secrets",
		"- persistentvolumeclaims",
		`resources: ["leases"]`,
		`resources: ["events"]`,
		`verbs: ["get"]`,
	} {
		if !strings.Contains(role, want) {
			t.Fatalf("operator Role missing %q\n--- role ---\n%s", want, role)
		}
	}

	// The point of the split: none of the namespaced grants may leak back into
	// the ClusterRole, where they would apply to every namespace in the cluster.
	for _, forbidden := range []string{
		"- roles",
		"- rolebindings",
		"- pods",
		"- deployments",
		"- statefulsets",
		"- services",
		"- serviceaccounts",
		"- configmaps",
		"- secrets",
		"- persistentvolumeclaims",
		`resources: ["leases"]`,
		`resources: ["events"]`,
	} {
		if strings.Contains(clusterRole, forbidden) {
			t.Fatalf("operator ClusterRole grants %q cluster-wide; it belongs in the namespaced Role\n--- clusterrole ---\n%s", forbidden, clusterRole)
		}
	}

	// The Role must be bound in every managed namespace, with the subject
	// pinned to the namespace the operator ServiceAccount actually lives in.
	for _, want := range []string{
		".Values.operator.managedNamespaces",
		"kind: RoleBinding",
		"namespace: {{ $ns }}",
		"namespace: {{ $.Values.operator.namespace }}",
	} {
		if !strings.Contains(role, want) {
			t.Fatalf("operator Role template missing %q\n--- role ---\n%s", want, role)
		}
	}
}

// TestSingleNodeKernelValuesAndTemplates verifies that a 1-node kernel topology
// produces a values.yaml with a kernel0 placeholder and templates that reference
// .Values.kernel0.config — the combination that previously caused a nil-pointer
// error during helm install dry-run.
func TestSingleNodeKernelValuesAndTemplates(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
		},
	})

	// values.yaml must contain the per-replica key kernel0 (and no kernel1).
	values := chartFile(t, charts, "values.yaml")
	if !strings.Contains(values, "kernel0:") {
		t.Fatalf("values.yaml missing kernel0 placeholder\n--- values.yaml ---\n%s", values)
	}
	if strings.Contains(values, "kernel1:") {
		t.Fatalf("values.yaml unexpectedly contains kernel1 for a 1-node topology\n--- values.yaml ---\n%s", values)
	}

	// secret-0.yaml (kernel's config holds key material) must reference
	// .Values.kernel0.config, not .Values.kernel.config.
	cm := chartFile(t, charts, "templates/kernel/secret-0.yaml")
	if !strings.Contains(cm, ".Values.kernel0.config") {
		t.Fatalf("kernel secret-0 must reference .Values.kernel0.config\n--- secret ---\n%s", cm)
	}

	// statefulset.yaml must checksum .Values.kernel0.config in its annotations.
	ss := chartFile(t, charts, "templates/kernel/statefulset.yaml")
	if !strings.Contains(ss, ".Values.kernel0.config") {
		t.Fatalf("kernel statefulset must reference .Values.kernel0.config\n--- statefulset ---\n%s", ss)
	}
}

// TestStatefulServiceWithoutPerReplicaConfigUsesTheSharedKey pins the rule that
// per-replica config keys belong to kernel (distinct TLS cert per replica), not
// to StatefulSets in general.
//
// stratum is a multi-replica StatefulSet whose replicas all read ONE config, and
// the bundle supplies exactly one entry under "stratum". When the key was chosen
// by IsStatefulSet, its configmaps referenced .Values.stratum0.config — which
// nothing populates — and all three pods died at startup with
// "data is null or empty".
func TestStatefulServiceWithoutPerReplicaConfigUsesTheSharedKey(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			StratumNodes: 3,
		},
	})

	ss := chartFile(t, charts, "templates/stratum/statefulset.yaml")
	if !strings.Contains(ss, "replicas: 3") {
		t.Fatalf("guard: expected a 3-replica stratum StatefulSet\n--- statefulset ---\n%s", ss)
	}

	for i := range 3 {
		cm := chartFile(t, charts, fmt.Sprintf("templates/stratum/configmap-%d.yaml", i))
		if !strings.Contains(cm, ".Values.stratum.config") {
			t.Errorf("stratum configmap-%d must reference the shared .Values.stratum.config\n--- configmap ---\n%s", i, cm)
		}
		if strings.Contains(cm, fmt.Sprintf(".Values.stratum%d.config", i)) {
			t.Errorf("stratum configmap-%d references .Values.stratum%d.config, which nothing populates", i, i)
		}
		// The StatefulSet's checksum annotation must name the same key.
		if strings.Contains(ss, fmt.Sprintf(".Values.stratum%d.config", i)) {
			t.Errorf("stratum statefulset checksum references .Values.stratum%d.config, which nothing populates\n--- statefulset ---\n%s", i, ss)
		}
	}

	// The headless Service must publish not-ready addresses, or the members
	// cannot resolve each other at startup and split into one-member clusters.
	hs := chartFile(t, charts, "templates/stratum/service-headless.yaml")
	if !strings.Contains(hs, "publishNotReadyAddresses: true") {
		t.Errorf("headless Service must set publishNotReadyAddresses\n--- service ---\n%s", hs)
	}

	// values.yaml must not carry per-replica placeholders for stratum.
	values := chartFile(t, charts, "values.yaml")
	if strings.Contains(values, "stratum0:") {
		t.Errorf("values.yaml has a stratum0 placeholder; stratum shares one config\n--- values.yaml ---\n%s", values)
	}
}

// TestStratumRendersBothHazelcastClusters pins the wiring for stratum's SECOND
// Hazelcast cluster — the working-set maps.
//
// Two clusters in one JVM cannot share a port, and the event store pins 5001
// with auto-increment off, so the working set needs its own named port and its
// own seed list. Without the seed list it falls back to standalone: each pod
// holds working sets nobody else can see, and the shared Service hands the next
// request to a pod that has never heard of the map.
//
// Kernel has a single cluster and must keep rendering exactly one port block.
func TestStratumRendersBothHazelcastClusters(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			StratumNodes: 3,
		},
	})

	ss := chartFile(t, charts, "templates/stratum/statefulset.yaml")
	for _, want := range []string{
		`- name: STRATUM_SEED_MEMBERS`,
		`"stratum-0.stratum:5001,stratum-1.stratum:5001,stratum-2.stratum:5001"`,
		`- name: STRATUM_WS_SEED_MEMBERS`,
		`"stratum-0.stratum:5701,stratum-1.stratum:5701,stratum-2.stratum:5701"`,
		"containerPort: 5001",
		"containerPort: 5701",
	} {
		if !strings.Contains(ss, want) {
			t.Errorf("stratum statefulset missing %q\n--- statefulset ---\n%s", want, ss)
		}
	}

	// Both ports must be reachable on the headless Service and on each per-pod
	// Service, or a seed address resolves to a member nothing can connect to.
	for _, f := range []string{
		"templates/stratum/service-headless.yaml",
		"templates/stratum/service-0.yaml",
		"templates/stratum/service-1.yaml",
		"templates/stratum/service-2.yaml",
	} {
		svc := chartFile(t, charts, f)
		if !strings.Contains(svc, "name: hazelcast\n") {
			t.Errorf("%s missing the event-store hazelcast port\n--- svc ---\n%s", f, svc)
		}
		if !strings.Contains(svc, "name: hazelcast-ws") || !strings.Contains(svc, "port: 5701") {
			t.Errorf("%s missing the working-set hazelcast port\n--- svc ---\n%s", f, svc)
		}
	}

	// Kernel is a HasHazelcast StatefulSet with ONE cluster: no ws port, no ws seeds.
	ks := chartFile(t, charts, "templates/kernel/statefulset.yaml")
	if strings.Contains(ks, "5701") || strings.Contains(ks, "STRATUM_WS_SEED_MEMBERS") {
		t.Errorf("kernel must not render a working-set cluster\n--- statefulset ---\n%s", ks)
	}
}

func generateTestChart(t *testing.T, cfg *protoOperator.Config) *Charts {
	t.Helper()
	charts, err := New("test", "1.2", nil).Generate(cfg, 1)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	return charts
}

func chartFile(t *testing.T, charts *Charts, path string) string {
	t.Helper()
	data, ok := charts.Files[path]
	if !ok {
		t.Fatalf("generated chart missing %s", path)
	}
	return string(data)
}

// TestPVCDeploymentsUseRecreateStrategy guards a deadlock: a Deployment that owns a
// ReadWriteOnce PVC must use strategy Recreate. With the default RollingUpdate, k8s
// starts the replacement pod before terminating the old one; on a single node both
// mount the same volume and the old pod still holds the data-dir lock, so the new pod
// never becomes Ready and the old one is never torn down. Observed live on eventbus,
// whose replacement pod CrashLooped on Kafka's data-dir lock for hours while the old
// pod kept serving.
func TestPVCDeploymentsUseRecreateStrategy(t *testing.T) {
	src, err := os.ReadFile("generator.go")
	if err != nil {
		t.Fatalf("read generator.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, `strategy = "  strategy:\n    type: Recreate\n"`) {
		t.Error("PVC-owning Deployments must set strategy: Recreate — the RollingUpdate default deadlocks on a ReadWriteOnce volume")
	}
	if !strings.Contains(body, "if svc.HasPVC {") {
		t.Error("the Recreate strategy must be gated on svc.HasPVC")
	}
}

// TestProbesQueryGRPCHealthOnPlaintextPort guards two defects that each shipped once.
//
// (1) tcpSocket probes stay true while a service's guts are dead — a wedged eventbus
// sat 1/1 Ready through drill 7 because port 8443 still accepted connections. Only a
// grpc.health.v1 query reflects handler health.
//
// (2) The probe must target the PLAINTEXT health port 8444, never the TLS service
// ports. The kubelet's gRPC probe cannot speak TLS, so probing 5000/8443 times out on
// every request; doing that restarted all 12 services twice before it was reverted.
//
// timeoutSeconds must stay above the k8s default of 1: a health RPC the application
// must answer needs more budget than a kernel-level TCP accept, and at 1s the JVM
// services were killed 3 times in 28 minutes by spurious liveness failures.
func TestProbesQueryGRPCHealthOnPlaintextPort(t *testing.T) {
	src, err := os.ReadFile("generator.go")
	if err != nil {
		t.Fatalf("read generator.go: %v", err)
	}
	body := string(src)

	for _, probe := range []string{
		"schematic.grpcProbe5000", "schematic.startupProbe5000",
		"schematic.grpcProbe8443", "schematic.startupProbe8443",
	} {
		if !strings.Contains(body, probe) {
			t.Errorf("probe helper %q is missing", probe)
		}
	}
	// No probe helper may fall back to a TCP check.
	probeRegion := body
	if i := strings.Index(probeRegion, `"schematic.grpcProbe5000"`); i >= 0 {
		if j := strings.Index(probeRegion[i:], `"schematic.imagePullSecrets"`); j > 0 {
			probeRegion = probeRegion[i : i+j]
		}
	}
	if strings.Contains(probeRegion, "tcpSocket") {
		t.Error("probe helpers must not use tcpSocket — a TCP accept stays true while the service is dead")
	}
	if strings.Contains(probeRegion, "grpc:\n  port: 8443") || strings.Contains(probeRegion, "grpc:\n  port: 5000") {
		t.Error("grpc probes must target the plaintext health port 8444 — the kubelet cannot probe a TLS port")
	}
	if !strings.Contains(probeRegion, "timeoutSeconds: 5") {
		t.Error("grpc probes need timeoutSeconds above the 1s default, or JVM services flap")
	}
}

// TestPVCPodsSetFsGroupSoNonRootCanWrite guards a defect that kept the fleet's
// event bus dead while every probe reported it healthy.
//
// A freshly provisioned PersistentVolume is root:root 0755. eventbus runs as the
// distroless `nonroot` user (65532), so it could not create
// /app/data/__cluster_metadata-0 and its embedded Kafka broker failed to format
// KRaft storage on every single attempt. It was on retry 131 when this was
// found; stratum and timedb had spent that whole time logging "Kafka not yet
// initialized".
//
// Nothing caught it because the pod's probes only ask whether the gRPC health
// endpoint answers, and it did.
//
// fsGroup must be gated on HasPVC and applied by BOTH renderers -- the
// text/template path and the buffer-built path -- or whichever one a service
// happens to use decides whether its volume is writable.
func TestPVCPodsSetFsGroupSoNonRootCanWrite(t *testing.T) {
	src, err := os.ReadFile("generator.go")
	if err != nil {
		t.Fatalf("read generator.go: %v", err)
	}
	body := string(src)

	if !strings.Contains(body, "func pvcFsGroupLines(svc serviceSpec) string {") {
		t.Fatal("pvcFsGroupLines is gone; a PVC pod's volume is root-owned and a non-root container cannot write to it")
	}
	if !strings.Contains(body, "%s      containers:") {
		t.Error("the buffer-built Deployment must emit the fsGroup securityContext before containers:")
	}
	if !strings.Contains(body, "pvcFsGroupLines(svc)") {
		t.Error("the buffer-built Deployment must call pvcFsGroupLines")
	}
	if !strings.Contains(body, "{{- if $.HasPVC }}\n      securityContext:\n        fsGroup: 65532") {
		t.Error("the templated Deployment must set fsGroup for PVC-owning pods too")
	}
	if !strings.Contains(body, "if !svc.HasPVC {\n\t\treturn \"\"\n\t}") {
		t.Error("fsGroup must be gated on HasPVC — pods with no volume have nothing to chgrp")
	}
}

// TestAppServicesRenderWithoutAPlatformProtoField is the point of app_services.
//
// Every other replica count on Topology is a fixed proto field, so before this
// an application could not declare a service without editing the PLATFORM's
// operator.proto — and proto3 offers no way to add a field to a message owned by
// another repo. echo, a sample service, had to take field 17 there just to be
// deployable, which is the clearest evidence that "schematic is the reusable
// base" was not yet true.
//
// A service named only in the app's own config must now produce a full workload
// with no platform proto change at all.
func TestAppServicesRenderWithoutAPlatformProtoField(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
			AppServices: map[string]int32{"widgeter": 2},
		},
	})

	dep := chartFile(t, charts, "templates/widgeter/deployment.yaml")
	if !strings.Contains(dep, "widgeter") {
		t.Errorf("app service produced no Deployment naming it:\n%s", dep)
	}
	if _, ok := charts.Files["templates/widgeter/service-0.yaml"]; !ok {
		t.Error("app service produced no Service; it would be unreachable in-cluster")
	}
	// A count of 0 means "not deployed" — presence with a positive count is the
	// enablement, so an app service needs no parallel optional_services flag.
	off := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
			AppServices: map[string]int32{"widgeter": 0},
		},
	})
	for path := range off.Files {
		if strings.Contains(path, "widgeter") {
			t.Errorf("app service with 0 replicas still rendered %s", path)
		}
	}
}

// The app_services path must produce the SAME workload as the fixed-field path
// it replaces.
//
// echo is the only service that exists both ways: it has a legacy echo_nodes
// field and it is the model the app-service block was written from. Rendering it
// each way and diffing is the strongest available check that an application
// declaring a service by name gets what a platform service gets — without
// having to deploy one first.
//
// Any difference other than the source of the replica count is a bug in the
// generic path.
func TestAppServiceRendersIdenticallyToTheFixedFieldPath(t *testing.T) {
	viaField := generateTestChart(t, &protoOperator.Config{
		Namespace:        "schematic",
		Topology:         &protoOperator.Topology{KernelNodes: 1, KmakerNodes: 1, EchoNodes: 1},
		OptionalServices: &protoOperator.OptionalServices{Echo: true},
	})
	viaMap := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{KernelNodes: 1, KmakerNodes: 1,
			AppServices: map[string]int32{"echo": 1}},
	})

	// Without this the whole comparison passes vacuously when neither rendering
	// emits echo at all — which is exactly how a "both paths agree" test lies.
	compared := 0
	for path, want := range viaField.Files {
		if !strings.HasPrefix(path, "templates/echo/") {
			continue
		}
		compared++
		got, ok := viaMap.Files[path]
		if !ok {
			t.Errorf("app_services did not produce %s, which the fixed field does", path)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s differs between the two paths:\n--- via echo_nodes ---\n%s\n--- via app_services ---\n%s",
				path, want, got)
		}
	}
	if compared == 0 {
		t.Fatal("neither path rendered any echo templates; the comparison proved nothing")
	}

	// And the reverse: no extra files the fixed path does not create.
	for path := range viaMap.Files {
		if !strings.HasPrefix(path, "templates/echo/") {
			continue
		}
		if _, ok := viaField.Files[path]; !ok {
			t.Errorf("app_services produced %s that the fixed field does not", path)
		}
	}
}

// TestEveryWorkloadToleratesTheArmTaint guards the toleration that lets this
// fleet schedule at all.
//
// The whole fleet is arm64 by design (control/GKE.md: t2a Spot nodes) and GKE
// taints Arm nodes kubernetes.io/arch=arm64:NoSchedule. Missing the toleration
// is invisible until a node is REPLACED — a running pod is not evicted when its
// node gains a NoSchedule taint — so it stays latent until the worst moment.
//
// It surfaced on a Spot preemption: the replacement node carried the taint and
// 14 of 15 pods went Pending with "1 node(s) had untolerated taint(s)" while the
// node sat at 25% CPU. The single pod that stayed up was cert-manager's
// acme-solver, which ships this toleration itself.
func TestEveryWorkloadToleratesTheArmTaint(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			StratumNodes:  3,
			SentinelNodes: 1,
			GatewayNodes:  1,
			EventbusNodes: 1,
			AppServices:   map[string]int32{"echo": 1},
		},
	})

	checked := 0
	for path, body := range charts.Files {
		if !strings.HasSuffix(path, "/deployment.yaml") && !strings.HasSuffix(path, "/statefulset.yaml") {
			continue
		}
		checked++
		text := string(body)
		if !strings.Contains(text, "key: kubernetes.io/arch") ||
			!strings.Contains(text, "value: arm64") ||
			!strings.Contains(text, "effect: NoSchedule") {
			t.Errorf("%s has no arm64 toleration; it cannot schedule on a tainted Arm node\n%s", path, text)
		}
	}
	if checked == 0 {
		t.Fatal("guard: no workload manifests were examined")
	}
}

// TestMultiReplicaServicesAskToSpreadAcrossNodes pins the anti-affinity that
// makes replica count mean something.
//
// Three kernels or three stratums stacked on one node survive nothing, and
// their Hazelcast members would share exactly the fate they exist to be
// independent of. The rule is PREFERRED, not required: with more replicas than
// nodes a required rule leaves pods Pending forever, and redundancy that cannot
// schedule is worse than co-location.
func TestMultiReplicaServicesAskToSpreadAcrossNodes(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:   3,
			StratumNodes:  3,
			SentinelNodes: 1,
		},
	})

	for _, path := range []string{
		"templates/kernel/statefulset.yaml",
		"templates/stratum/statefulset.yaml",
	} {
		body := chartFile(t, charts, path)
		for _, want := range []string{
			"podAntiAffinity",
			"preferredDuringSchedulingIgnoredDuringExecution",
			"topologyKey: kubernetes.io/hostname",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %q — replicas would stack on one node\n%s", path, want, body)
			}
		}
		if strings.Contains(body, "requiredDuringSchedulingIgnoredDuringExecution") {
			t.Errorf("%s uses a REQUIRED anti-affinity rule; with replicas > nodes that leaves pods Pending forever", path)
		}
	}

	// A single-replica service gains nothing from the rule and should not carry it.
	single := chartFile(t, charts, "templates/sentinel/deployment.yaml")
	if strings.Contains(single, "podAntiAffinity") {
		t.Errorf("a 1-replica service should not carry anti-affinity\n%s", single)
	}
}

// A StatefulSet owns its volume through a volumeClaimTemplate, which is exactly
// the case that was missing fsGroup: the Deployment path called
// pvcFsGroupLines and the StatefulSet path did not, so kernel and stratum were
// the only two services that could not write to their own disk.
//
// It failed silently for as long as it existed. Kernel's persistence layer logs
// a WARN and returns -- "Failed to persist kernel disk snapshot to
// /app/data/kernel: AccessDeniedException" -- so every probe passed, the fleet
// was healthy, and the config state simply never reached the disk. Turn-down
// then looked like it was destroying state that had in fact never been written.
func TestStatefulSetsCanWriteToTheirOwnVolume(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:   3,
			StratumNodes:  3,
			SentinelNodes: 1,
		},
	})

	for _, path := range []string{
		"templates/kernel/statefulset.yaml",
		"templates/stratum/statefulset.yaml",
	} {
		body := chartFile(t, charts, path)
		if !strings.Contains(body, "volumeClaimTemplates") {
			t.Fatalf("%s has no volumeClaimTemplates — this test is asserting against the wrong file", path)
		}
		if !strings.Contains(body, "fsGroup: 65532") {
			t.Errorf("%s mounts a PVC but sets no fsGroup; a distroless nonroot image cannot write to a root-owned volume\n%s", path, body)
		}
	}
}

// TestKeyMaterialConfigsArePackagedAsSecrets: kernel's per-node config carries
// the node TLS key and its signing CA key, kmaker's carries the cluster CA
// signing key, and gateway's carries the external TLS key. All three were
// packaged as ConfigMaps, which are not treated as secret data by Kubernetes —
// no encryption-at-rest by default, and readable by anything with configmap get.
func TestKeyMaterialConfigsArePackagedAsSecrets(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:   3,
			KmakerNodes:   1,
			GatewayNodes:  1,
			SentinelNodes: 1,
		},
	})

	for _, tc := range []struct {
		svc      string
		replicas int
	}{
		{"kernel", 3},
		{"kmaker", 1},
		{"gateway", 1},
	} {
		for i := range tc.replicas {
			if _, ok := charts.Files[fmt.Sprintf("templates/%s/configmap-%d.yaml", tc.svc, i)]; ok {
				t.Errorf("%s config %d is still emitted as a ConfigMap; it holds private key material", tc.svc, i)
			}
			raw, ok := charts.Files[fmt.Sprintf("templates/%s/secret-%d.yaml", tc.svc, i)]
			if !ok {
				t.Errorf("generated chart has no templates/%s/secret-%d.yaml", tc.svc, i)
				continue
			}
			secret := string(raw)
			for _, want := range []string{"kind: Secret", "type: Opaque", "stringData:", "config.yaml:"} {
				if !strings.Contains(secret, want) {
					t.Errorf("%s secret-%d missing %q\n--- secret ---\n%s", tc.svc, i, want, secret)
				}
			}
			if strings.Contains(secret, "kind: ConfigMap") {
				t.Errorf("%s secret-%d is a ConfigMap\n--- secret ---\n%s", tc.svc, i, secret)
			}
		}
	}

	// The workloads must mount the Secret, not a ConfigMap of the same name.
	statefulSet := chartFile(t, charts, "templates/kernel/statefulset.yaml")
	for i := range 3 {
		want := fmt.Sprintf("- name: config-%d\n        secret:\n          secretName: kernel-%d-config", i, i)
		if !strings.Contains(statefulSet, want) {
			t.Errorf("kernel StatefulSet does not mount config-%d from a Secret\n--- statefulset ---\n%s", i, statefulSet)
		}
	}
	for i := range 3 {
		if strings.Contains(statefulSet, fmt.Sprintf("configMap:\n          name: kernel-%d-config", i)) {
			t.Errorf("kernel StatefulSet still mounts config-%d from a ConfigMap\n--- statefulset ---\n%s", i, statefulSet)
		}
	}
	for _, svc := range []string{"kmaker", "gateway"} {
		deployment := chartFile(t, charts, fmt.Sprintf("templates/%s/deployment.yaml", svc))
		want := fmt.Sprintf("- name: config\n        secret:\n          secretName: %s0-config", svc)
		if !strings.Contains(deployment, want) {
			t.Errorf("%s Deployment does not mount its config from a Secret\n--- deployment ---\n%s", svc, deployment)
		}
		if strings.Contains(deployment, fmt.Sprintf("configMap:\n          name: %s0-config", svc)) {
			t.Errorf("%s Deployment still mounts its config from a ConfigMap\n--- deployment ---\n%s", svc, deployment)
		}
	}

	// A service with no key material in its config stays a ConfigMap.
	sentinel := chartFile(t, charts, "templates/sentinel/configmap-0.yaml")
	if !strings.Contains(sentinel, "kind: ConfigMap") {
		t.Errorf("sentinel config should still be a ConfigMap\n--- configmap ---\n%s", sentinel)
	}
}

// TestWriteToDiskRemovesSupersededConfigMapTemplate: an output directory
// generated before the key-material configs moved to Secrets still holds
// templates/kernel/configmap-0.yaml. Nothing else prunes that directory, and
// helm would apply the stale ConfigMap alongside the new Secret.
func TestWriteToDiskRemovesSupersededConfigMapTemplate(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "schematic", "templates", "kernel", "configmap-0.yaml")
	if err := os.MkdirAll(filepath.Dir(stale), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("kind: ConfigMap\n"), 0644); err != nil {
		t.Fatal(err)
	}

	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology:  &protoOperator.Topology{KernelNodes: 1},
	})
	if err := New("test", "1.2", nil).WriteToDisk(charts, dir); err != nil {
		t.Fatalf("WriteToDisk() error = %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale kernel configmap-0.yaml still on disk (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "schematic", "templates", "kernel", "secret-0.yaml")); err != nil {
		t.Errorf("kernel secret-0.yaml not written: %v", err)
	}
}

// TestChartLabelsWorkloadsWithTheOperatorManagedByValue guards the contract the
// reconciler's stale-deployment pruning selects on. Helm 3 does not set
// app.kubernetes.io/managed-by by itself, so the value is whatever this chart's
// helper writes -- and the reconciler must select on exactly that.
func TestChartLabelsWorkloadsWithTheOperatorManagedByValue(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			GatewayNodes: 1,
		},
	})

	want := ManagedByLabel + ": " + ManagedByValue
	helpers := chartFile(t, charts, "templates/_helpers.tpl")
	if !strings.Contains(helpers, want) {
		t.Fatalf("_helpers.tpl missing %q\n--- helpers ---\n%s", want, helpers)
	}
	if strings.Contains(helpers, ManagedByLabel+": Helm") {
		t.Fatalf("_helpers.tpl still labels resources managed-by Helm:\n%s", helpers)
	}

	// Every workload pulls the label in via the shared helper.
	for _, path := range []string{
		"templates/kernel/statefulset.yaml",
		"templates/gateway/deployment.yaml",
	} {
		body := chartFile(t, charts, path)
		if !strings.Contains(body, `include "schematic.labels"`) {
			t.Fatalf("%s does not include the schematic.labels helper:\n%s", path, body)
		}
	}
}

// TestWriteToDiskWritesChartFilesPrivate: the rendered chart holds the GitLab
// registry token (templates/registry-secret.yaml) and the service configs
// carrying kernel's and kmaker's private keys (templates/<svc>/secret-N.yaml).
// Every file was written 0644, so all of that was world-readable on the
// operator host. It also has to tighten a file already on disk: the tree is
// routinely regenerated in place and os.WriteFile leaves an existing file's
// mode alone, so a chart first rendered by a pre-fix binary would stay 0644.
func TestWriteToDiskWritesChartFilesPrivate(t *testing.T) {
	dir := t.TempDir()
	preExisting := filepath.Join(dir, "schematic", "templates", "registry-secret.yaml")
	if err := os.MkdirAll(filepath.Dir(preExisting), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preExisting, []byte("kind: Secret\n"), 0644); err != nil {
		t.Fatal(err)
	}

	charts := generateTestChart(t, &protoOperator.Config{
		Namespace:           "schematic",
		GitlabRegistryToken: "glpat-not-a-real-token",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
		},
	})
	if _, ok := charts.Files["templates/registry-secret.yaml"]; !ok {
		t.Fatal("generated chart has no templates/registry-secret.yaml — test would not cover the token")
	}
	if _, ok := charts.Files["templates/kernel/secret-0.yaml"]; !ok {
		t.Fatal("generated chart has no templates/kernel/secret-0.yaml — test would not cover key material")
	}

	if err := New("test", "1.2", nil).WriteToDisk(charts, dir); err != nil {
		t.Fatalf("WriteToDisk() error = %v", err)
	}

	for relPath := range charts.Files {
		info, err := os.Stat(filepath.Join(dir, "schematic", relPath))
		if err != nil {
			t.Errorf("stat %s: %v", relPath, err)
			continue
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Errorf("%s mode = %#o, want 0600 (the chart tree carries the registry token and kernel's private keys)", relPath, got)
		}
	}
}
