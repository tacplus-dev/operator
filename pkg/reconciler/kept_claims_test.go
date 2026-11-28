package reconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tacplus-dev/operator/pkg/helm"
	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// keptClaimsTestChart is the chart the generator emits for cfg.
func keptClaimsTestChart(t *testing.T, cfg *protoOperator.Config) *helmchart.Chart {
	t.Helper()
	charts, err := helm.New("test", "1.2", nil, nil, nil).Generate(cfg, 1, false)
	if err != nil {
		t.Fatalf("generating chart: %v", err)
	}
	buffered := make([]*helmloader.BufferedFile, 0, len(charts.Files))
	for name, data := range charts.Files {
		buffered = append(buffered, &helmloader.BufferedFile{Name: name, Data: data})
	}
	ch, err := helmloader.LoadFiles(buffered)
	if err != nil {
		t.Fatalf("loading generated chart: %v", err)
	}
	return ch
}

// TestScalingStratumToZeroKeepsItsClaims is #54: stratum_nodes 3 → 0 removes
// stratum's StatefulSet (and redo's, which is rendered with it), and their
// PersistentVolumeClaims stay, on purpose. The operator deletes none of them
// and says so: one warning per StatefulSet naming its kept claims and the
// command that removes them. Kernel, still running, is not named.
func TestScalingStratumToZeroKeepsItsClaims(t *testing.T) {
	ctx := context.Background()
	cfg := &protoOperator.Config{
		Namespace: "schematic",
		Topology:  &protoOperator.Topology{KernelNodes: 1, StratumNodes: 3},
	}

	// The cluster as the chart at stratum_nodes 3 left it: its StatefulSets and
	// the claims their volumeClaimTemplates made.
	before, err := renderStatefulSets(keptClaimsTestChart(t, cfg), "schematic", map[string]interface{}{})
	if err != nil {
		t.Fatalf("rendering the chart at stratum_nodes 3: %v", err)
	}
	var claims []client.Object
	for _, st := range before {
		for _, tmpl := range st.Spec.VolumeClaimTemplates {
			for i := range int(*st.Spec.Replicas) {
				claims = append(claims, &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-%s-%d", tmpl.Name, st.Name, i),
						Namespace: "schematic",
						Labels:    st.Spec.Selector.MatchLabels,
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")}},
					},
				})
			}
		}
	}
	if len(claims) != 1+3+1 {
		t.Fatalf("expected kernel's, three stratum's and redo's claims at stratum_nodes 3, got %d", len(claims))
	}

	// stratum_nodes 0: Helm leaves the StatefulSets the chart still renders.
	cfg.Topology.StratumNodes = 0
	after, err := renderStatefulSets(keptClaimsTestChart(t, cfg), "schematic", map[string]interface{}{})
	if err != nil {
		t.Fatalf("rendering the chart at stratum_nodes 0: %v", err)
	}
	if after["stratum"] != nil || after["redo-restore"] != nil {
		t.Fatalf("stratum_nodes 0 still renders stratum or redo: %v", after)
	}
	objs := slices.Clone(claims)
	for _, st := range after {
		st.Namespace = "schematic"
		objs = append(objs, st)
	}
	cl := fake.NewClientBuilder().WithScheme(immutableTestScheme(t)).WithObjects(objs...).Build()

	core, logs := observer.New(zapcore.InfoLevel)
	r := &Reconciler{cfg: cfg, client: cl, log: zap.New(core)}
	r.reportKeptClaims(ctx, cfg)

	// Nothing is deleted.
	var left corev1.PersistentVolumeClaimList
	if err := cl.List(ctx, &left); err != nil {
		t.Fatalf("listing claims: %v", err)
	}
	if len(left.Items) != len(claims) {
		t.Fatalf("expected every claim kept, %d of %d are left", len(left.Items), len(claims))
	}

	kept := map[string][]string{}
	for _, e := range logs.FilterMessage(keptClaimsMessage).All() {
		if e.Level != zapcore.WarnLevel {
			t.Errorf("kept claims logged at %v, want warn", e.Level)
		}
		f := e.ContextMap()
		st, _ := f["statefulset"].(string)
		for _, c := range f["claims"].([]interface{}) {
			kept[st] = append(kept[st], c.(string))
		}
		remove, _ := f["remove"].(string)
		for _, c := range kept[st] {
			if !strings.Contains(remove, c) || !strings.HasPrefix(remove, "kubectl delete pvc -n schematic ") {
				t.Errorf("remove command %q does not delete %s in schematic", remove, c)
			}
		}
	}
	want := map[string][]string{
		"stratum":      {"data-stratum-0", "data-stratum-1", "data-stratum-2"},
		"redo-restore": {"data-redo-restore-0"},
	}
	if len(kept) != len(want) {
		t.Fatalf("kept claims reported = %v, want %v", kept, want)
	}
	for st, names := range want {
		if !slices.Equal(kept[st], names) {
			t.Errorf("kept claims of %s = %v, want %v", st, kept[st], names)
		}
	}

	// The next pass, with nothing changed, does not say it again.
	r.reportKeptClaims(ctx, cfg)
	if n := logs.FilterMessage(keptClaimsMessage).Len(); n != len(want) {
		t.Errorf("an unchanged second pass logged again: %d warnings, want %d", n, len(want))
	}
}

// TestScalingStratumDownKeepsTheClaimsPastItsReplicas: 3 → 1 keeps the two
// claims the StatefulSet no longer mounts, the way Kubernetes' default
// retention does, and the operator names them as well.
func TestScalingStratumDownKeepsTheClaimsPastItsReplicas(t *testing.T) {
	ctx := context.Background()
	cfg := &protoOperator.Config{
		Namespace: "schematic",
		Topology:  &protoOperator.Topology{KernelNodes: 1, StratumNodes: 1},
	}
	rendered, err := renderStatefulSets(keptClaimsTestChart(t, cfg), "schematic", map[string]interface{}{})
	if err != nil {
		t.Fatalf("rendering the chart: %v", err)
	}
	rendered["stratum"].Namespace = "schematic"
	objs := []client.Object{rendered["stratum"]}
	for i := range 3 {
		objs = append(objs, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("data-stratum-%d", i), Namespace: "schematic", Labels: map[string]string{"app": "stratum"},
		}})
	}
	cl := fake.NewClientBuilder().WithScheme(immutableTestScheme(t)).WithObjects(objs...).Build()

	core, logs := observer.New(zapcore.InfoLevel)
	r := &Reconciler{cfg: cfg, client: cl, log: zap.New(core)}
	r.reportKeptClaims(ctx, cfg)

	entries := logs.FilterMessage(keptClaimsMessage).All()
	if len(entries) != 1 {
		t.Fatalf("expected one warning, got %d", len(entries))
	}
	var got []string
	for _, c := range entries[0].ContextMap()["claims"].([]interface{}) {
		got = append(got, c.(string))
	}
	if !slices.Equal(got, []string{"data-stratum-1", "data-stratum-2"}) {
		t.Errorf("kept claims = %v, want data-stratum-1 and data-stratum-2", got)
	}
}
