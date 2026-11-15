package helm

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/tacplus-dev/operator/pkg/deployment"
	protoKernel "github.com/tacplus-dev/operator/tacplus/types/services/kernel"
	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
	protoHost "github.com/tacplus-dev/operator/tacplus/types/sys/host"
)

// source is where a connection comes from: a pod in a namespace, or (pod nil)
// an address no pod selector matches, such as the kubelet's on the node.
type source struct {
	namespace string
	nsLabels  map[string]string
	pod       map[string]string
}

func platformPod(ns string, app string) source {
	return source{namespace: ns, nsLabels: map[string]string{"kubernetes.io/metadata.name": ns}, pod: map[string]string{"app": app}}
}

func tenantPod(dep, ns string) source {
	return source{namespace: ns, nsLabels: map[string]string{"kubernetes.io/metadata.name": ns, deployment.OwnerLabel: dep}, pod: map[string]string{"app": "docmanager-acme"}}
}

var node = source{}

func selects(t *testing.T, sel *metav1.LabelSelector, set map[string]string) bool {
	t.Helper()
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		t.Fatalf("selector %+v: %v", sel, err)
	}
	return s.Matches(labels.Set(set))
}

// admitted evaluates the rendered NetworkPolicies as Kubernetes does: a pod no
// ingress policy selects admits everything; one that some policy selects
// admits what any rule of any of them admits.
func admitted(t *testing.T, objs []renderedObject, ns string, dst map[string]string, from source, port int) bool {
	t.Helper()
	selected := false
	for _, o := range objs {
		if o.Kind != "NetworkPolicy" || o.Namespace != ns {
			continue
		}
		np := decodePolicy(t, o)
		if !selects(t, &np.Spec.PodSelector, dst) {
			continue
		}
		selected = true
		for _, rule := range np.Spec.Ingress {
			if ruleAdmits(t, ns, rule, from, port) {
				return true
			}
		}
	}
	return !selected
}

func ruleAdmits(t *testing.T, ns string, rule networkingv1.NetworkPolicyIngressRule, from source, port int) bool {
	portOK := len(rule.Ports) == 0
	for _, p := range rule.Ports {
		if p.Port == nil || p.Port.IntValue() == port || (p.EndPort != nil && p.Port.IntValue() <= port && port <= int(*p.EndPort)) {
			portOK = true
		}
	}
	if !portOK {
		return false
	}
	if len(rule.From) == 0 {
		return true
	}
	if from.pod == nil {
		return false
	}
	for _, peer := range rule.From {
		if peer.IPBlock != nil {
			t.Fatalf("unexpected ipBlock peer %+v", peer)
		}
		nsOK := from.namespace == ns
		if peer.NamespaceSelector != nil {
			nsOK = selects(t, peer.NamespaceSelector, from.nsLabels)
		}
		podOK := peer.PodSelector == nil || selects(t, peer.PodSelector, from.pod)
		if nsOK && podOK {
			return true
		}
	}
	return false
}

var kernelPod = map[string]string{"app": "kernel"}

// Kernel's Raft port carries unauthenticated gRPC until kernel#42, and its
// Hazelcast member port an unauthenticated member protocol holding kernel's
// state (#30): in a deployment only kernel's own pods reach either, while the
// deployment keeps reaching kernel's gRPC port and the kubelet its health port.
func TestKernelRaftPortAdmitsOnlyKernelPodsInADeployment(t *testing.T) {
	objs := renderedObjects(t, generateDeployment(t, deploymentConfig("prod")))
	raft := int(protoKernel.DefaultRatisListenerPort)
	hz := int(protoKernel.DefaultHazelcastListenerPort)

	np := decodePolicy(t, findObject(t, objs, "NetworkPolicy", "tac-prod", "kernel-raft"))
	if !selects(t, &np.Spec.PodSelector, kernelPod) || selects(t, &np.Spec.PodSelector, map[string]string{"app": "gateway0"}) {
		t.Errorf("kernel-raft must select kernel's pods and only them: %+v", np.Spec.PodSelector)
	}

	for _, c := range []struct {
		name string
		from source
		port int
		want bool
	}{
		{"kernel to raft", platformPod("tac-prod", "kernel"), raft, true},
		{"gateway to raft", platformPod("tac-prod", "gateway0"), raft, false},
		{"tenant to raft", tenantPod("prod", "tac-prod-a1b2c3d4"), raft, false},
		{"other deployment to raft", platformPod("tac-dev", "kernel"), raft, false},
		{"node to raft", node, raft, false},
		{"gateway to grpc", platformPod("tac-prod", "gateway0"), 5000, true},
		{"tenant to grpc", tenantPod("prod", "tac-prod-a1b2c3d4"), 5000, true},
		{"other deployment to grpc", platformPod("tac-dev", "gateway0"), 5000, false},
		{"kernel to hazelcast", platformPod("tac-prod", "kernel"), hz, true},
		{"gateway to hazelcast", platformPod("tac-prod", "gateway0"), hz, false},
		{"tenant to hazelcast", tenantPod("prod", "tac-prod-a1b2c3d4"), hz, false},
		{"other deployment's kernel to hazelcast", platformPod("tac-dev", "kernel"), hz, false},
		{"other deployment to hazelcast", platformPod("tac-dev", "gateway0"), hz, false},
		{"node to hazelcast", node, hz, false},
		{"node to health", node, grpcHealthProbePort, true},
	} {
		if got := admitted(t, objs, "tac-prod", kernelPod, c.from, c.port); got != c.want {
			t.Errorf("%s (port %d): admitted = %v, want %v", c.name, c.port, got, c.want)
		}
	}

	// The rest of the namespace keeps its isolation.
	if !admitted(t, objs, "tac-prod", map[string]string{"app": "stratum"}, platformPod("tac-prod", "gateway0"), 8443) {
		t.Error("the deployment no longer reaches stratum")
	}
	if admitted(t, objs, "tac-prod", map[string]string{"app": "stratum"}, platformPod("tac-dev", "gateway0"), 8443) {
		t.Error("another deployment reaches stratum")
	}
}

// Without a deployment (one namespace) the same ports are kernel's alone; its
// gRPC port stays open as it was.
func TestKernelRaftPortAdmitsOnlyKernelPodsInOneNamespace(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology:  &protoOperator.Topology{KernelNodes: 3, GatewayNodes: 1},
	})
	objs := renderedObjects(t, charts)
	raft := int(protoKernel.DefaultRatisListenerPort)
	hz := int(protoKernel.DefaultHazelcastListenerPort)
	for _, c := range []struct {
		name string
		from source
		port int
		want bool
	}{
		{"kernel to raft", platformPod("schematic", "kernel"), raft, true},
		{"gateway to raft", platformPod("schematic", "gateway0"), raft, false},
		{"other namespace's kernel to raft", platformPod("elsewhere", "kernel"), raft, false},
		{"gateway to grpc", platformPod("schematic", "gateway0"), 5000, true},
		{"kernel to hazelcast", platformPod("schematic", "kernel"), hz, true},
		{"gateway to hazelcast", platformPod("schematic", "gateway0"), hz, false},
		{"other namespace's kernel to hazelcast", platformPod("elsewhere", "kernel"), hz, false},
		{"node to health", node, grpcHealthProbePort, true},
	} {
		if got := admitted(t, objs, "schematic", kernelPod, c.from, c.port); got != c.want {
			t.Errorf("%s (port %d): admitted = %v, want %v", c.name, c.port, got, c.want)
		}
	}
}

// The port is kernel's Config.ratis_listener, never the operator's own guess.
func TestKernelRaftPortComesFromKernelConfig(t *testing.T) {
	kernel := &protoKernel.Config{RatisListener: &protoHost.Host{Host: "0.0.0.0", Port: 6002}}
	charts, err := New("test", "1.2", nil, map[string]bool{"docmanager": true}, nil).
		WithKernelConfig(kernel).Generate(deploymentConfig("prod"), 0, false)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	objs := renderedObjects(t, charts)
	if !admitted(t, objs, "tac-prod", kernelPod, platformPod("tac-prod", "kernel"), 6002) {
		t.Error("kernel's pods do not reach the configured Raft port")
	}
	if admitted(t, objs, "tac-prod", kernelPod, platformPod("tac-prod", "gateway0"), 6002) {
		t.Error("the configured Raft port admits a pod that is not kernel")
	}

	if got := KernelRaftPort(nil); got != int32(protoKernel.DefaultRatisListenerPort) {
		t.Errorf("KernelRaftPort(nil) = %d, want kernel's default %d", got, protoKernel.DefaultRatisListenerPort)
	}
	if got := KernelRaftPort(kernel); got != 6002 {
		t.Errorf("KernelRaftPort = %d, want 6002", got)
	}
}
