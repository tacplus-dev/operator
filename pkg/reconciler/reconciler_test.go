package reconciler

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"go.uber.org/zap"
	"helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	helmrelease "helm.sh/helm/v3/pkg/release"
	helmstorage "helm.sh/helm/v3/pkg/storage"
	helmdriver "helm.sh/helm/v3/pkg/storage/driver"
	helmtime "helm.sh/helm/v3/pkg/time"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
	"gitlab.com/seabeam/schematic/platform/services/operator/pkg/helm"
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
		generator: helm.New("test", "1.2", nil, nil, nil),
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

func stalePendingReleaseStorage(t *testing.T, revisions ...*helmrelease.Release) *action.Configuration {
	t.Helper()

	store := helmstorage.Init(helmdriver.NewMemory())
	for _, rel := range revisions {
		if err := store.Create(rel); err != nil {
			t.Fatalf("seeding release revision %d: %v", rel.Version, err)
		}
	}

	return &action.Configuration{
		Releases:   store,
		KubeClient: &kubefake.PrintingKubeClient{Out: io.Discard},
		Log:        func(string, ...interface{}) {},
	}
}

func releaseRevision(version int, status helmrelease.Status, lastDeployed time.Time) *helmrelease.Release {
	return &helmrelease.Release{
		Name:      releaseName,
		Namespace: "default",
		Version:   version,
		Info: &helmrelease.Info{
			Status:       status,
			LastDeployed: helmtime.Time{Time: lastDeployed},
		},
		Chart: &helmchart.Chart{
			Metadata: &helmchart.Metadata{
				Name:       releaseName,
				Version:    "0.1.0",
				APIVersion: helmchart.APIVersionV2,
			},
		},
	}
}

// Helm marks the previous revision superseded as soon as an upgrade starts, so an
// upgrade that dies before the new revision reaches deployed leaves the release with
// no deployed revision at all. Recovery must still roll back to that superseded one.
func TestRecoverStalePendingReleaseWithoutDeployedRevision(t *testing.T) {
	stale := time.Now().Add(-StalePendingReleaseAge - time.Minute)
	cfg := stalePendingReleaseStorage(t,
		releaseRevision(1, helmrelease.StatusSuperseded, stale),
		releaseRevision(2, helmrelease.StatusSuperseded, stale),
		releaseRevision(3, helmrelease.StatusPendingUpgrade, stale),
	)

	latest, err := cfg.Releases.Last(releaseName)
	if err != nil {
		t.Fatalf("reading latest revision: %v", err)
	}

	r := &Reconciler{log: zap.NewNop()}
	recovered, err := r.recoverStalePendingRelease(cfg, latest)
	if err != nil {
		t.Fatalf("recovering stale pending release: %v", err)
	}
	if !recovered {
		t.Fatal("expected stale pending release to be recovered")
	}

	deployed, err := cfg.Releases.Deployed(releaseName)
	if err != nil {
		t.Fatalf("reading deployed revision after recovery: %v", err)
	}
	if deployed.Version != 4 {
		t.Fatalf("expected rollback to create revision 4, got %d", deployed.Version)
	}
}

// A first install that crashed mid-flight leaves a pending release with no prior
// revision of any status. There is nothing to roll back to, so recovery uninstalls the
// stuck release and lets the next reconcile install from scratch.
func TestRecoverStalePendingReleaseWithoutAnyPriorRevision(t *testing.T) {
	stale := time.Now().Add(-StalePendingReleaseAge - time.Minute)
	cfg := stalePendingReleaseStorage(t,
		releaseRevision(1, helmrelease.StatusPendingInstall, stale),
	)

	latest, err := cfg.Releases.Last(releaseName)
	if err != nil {
		t.Fatalf("reading latest revision: %v", err)
	}

	r := &Reconciler{log: zap.NewNop()}
	recovered, err := r.recoverStalePendingRelease(cfg, latest)
	if err != nil {
		t.Fatalf("recovering stale pending release with no history: %v", err)
	}
	if !recovered {
		t.Fatal("expected stale pending release to be recovered")
	}

	remaining, err := cfg.Releases.Last(releaseName)
	if err == nil && remaining.Info.Status != helmrelease.StatusUninstalled {
		t.Fatalf("expected the stuck release to be uninstalled, got revision %d %s", remaining.Version, remaining.Info.Status)
	}
}

// The only predecessor being failed rather than superseded (an install that failed, then
// an upgrade attempt that also died) must still be a valid rollback target.
func TestRecoverStalePendingReleaseWithFailedPredecessor(t *testing.T) {
	stale := time.Now().Add(-StalePendingReleaseAge - time.Minute)
	cfg := stalePendingReleaseStorage(t,
		releaseRevision(1, helmrelease.StatusFailed, stale),
		releaseRevision(2, helmrelease.StatusPendingUpgrade, stale),
	)

	latest, err := cfg.Releases.Last(releaseName)
	if err != nil {
		t.Fatalf("reading latest revision: %v", err)
	}

	target, err := rollbackTargetRevision(cfg, latest)
	if err != nil {
		t.Fatalf("picking rollback target with a failed predecessor: %v", err)
	}
	if target.Version != 1 {
		t.Fatalf("expected failed revision 1 to be the rollback target, got %d", target.Version)
	}

	r := &Reconciler{log: zap.NewNop()}
	recovered, err := r.recoverStalePendingRelease(cfg, latest)
	if err != nil {
		t.Fatalf("recovering stale pending release: %v", err)
	}
	if !recovered {
		t.Fatal("expected stale pending release to be recovered")
	}

	deployed, err := cfg.Releases.Deployed(releaseName)
	if err != nil {
		t.Fatalf("reading deployed revision after recovery: %v", err)
	}
	if deployed.Version != 3 {
		t.Fatalf("expected rollback to create revision 3, got %d", deployed.Version)
	}
}

// Newest-first candidate selection still holds once failed revisions are eligible.
func TestRollbackTargetRevisionPrefersNewestCandidate(t *testing.T) {
	stale := time.Now().Add(-StalePendingReleaseAge - time.Minute)
	cfg := stalePendingReleaseStorage(t,
		releaseRevision(1, helmrelease.StatusSuperseded, stale),
		releaseRevision(2, helmrelease.StatusFailed, stale),
		releaseRevision(3, helmrelease.StatusPendingUpgrade, stale),
	)

	latest, err := cfg.Releases.Last(releaseName)
	if err != nil {
		t.Fatalf("reading latest revision: %v", err)
	}

	target, err := rollbackTargetRevision(cfg, latest)
	if err != nil {
		t.Fatalf("picking rollback target: %v", err)
	}
	if target.Version != 2 {
		t.Fatalf("expected revision 2 to be the rollback target, got %d", target.Version)
	}
}

// With the stuck pending revision as the only history entry there is no rollback target,
// and that must be reported distinguishably from a history-query failure.
func TestRollbackTargetRevisionWithoutAnyPriorRevision(t *testing.T) {
	stale := time.Now().Add(-StalePendingReleaseAge - time.Minute)
	cfg := stalePendingReleaseStorage(t,
		releaseRevision(1, helmrelease.StatusPendingInstall, stale),
	)

	latest, err := cfg.Releases.Last(releaseName)
	if err != nil {
		t.Fatalf("reading latest revision: %v", err)
	}

	if _, err := rollbackTargetRevision(cfg, latest); !errors.Is(err, errNoRollbackTarget) {
		t.Fatalf("rollbackTargetRevision error = %v, want errNoRollbackTarget", err)
	}
}

func TestRollbackTargetRevisionPrefersDeployedRevision(t *testing.T) {
	stale := time.Now().Add(-StalePendingReleaseAge - time.Minute)
	cfg := stalePendingReleaseStorage(t,
		releaseRevision(1, helmrelease.StatusSuperseded, stale),
		releaseRevision(2, helmrelease.StatusDeployed, stale),
		releaseRevision(3, helmrelease.StatusPendingUpgrade, stale),
	)

	latest, err := cfg.Releases.Last(releaseName)
	if err != nil {
		t.Fatalf("reading latest revision: %v", err)
	}

	target, err := rollbackTargetRevision(cfg, latest)
	if err != nil {
		t.Fatalf("picking rollback target: %v", err)
	}
	if target.Version != 2 {
		t.Fatalf("expected deployed revision 2 to be the rollback target, got %d", target.Version)
	}
}

// immutableTestScheme is the scheme the immutable-field tests need: StatefulSets
// and PVCs, plus the cluster-scoped StorageClass the PVC-expansion path reads.
func immutableTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding core scheme: %v", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding storage scheme: %v", err)
	}
	return scheme
}

// immutableTestChart is a one-StatefulSet stand-in for the generated chart,
// shaped like the kernel StatefulSet the generator emits: headless serviceName,
// OrderedReady pod management, an app selector and a single "data"
// volumeClaimTemplate whose size comes from values.
func immutableTestChart(t *testing.T) *helmchart.Chart {
	t.Helper()
	ch, err := helmloader.LoadFiles([]*helmloader.BufferedFile{
		{Name: "Chart.yaml", Data: []byte("apiVersion: v2\nname: schematic\nversion: 0.1.0\n")},
		{Name: "values.yaml", Data: []byte("global:\n  namespace: schematic\nkernel:\n  storage: 2Gi\n  serviceName: kernel\n")},
		{Name: "templates/kernel/statefulset.yaml", Data: []byte(`apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: kernel
  namespace: {{ .Values.global.namespace }}
  labels:
    {{ ` + "`" + `app.kubernetes.io/managed-by` + "`" + ` }}: schematic-operator
spec:
  serviceName: {{ .Values.kernel.serviceName }}
  replicas: 1
  podManagementPolicy: OrderedReady
  selector:
    matchLabels:
      app: kernel
  template:
    metadata:
      labels:
        app: kernel
    spec:
      containers:
      - name: kernel
        image: kernel:test
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: [ReadWriteOnce]
      resources:
        requests:
          storage: {{ .Values.kernel.storage }}
`)},
	})
	if err != nil {
		t.Fatalf("loading test chart: %v", err)
	}
	return ch
}

// immutableTestStatefulSet is the live kernel StatefulSet as the chart last
// created it: 2Gi, and carrying the server-side defaults (storageClassName,
// volumeMode) a rendered manifest never has.
//
// Its metadata is what Helm leaves on the object, not what the chart renders:
// Helm stamps its release annotations and overwrites the chart's managed-by
// label with "Helm" on every resource it installs or upgrades (checked on the
// live kernel StatefulSet, 2026-09-28).
func immutableTestStatefulSet() *appsv1.StatefulSet {
	sc := "standard"
	volumeMode := corev1.PersistentVolumeFilesystem
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kernel",
			Namespace: "schematic",
			Labels:    map[string]string{helm.ManagedByLabel: "Helm"},
			Annotations: map[string]string{
				"meta.helm.sh/release-name":      "schematic",
				"meta.helm.sh/release-namespace": "schematic",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:         "kernel",
			PodManagementPolicy: appsv1.OrderedReadyPodManagement,
			Selector:            &metav1.LabelSelector{MatchLabels: map[string]string{"app": "kernel"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "kernel"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "kernel", Image: "kernel:test"}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: &sc,
					VolumeMode:       &volumeMode,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
					},
				},
			}},
		},
	}
}

func immutableTestPVC(size string) *corev1.PersistentVolumeClaim {
	sc := "standard"
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-kernel-0",
			Namespace: "schematic",
			// The StatefulSet controller stamps its selector's labels onto the
			// PVCs it creates from a volumeClaimTemplate.
			Labels: map[string]string{"app": "kernel"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
}

func immutableTestStorageClass(allowExpansion bool) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: "standard"},
		Provisioner:          "test",
		AllowVolumeExpansion: &allowExpansion,
	}
}

// TestReconcileImmutableStatefulSetFieldsExpandsGrownPVC is the failure Jody hit:
// a 2Gi→8Gi bump changes volumeClaimTemplates, which Kubernetes refuses to patch
// on a live StatefulSet, and the whole combined release dies at kernel. The size
// grow alone is serviceable without recreating anything: expand the live PVC and
// leave the StatefulSet — and therefore the rest of the upgrade — alone.
// TestReconcileImmutableStatefulSetFieldsExpandsGrownPVC: a pure storage grow
// expands the live PVC in place FIRST, then still orphan-deletes the
// StatefulSet — its own template still names the old size, so leaving it alive
// would make the very next Helm upgrade hit the identical forbidden-patch
// rejection Jody's roll actually hit. Expanding before deleting means the
// recreated StatefulSet re-adopts a PVC that is already the right size.
func TestReconcileImmutableStatefulSetFieldsExpandsGrownPVC(t *testing.T) {
	scheme := immutableTestScheme(t)
	ctx := context.Background()

	var deletePolicy *metav1.DeletionPropagation
	var pvcSizeAtDelete string
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		immutableTestStatefulSet(),
		immutableTestPVC("2Gi"),
		immutableTestStorageClass(true),
	).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			var pvc corev1.PersistentVolumeClaim
			if err := c.Get(ctx, types.NamespacedName{Namespace: "schematic", Name: "data-kernel-0"}, &pvc); err == nil {
				size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
				pvcSizeAtDelete = size.String()
			}
			var o client.DeleteOptions
			for _, opt := range opts {
				opt.ApplyToDelete(&o)
			}
			deletePolicy = o.PropagationPolicy
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()

	r := &Reconciler{client: cl, log: zap.NewNop()}

	vals := map[string]interface{}{"kernel": map[string]interface{}{"storage": "8Gi"}}
	if err := r.reconcileImmutableStatefulSetFields(ctx, "schematic", immutableTestChart(t), vals); err != nil {
		t.Fatalf("reconciling immutable fields: %v", err)
	}

	var pvc corev1.PersistentVolumeClaim
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: "schematic", Name: "data-kernel-0"}, &pvc); err != nil {
		t.Fatalf("getting pvc: %v", err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "8Gi" {
		t.Fatalf("expected pvc grown to 8Gi, got %s", got.String())
	}
	if pvcSizeAtDelete != "8Gi" {
		t.Fatalf("expected pvc already expanded to 8Gi before the statefulset delete, got %s", pvcSizeAtDelete)
	}

	var st appsv1.StatefulSet
	err := r.client.Get(ctx, types.NamespacedName{Namespace: "schematic", Name: "kernel"}, &st)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected statefulset to be orphan-deleted after the pvc grow, got err=%v", err)
	}
	if deletePolicy == nil || *deletePolicy != metav1.DeletePropagationOrphan {
		t.Fatalf("expected an orphan delete so pods keep running, got %v", deletePolicy)
	}
}

// TestReconcileImmutableStatefulSetFieldsSkipsPVCWhoseStorageClassForbidsExpansion:
// a volume that cannot be grown is one volume's problem, not a reason to error
// the reconcile and leave the rest of the fleet un-rolled.
// TestReconcileImmutableStatefulSetFieldsSkipsPVCWhoseStorageClassForbidsExpansion:
// a volume that cannot be grown is one volume's problem, not a reason to error
// the reconcile and leave the rest of the fleet un-rolled. The StatefulSet is
// still orphan-deleted — its template disagrees with the chart regardless of
// whether the PVC itself could be resized, and leaving it live would still
// make the next Helm upgrade fail the same way — but the PVC is left at its
// old size since nothing can grow it.
func TestReconcileImmutableStatefulSetFieldsSkipsPVCWhoseStorageClassForbidsExpansion(t *testing.T) {
	scheme := immutableTestScheme(t)
	ctx := context.Background()

	r := &Reconciler{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			immutableTestStatefulSet(),
			immutableTestPVC("2Gi"),
			immutableTestStorageClass(false),
		).Build(),
		log: zap.NewNop(),
	}

	vals := map[string]interface{}{"kernel": map[string]interface{}{"storage": "8Gi"}}
	if err := r.reconcileImmutableStatefulSetFields(ctx, "schematic", immutableTestChart(t), vals); err != nil {
		t.Fatalf("a non-expandable StorageClass must not fail the reconcile: %v", err)
	}

	var pvc corev1.PersistentVolumeClaim
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: "schematic", Name: "data-kernel-0"}, &pvc); err != nil {
		t.Fatalf("getting pvc: %v", err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "2Gi" {
		t.Fatalf("expected pvc left at 2Gi (storage class forbids expansion), got %s", got.String())
	}
	var st appsv1.StatefulSet
	err := r.client.Get(ctx, types.NamespacedName{Namespace: "schematic", Name: "kernel"}, &st)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected statefulset to be orphan-deleted even though its pvc could not grow, got err=%v", err)
	}
}

// TestReconcileImmutableStatefulSetFieldsOrphanDeletesOnForbiddenFieldChange:
// an immutable change that is not a size grow can only be applied by recreating
// the StatefulSet, which must happen with the pods orphaned so the running fleet
// is untouched.
func TestReconcileImmutableStatefulSetFieldsOrphanDeletesOnForbiddenFieldChange(t *testing.T) {
	scheme := immutableTestScheme(t)
	ctx := context.Background()

	var deletePolicy *metav1.DeletionPropagation
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		immutableTestStatefulSet(),
		immutableTestPVC("2Gi"),
		immutableTestStorageClass(true),
	).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			var o client.DeleteOptions
			for _, opt := range opts {
				opt.ApplyToDelete(&o)
			}
			deletePolicy = o.PropagationPolicy
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()

	r := &Reconciler{client: cl, log: zap.NewNop()}

	// The chart now puts kernel behind a different headless Service — an
	// immutable field with no in-place fix.
	vals := map[string]interface{}{"kernel": map[string]interface{}{"serviceName": "kernel-headless"}}
	if err := r.reconcileImmutableStatefulSetFields(ctx, "schematic", immutableTestChart(t), vals); err != nil {
		t.Fatalf("reconciling immutable fields: %v", err)
	}

	var st appsv1.StatefulSet
	err := r.client.Get(ctx, types.NamespacedName{Namespace: "schematic", Name: "kernel"}, &st)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected statefulset to be deleted, got err=%v", err)
	}
	if deletePolicy == nil || *deletePolicy != metav1.DeletePropagationOrphan {
		t.Fatalf("expected an orphan delete so pods keep running, got %v", deletePolicy)
	}
}

// TestReconcileImmutableStatefulSetFieldsLeavesUnchangedStatefulSetAlone guards
// the loop: a live StatefulSet carries server-side defaults the rendered
// manifest omits, and reading those as drift would orphan-delete the fleet on
// every 30s reconcile.
func TestReconcileImmutableStatefulSetFieldsLeavesUnchangedStatefulSetAlone(t *testing.T) {
	scheme := immutableTestScheme(t)
	ctx := context.Background()

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		immutableTestStatefulSet(),
		immutableTestPVC("2Gi"),
		immutableTestStorageClass(true),
	).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			t.Fatalf("unexpected delete of %s", obj.GetName())
			return nil
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			t.Fatalf("unexpected patch of %s", obj.GetName())
			return nil
		},
	}).Build()

	r := &Reconciler{client: cl, log: zap.NewNop()}

	// Chart default values: the same 2Gi and serviceName the live object has.
	if err := r.reconcileImmutableStatefulSetFields(ctx, "schematic", immutableTestChart(t), map[string]interface{}{}); err != nil {
		t.Fatalf("reconciling immutable fields: %v", err)
	}
}

// TestReconcileImmutableStatefulSetFieldsSkipsForeignStatefulSet: Helm's
// release annotations are the only thing establishing ownership, and deleting
// somebody else's StatefulSet would be a much worse outage than the one this
// code prevents. A same-named StatefulSet of another Helm release is not ours.
func TestReconcileImmutableStatefulSetFieldsSkipsForeignStatefulSet(t *testing.T) {
	scheme := immutableTestScheme(t)
	ctx := context.Background()

	foreign := immutableTestStatefulSet()
	foreign.Annotations = map[string]string{
		"meta.helm.sh/release-name":      "other",
		"meta.helm.sh/release-namespace": "schematic",
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		foreign,
		immutableTestPVC("2Gi"),
		immutableTestStorageClass(true),
	).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			t.Fatalf("unexpected delete of foreign %s", obj.GetName())
			return nil
		},
	}).Build()

	r := &Reconciler{client: cl, log: zap.NewNop()}

	vals := map[string]interface{}{"kernel": map[string]interface{}{"serviceName": "kernel-headless"}}
	if err := r.reconcileImmutableStatefulSetFields(ctx, "schematic", immutableTestChart(t), vals); err != nil {
		t.Fatalf("reconciling immutable fields: %v", err)
	}

	var st appsv1.StatefulSet
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: "schematic", Name: "kernel"}, &st); err != nil {
		t.Fatalf("expected foreign statefulset to be left alone: %v", err)
	}
}

// TestImmutableStatefulSetDiffAgainstTheGeneratedChart runs the comparison
// against what the generator actually emits, not a hand-written stand-in. It
// pins the two things that matter in production:
//
//   - A live StatefulSet that already matches the chart reports no drift. The
//     rendered manifest omits server-side defaults (storageClassName, volumeMode)
//     and spells an empty selector matchExpressions where the stored object has
//     none; reading either as drift would orphan-delete the fleet every 30s.
//   - a live PVC smaller than the chart's is classified as a PVC expansion, so
//     the change Jody hit is serviced here and never reaches helm upgrade.
func TestImmutableStatefulSetDiffAgainstTheGeneratedChart(t *testing.T) {
	rendered, err := renderStatefulSets(generatedTestChart(t), "schematic", map[string]interface{}{})
	if err != nil {
		t.Fatalf("rendering generated chart: %v", err)
	}
	desired := rendered["kernel"]
	if desired == nil {
		t.Fatal("generated chart rendered no kernel StatefulSet")
	}

	// In sync: the live PVC already matches kernel's chart size.
	live := immutableTestStatefulSet()
	if fields, grown := immutableStatefulSetDiff(live, desired); len(fields) != 0 || len(grown) != 0 {
		t.Fatalf("expected no drift against the generated chart, got fields=%v grown=%v", fields, grown)
	}

	// The production failure: live is still on a smaller size than the chart.
	live = immutableTestStatefulSet()
	live.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("1Gi")
	fields, grown := immutableStatefulSetDiff(live, desired)
	if len(fields) != 0 {
		t.Fatalf("a pure size grow must not be a forbidden-field change, got %v", fields)
	}
	if got, ok := grown["data"]; !ok || got.String() != "2Gi" {
		t.Fatalf("expected the data template recorded as grown to 2Gi, got %v", grown)
	}

	// The org-rename roll: live was created at 8Gi by f980a22's chart, and the
	// chart has since been reverted to 2Gi. A PVC cannot shrink, so this is a
	// forbidden change that only a recreate can clear, never a PVC expansion.
	live = immutableTestStatefulSet()
	live.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("8Gi")
	fields, grown = immutableStatefulSetDiff(live, desired)
	if !slices.Equal(fields, []string{"spec.volumeClaimTemplates[data].spec.resources.requests.storage"}) || len(grown) != 0 {
		t.Fatalf("expected a storage shrink reported as forbidden, got fields=%v grown=%v", fields, grown)
	}

	// Kernel moved from Parallel to OrderedReady pod management, which
	// Kubernetes will not patch either.
	live = immutableTestStatefulSet()
	live.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
	fields, grown = immutableStatefulSetDiff(live, desired)
	if !slices.Equal(fields, []string{"spec.podManagementPolicy"}) || len(grown) != 0 {
		t.Fatalf("expected the Parallel kernel reported as a forbidden podManagementPolicy change, got fields=%v grown=%v", fields, grown)
	}
}

// TestReconcileImmutableStatefulSetFieldsRecreatesTheLiveKernel is the
// org-rename roll (2026-09-28) end to end against the generated chart: the live
// kernel StatefulSet, as Helm left it in the cluster, had an 8Gi template and
// Parallel pod management; the chart says 2Gi and OrderedReady. Helm's upgrade
// was rejected on kernel because this path never saw the object — it selected
// on the chart's managed-by label, which Helm overwrites with "Helm". The live
// object must be orphan-deleted so the upgrade can recreate it, and the 8Gi
// PVCs, which cannot shrink, must not be touched.
func TestReconcileImmutableStatefulSetFieldsRecreatesTheLiveKernel(t *testing.T) {
	scheme := immutableTestScheme(t)
	ctx := context.Background()
	ch := generatedTestChart(t)

	rendered, err := renderStatefulSets(ch, "schematic", map[string]interface{}{})
	if err != nil {
		t.Fatalf("rendering generated chart: %v", err)
	}
	live := rendered["kernel"].DeepCopy()
	live.Labels[helm.ManagedByLabel] = "Helm"
	live.Annotations = map[string]string{
		"meta.helm.sh/release-name":      "schematic",
		"meta.helm.sh/release-namespace": "schematic",
	}
	live.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
	live.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("8Gi")

	var deletePolicy *metav1.DeletionPropagation
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		live,
		immutableTestPVC("8Gi"),
		immutableTestStorageClass(true),
	).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			var o client.DeleteOptions
			for _, opt := range opts {
				opt.ApplyToDelete(&o)
			}
			deletePolicy = o.PropagationPolicy
			return c.Delete(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			t.Fatalf("unexpected patch of %s: a PVC cannot shrink", obj.GetName())
			return nil
		},
	}).Build()

	r := &Reconciler{client: cl, log: zap.NewNop()}
	if err := r.reconcileImmutableStatefulSetFields(ctx, "schematic", ch, map[string]interface{}{}); err != nil {
		t.Fatalf("reconciling immutable fields: %v", err)
	}

	var st appsv1.StatefulSet
	err = r.client.Get(ctx, types.NamespacedName{Namespace: "schematic", Name: "kernel"}, &st)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected the live kernel statefulset to be deleted, got err=%v", err)
	}
	if deletePolicy == nil || *deletePolicy != metav1.DeletePropagationOrphan {
		t.Fatalf("expected an orphan delete so kernel pods keep running, got %v", deletePolicy)
	}
}

// generatedTestChart loads the chart the generator actually emits for a
// one-kernel, one-stratum topology.
func generatedTestChart(t *testing.T) *helmchart.Chart {
	t.Helper()
	charts, err := helm.New("test", "1.2", nil, nil, nil).Generate(&protoOperator.Config{
		Namespace: "schematic",
		Topology:  &protoOperator.Topology{KernelNodes: 1, StratumNodes: 1},
	}, 1, false)
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
