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

// TestTheSharedStratumsClaimsAreKept is #54 on #2's upgrade: Stratum is per
// tenant and in memory now, so the chart no longer renders the shared stratum
// StatefulSet whose writers had volumes, and their PersistentVolumeClaims stay,
// on purpose. The operator deletes none of them and says so: one warning
// naming the kept claims and the command that removes them. What the chart
// still runs with a volume (kernel, redo) is not named.
func TestTheSharedStratumsClaimsAreKept(t *testing.T) {
	ctx := context.Background()
	cfg := &protoOperator.Config{
		Namespace: "schematic",
		Tenants:   []string{"acme"},
		Topology:  &protoOperator.Topology{KernelNodes: 1},
	}

	// The cluster: the StatefulSets this chart renders, the claims their
	// volumeClaimTemplates made, and the old shared stratum's three claims.
	rendered, err := renderStatefulSets(keptClaimsTestChart(t, cfg), "schematic", map[string]interface{}{})
	if err != nil {
		t.Fatalf("rendering the chart: %v", err)
	}
	if rendered["stratum"] != nil || rendered["stratum-acme"] == nil || rendered["redo"] == nil {
		t.Fatalf("guard: want stratum-acme and redo and no shared stratum, got %v", rendered)
	}
	var claims []client.Object
	for _, st := range rendered {
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
						Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")}},
					},
				})
			}
		}
	}
	if len(claims) != 1+1 {
		t.Fatalf("expected kernel's and redo's claims, got %d", len(claims))
	}
	for i := range 3 {
		claims = append(claims, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("data-stratum-%d", i), Namespace: "schematic", Labels: map[string]string{"app": "stratum"},
		}})
	}
	objs := slices.Clone(claims)
	for _, st := range rendered {
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
		"stratum": {"data-stratum-0", "data-stratum-1", "data-stratum-2"},
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

// TestScalingKernelDownKeepsTheClaimsPastItsReplicas: 3 → 1 keeps the two
// claims the StatefulSet no longer mounts, the way Kubernetes' default
// retention does, and the operator names them as well.
func TestScalingKernelDownKeepsTheClaimsPastItsReplicas(t *testing.T) {
	ctx := context.Background()
	cfg := &protoOperator.Config{
		Namespace: "schematic",
		Topology:  &protoOperator.Topology{KernelNodes: 1},
	}
	rendered, err := renderStatefulSets(keptClaimsTestChart(t, cfg), "schematic", map[string]interface{}{})
	if err != nil {
		t.Fatalf("rendering the chart: %v", err)
	}
	rendered["kernel"].Namespace = "schematic"
	objs := []client.Object{rendered["kernel"]}
	for i := range 3 {
		objs = append(objs, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("data-kernel-%d", i), Namespace: "schematic", Labels: map[string]string{"app": "kernel"},
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
	if !slices.Equal(got, []string{"data-kernel-1", "data-kernel-2"}) {
		t.Errorf("kept claims = %v, want data-kernel-1 and data-kernel-2", got)
	}
}
