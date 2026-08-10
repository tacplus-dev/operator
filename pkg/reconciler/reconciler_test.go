package reconciler

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	helmrelease "helm.sh/helm/v3/pkg/release"
	helmtime "helm.sh/helm/v3/pkg/time"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
				"app.kubernetes.io/managed-by": "Helm",
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
