package reconciler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gitlab.com/seabeam/schematic/platform/services/operator/pkg/helm"
)

func testNode(pool string, ready, unschedulable bool) corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	n := corev1.Node{
		Spec:   corev1.NodeSpec{Unschedulable: unschedulable},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}},
	}
	if pool != "" {
		n.ObjectMeta = metav1.ObjectMeta{Labels: map[string]string{helm.NodePoolLabel: pool}}
	}
	return n
}

// TestCountSchedulableNodes: with no core pool every Ready, schedulable node
// counts and pools are off; once a Ready core node exists pools are on and only
// core nodes count, because the count sizes the gateways and gateways live on
// core. A core label on a node that is not Ready or is cordoned turns nothing on.
func TestCountSchedulableNodes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nodes     []corev1.Node
		wantCount int
		wantPools bool
	}{
		{"unlabelled cluster", []corev1.Node{testNode("", true, false), testNode("", true, false), testNode("", false, false)}, 2, false},
		{"core and tenant pools", []corev1.Node{testNode(helm.CorePool, true, false), testNode(helm.CorePool, true, false), testNode(helm.TenantPool, true, false), testNode(helm.TenantPool, true, false), testNode(helm.TenantPool, true, false)}, 2, true},
		{"only tenant pool labelled", []corev1.Node{testNode("", true, false), testNode(helm.TenantPool, true, false)}, 2, false},
		{"core node not ready", []corev1.Node{testNode(helm.CorePool, false, false), testNode("", true, false)}, 1, false},
		{"core node cordoned", []corev1.Node{testNode(helm.CorePool, true, true), testNode("", true, false)}, 1, false},
	} {
		count, pools := countSchedulableNodes(tc.nodes)
		if count != tc.wantCount || pools != tc.wantPools {
			t.Errorf("%s: countSchedulableNodes = (%d, %v), want (%d, %v)", tc.name, count, pools, tc.wantCount, tc.wantPools)
		}
	}
}
