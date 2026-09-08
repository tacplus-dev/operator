package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	helmrelease "helm.sh/helm/v3/pkg/release"
	helmtime "helm.sh/helm/v3/pkg/time"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	protoKernel "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/kernel"
	protoOpa "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/opa"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
	protoOpalib "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/sys/opalib"
	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/helm"
)

func TestRemoveKernelHotfixDrift(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding core scheme: %v", err)
	}

	st := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kernel",
			Namespace: "schematic",
			Labels: map[string]string{
				helm.ManagedByLabel: helm.ManagedByValue,
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "kernel",
							Command: []string{"java"},
							Args:    []string{"-jar", "/hotfix/kernel-261.jar"},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "bootstrap", MountPath: "/bootstrap"},
								{Name: "data", MountPath: "/app/data"},
								{Name: "kernel-hotfix", MountPath: "/hotfix"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{Name: "bootstrap", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "config-0", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
						{Name: "kernel-hotfix", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/opt/schematic-hotfix"}}},
					},
				},
			},
		},
	}

	r := &Reconciler{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(st).Build(),
		log:    zap.NewNop(),
	}

	if err := r.removeKernelHotfixDrift(context.Background(), "schematic"); err != nil {
		t.Fatalf("removing hotfix drift: %v", err)
	}

	var got appsv1.StatefulSet
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "schematic", Name: "kernel"}, &got); err != nil {
		t.Fatalf("getting statefulset: %v", err)
	}

	kernel := got.Spec.Template.Spec.Containers[0]
	if len(kernel.Command) != 0 {
		t.Fatalf("expected command to be cleared, got %#v", kernel.Command)
	}
	if len(kernel.Args) != 0 {
		t.Fatalf("expected args to be cleared, got %#v", kernel.Args)
	}
	if names := volumeMountNames(kernel.VolumeMounts); len(names) != 2 || names[0] != "bootstrap" || names[1] != "data" {
		t.Fatalf("unexpected volume mounts after cleanup: %#v", names)
	}
	if names := volumeNames(got.Spec.Template.Spec.Volumes); len(names) != 2 || names[0] != "bootstrap" || names[1] != "config-0" {
		t.Fatalf("unexpected volumes after cleanup: %#v", names)
	}
}

func TestRemoveKernelHotfixDriftSkipsNonHelmStatefulSet(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding core scheme: %v", err)
	}

	st := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "kernel", Namespace: "schematic"},
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "kernel",
						Command: []string{"java"},
						Args:    []string{"-jar", "/hotfix/kernel-261.jar"},
					}},
				},
			},
		},
	}

	r := &Reconciler{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(st).Build(),
		log:    zap.NewNop(),
	}

	if err := r.removeKernelHotfixDrift(context.Background(), "schematic"); err != nil {
		t.Fatalf("removing hotfix drift: %v", err)
	}

	var got appsv1.StatefulSet
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "schematic", Name: "kernel"}, &got); err != nil {
		t.Fatalf("getting statefulset: %v", err)
	}
	if len(got.Spec.Template.Spec.Containers[0].Args) == 0 {
		t.Fatal("expected non-Helm StatefulSet to be left unchanged")
	}
}

func TestIsStalePendingRelease(t *testing.T) {
	now := time.Date(2026, 6, 17, 22, 0, 0, 0, time.UTC)

	rel := &helmrelease.Release{
		Info: &helmrelease.Info{
			Status:       helmrelease.StatusPendingUpgrade,
			LastDeployed: helmtime.Time{Time: now.Add(-StalePendingReleaseAge)},
		},
	}
	stale, age := isStalePendingRelease(rel, now)
	if !stale {
		t.Fatal("expected pending release at timeout to be stale")
	}
	if age != StalePendingReleaseAge {
		t.Fatalf("unexpected age: %s", age)
	}
}

func TestIsStalePendingReleaseSkipsFreshPendingRelease(t *testing.T) {
	now := time.Date(2026, 6, 17, 22, 0, 0, 0, time.UTC)

	rel := &helmrelease.Release{
		Info: &helmrelease.Info{
			Status:       helmrelease.StatusPendingUpgrade,
			LastDeployed: helmtime.Time{Time: now.Add(-StalePendingReleaseAge + time.Second)},
		},
	}
	stale, _ := isStalePendingRelease(rel, now)
	if stale {
		t.Fatal("expected fresh pending release not to be stale")
	}
}

func TestIsStalePendingReleaseSkipsDeployedRelease(t *testing.T) {
	now := time.Date(2026, 6, 17, 22, 0, 0, 0, time.UTC)

	rel := &helmrelease.Release{
		Info: &helmrelease.Info{
			Status:       helmrelease.StatusDeployed,
			LastDeployed: helmtime.Time{Time: now.Add(-StalePendingReleaseAge)},
		},
	}
	stale, _ := isStalePendingRelease(rel, now)
	if stale {
		t.Fatal("expected deployed release not to be stale")
	}
}

func volumeMountNames(mounts []corev1.VolumeMount) []string {
	names := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		names = append(names, mount.Name)
	}
	return names
}

func volumeNames(volumes []corev1.Volume) []string {
	names := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		names = append(names, volume.Name)
	}
	return names
}

// TestRemoveKernelHotfixDriftIgnoresTheHelmManagedByValue is the "before" half of
// the fix: the code used to gate on managed-by=Helm, a value the chart never
// applies, so hotfix drift was never cleaned up. A StatefulSet actually carrying
// that value is not ours and must still be left alone.
func TestRemoveKernelHotfixDriftIgnoresTheHelmManagedByValue(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding core scheme: %v", err)
	}

	st := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kernel",
			Namespace: "schematic",
			Labels:    map[string]string{helm.ManagedByLabel: "Helm"},
		},
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "kernel",
						Command: []string{"java"},
						Args:    []string{"-jar", "/hotfix/kernel-261.jar"},
					}},
				},
			},
		},
	}

	r := &Reconciler{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(st).Build(),
		log:    zap.NewNop(),
	}

	if err := r.removeKernelHotfixDrift(context.Background(), "schematic"); err != nil {
		t.Fatalf("removing hotfix drift: %v", err)
	}

	var got appsv1.StatefulSet
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "schematic", Name: "kernel"}, &got); err != nil {
		t.Fatalf("getting statefulset: %v", err)
	}
	if len(got.Spec.Template.Spec.Containers[0].Args) == 0 {
		t.Fatal("expected a foreign Helm-managed StatefulSet to be left unchanged")
	}
}

// TestPruneStaleDeploymentsSelectsTheChartsManagedByLabel covers both halves of
// the fix on one fixture set: the old selector (managed-by=Helm) matches nothing
// the chart creates, so pruning silently no-opped forever; the corrected
// selector finds the orphan and deletes it together with its Service, ConfigMap
// and Secret.
func TestPruneStaleDeploymentsSelectsTheChartsManagedByLabel(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding core scheme: %v", err)
	}

	chartLabels := map[string]string{
		helm.ManagedByLabel: helm.ManagedByValue,
		"helm.sh/chart":     "schematic-0.1.0",
	}
	deploy := func(name string, labels map[string]string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "schematic", Labels: labels},
		}
	}

	objs := []client.Object{
		// Orphaned by a topology scale-down: chart-labelled, no longer expected.
		deploy("gateway1", chartLabels),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gateway1", Namespace: "schematic"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gateway1-config", Namespace: "schematic"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gateway1-config", Namespace: "schematic"}},
		// Still expected.
		deploy("gateway0", chartLabels),
		// Not ours: a foreign release, and an unlabelled deployment.
		deploy("grafana", map[string]string{helm.ManagedByLabel: "Helm"}),
		deploy("adhoc", nil),
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	ctx := context.Background()

	// Before: the selector the code used to carry matches none of the chart's
	// own deployments, which is why pruning never ran.
	var oldSelectorMatches appsv1.DeploymentList
	if err := cl.List(ctx, &oldSelectorMatches, &client.ListOptions{
		Namespace:     "schematic",
		LabelSelector: labels.SelectorFromSet(labels.Set{helm.ManagedByLabel: "Helm"}),
	}); err != nil {
		t.Fatalf("listing with the old selector: %v", err)
	}
	for _, d := range oldSelectorMatches.Items {
		if d.Name == "gateway1" || d.Name == "gateway0" {
			t.Fatalf("old selector unexpectedly matched chart deployment %q", d.Name)
		}
	}

	r := &Reconciler{
		cfg: &protoOperator.Config{
			Namespace: "schematic",
			Topology:  &protoOperator.Topology{KernelNodes: 1, GatewayNodes: 1},
		},
		generator: helm.New("test", "1.2", nil, nil),
		client:    cl,
		log:       zap.NewNop(),
	}

	// After: the orphan is pruned along with its config objects.
	if err := r.pruneStaleDeployments(ctx, 1); err != nil {
		t.Fatalf("pruning stale deployments: %v", err)
	}

	var remaining appsv1.DeploymentList
	if err := cl.List(ctx, &remaining, &client.ListOptions{Namespace: "schematic"}); err != nil {
		t.Fatalf("listing deployments: %v", err)
	}
	got := map[string]bool{}
	for _, d := range remaining.Items {
		got[d.Name] = true
	}
	if got["gateway1"] {
		t.Fatal("expected stale gateway1 to be pruned")
	}
	for _, keep := range []string{"gateway0", "grafana", "adhoc"} {
		if !got[keep] {
			t.Fatalf("expected %q to survive pruning, remaining: %v", keep, got)
		}
	}

	for _, obj := range []struct {
		name string
		into client.Object
	}{
		{"service", &corev1.Service{}},
		{"configmap", &corev1.ConfigMap{}},
		{"secret", &corev1.Secret{}},
	} {
		name := "gateway1"
		if obj.name != "service" {
			name = "gateway1-config"
		}
		err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: "schematic"}, obj.into)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected gateway1 %s to be deleted, got err = %v", obj.name, err)
		}
	}
}

// fakeOpaPusher records every UpdateOpaConfig call and can be made to fail.
type fakeOpaPusher struct {
	calls []*protoOpa.Config
	err   error
}

func (f *fakeOpaPusher) UpdateOpaConfig(_ context.Context, req *protoKernel.UpdateOpaConfigRequest, _ ...grpc.CallOption) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, req.GetConfig().GetOpaConfig())
	return nil
}

func opaConfigWithEntryPoint(entryPoint string) *protoOpa.Config {
	return &protoOpa.Config{
		EntitlementsPolicy: &protoOpalib.OpaPolicy{EntryPoint: entryPoint},
	}
}

// TestPushOpaConfig covers the whole contract of the kernel push: it happens
// once per distinct config, it is skipped when there is no kernel identity yet,
// and a failed push is retried rather than swallowed.
func TestPushOpaConfig(t *testing.T) {
	ctx := context.Background()

	t.Run("pushes once per distinct config", func(t *testing.T) {
		pusher := &fakeOpaPusher{}
		r := &Reconciler{log: zap.NewNop(), opaPusher: pusher}

		first := opaConfigWithEntryPoint("data.entitlements.eval")
		r.pushOpaConfig(ctx, first)
		// An identical value on the next tick must not push again: the reconcile
		// loop runs every 30s and the config carries the whole rego bundle.
		r.pushOpaConfig(ctx, opaConfigWithEntryPoint("data.entitlements.eval"))
		r.pushOpaConfig(ctx, opaConfigWithEntryPoint("data.other.eval"))

		if len(pusher.calls) != 2 {
			t.Fatalf("got %d pushes, want 2", len(pusher.calls))
		}
		if got := pusher.calls[0].GetEntitlementsPolicy().GetEntryPoint(); got != "data.entitlements.eval" {
			t.Fatalf("first push entry point = %q", got)
		}
		if got := pusher.calls[1].GetEntitlementsPolicy().GetEntryPoint(); got != "data.other.eval" {
			t.Fatalf("second push entry point = %q", got)
		}
		if !proto.Equal(pusher.calls[0], first) {
			t.Fatalf("first push did not carry the config it was given")
		}
	})

	t.Run("no-ops without a config or a kernel identity", func(t *testing.T) {
		pusher := &fakeOpaPusher{}
		r := &Reconciler{log: zap.NewNop(), opaPusher: pusher}
		r.pushOpaConfig(ctx, nil)
		if len(pusher.calls) != 0 {
			t.Fatalf("nil config pushed %d times", len(pusher.calls))
		}

		// Until the ServiceManager bootstrap finishes there is no pusher, and
		// the reconcile must carry on regardless.
		noIdentity := &Reconciler{log: zap.NewNop()}
		noIdentity.pushOpaConfig(ctx, opaConfigWithEntryPoint("data.entitlements.eval"))
		if noIdentity.lastOpaPush != nil {
			t.Fatalf("recorded a push with no pusher installed")
		}
	})

	t.Run("retries after a failed push", func(t *testing.T) {
		pusher := &fakeOpaPusher{err: errors.New("kernel unavailable")}
		r := &Reconciler{log: zap.NewNop(), opaPusher: pusher}
		cfg := opaConfigWithEntryPoint("data.entitlements.eval")

		r.pushOpaConfig(ctx, cfg)
		if r.lastOpaPush != nil {
			t.Fatalf("a failed push must not be recorded as delivered")
		}

		pusher.err = nil
		r.pushOpaConfig(ctx, cfg)
		if len(pusher.calls) != 1 {
			t.Fatalf("got %d successful pushes, want 1", len(pusher.calls))
		}
	})
}

// TestSetOpaConfigPusher checks the seam the background ServiceManager
// bootstrap uses: a pusher installed after the reconciler is running is picked
// up by the next reconcile.
func TestSetOpaConfigPusher(t *testing.T) {
	r := &Reconciler{log: zap.NewNop()}
	pusher := &fakeOpaPusher{}
	r.SetOpaConfigPusher(pusher)
	r.pushOpaConfig(context.Background(), opaConfigWithEntryPoint("data.entitlements.eval"))
	if len(pusher.calls) != 1 {
		t.Fatalf("got %d pushes after SetOpaConfigPusher, want 1", len(pusher.calls))
	}
}
