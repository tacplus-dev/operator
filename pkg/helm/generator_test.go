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
			TimedbNodes:     1,
			GraphdbNodes:    1,
			ArgusNodes:      1,
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
		"argus",
		"timedb",
		"graphdb",
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

func TestOperatorChartRBACCanManageGeneratedServiceRBAC(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "helm", "templates", "clusterrole.yaml"))
	if err != nil {
		t.Fatalf("read operator ClusterRole template: %v", err)
	}
	clusterRole := string(data)
	for _, want := range []string{
		"- roles",
		"- rolebindings",
		"- clusterroles",
		"- clusterrolebindings",
		"- pods",
		"verbs: [\"get\"]",
	} {
		if !strings.Contains(clusterRole, want) {
			t.Fatalf("operator ClusterRole missing %q\n--- clusterrole ---\n%s", want, clusterRole)
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

	// configmap-0.yaml must reference .Values.kernel0.config (not .Values.kernel.config).
	cm := chartFile(t, charts, "templates/kernel/configmap-0.yaml")
	if !strings.Contains(cm, ".Values.kernel0.config") {
		t.Fatalf("kernel configmap-0 must reference .Values.kernel0.config\n--- configmap ---\n%s", cm)
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
