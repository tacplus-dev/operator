package helm

import (
	"slices"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/releaseutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8syaml "sigs.k8s.io/yaml"
)

// The operator's own chart (helm/) must deliver images.yaml to the in-cluster
// operator. LoadConfig merges image_digests from <config dir>/images.yaml, but a
// chart that never puts the file there leaves every image on its mutable tag,
// and a new build pushed under :local rolls nothing. A changed manifest must
// also roll the operator itself, or it keeps reconciling the old digests.
func TestOperatorChartDeliversImagesYAML(t *testing.T) {
	ch, err := helmloader.Load("../../helm")
	if err != nil {
		t.Fatalf("loading operator chart: %v", err)
	}
	render := func(images string) (corev1.ConfigMap, appsv1.Deployment) {
		install := action.NewInstall(new(action.Configuration))
		install.ReleaseName = "schematic-operator"
		install.Namespace = "schematic"
		install.DryRun = true
		install.ClientOnly = true
		rel, err := install.Run(ch, map[string]interface{}{
			"config": map[string]interface{}{
				"settings": "namespace: schematic\n",
				"crypto":   "pki: {}\n",
				"bundle":   "c2VydmljZXMK",
				"images":   images,
			},
		})
		if err != nil {
			t.Fatalf("helm dry-run render: %v", err)
		}
		var cm corev1.ConfigMap
		var dep appsv1.Deployment
		for _, doc := range releaseutil.SplitManifests(rel.Manifest) {
			switch {
			case strings.Contains(doc, "kind: ConfigMap") && strings.Contains(doc, "name: schematic-operator-config"):
				if err := k8syaml.Unmarshal([]byte(doc), &cm); err != nil {
					t.Fatal(err)
				}
			case strings.Contains(doc, "kind: Deployment"):
				if err := k8syaml.Unmarshal([]byte(doc), &dep); err != nil {
					t.Fatal(err)
				}
			}
		}
		return cm, dep
	}

	first := "image_digests:\n  kernel: sha256:" + strings.Repeat("11", 32) + "\n"
	second := "image_digests:\n  kernel: sha256:" + strings.Repeat("22", 32) + "\n"
	cm, dep := render(first)

	if got := cm.Data["images.yaml"]; got != first {
		t.Errorf("schematic-operator-config images.yaml = %q, want %q", got, first)
	}
	var mounted bool
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name == "config" && m.MountPath == "/config/images.yaml" && m.SubPath == "images.yaml" {
				mounted = true
			}
		}
	}
	if !mounted {
		t.Errorf("operator container does not mount images.yaml at /config/images.yaml: %+v", dep.Spec.Template.Spec.Containers[0].VolumeMounts)
	}

	_, depSecond := render(second)
	if dep.Spec.Template.Annotations["checksum/config-images"] == depSecond.Spec.Template.Annotations["checksum/config-images"] {
		t.Error("a new images.yaml left the operator pod template unchanged: the operator keeps deploying the old digests")
	}
}

// renderOperatorChart renders the operator's own chart with vals and returns
// its objects' kind, name and namespace, and its Deployment.
func renderOperatorChart(t *testing.T, vals map[string]interface{}) ([]metav1.PartialObjectMetadata, appsv1.Deployment) {
	t.Helper()
	ch, err := helmloader.Load("../../helm")
	if err != nil {
		t.Fatalf("loading operator chart: %v", err)
	}
	install := action.NewInstall(new(action.Configuration))
	install.ReleaseName = "schematic-operator"
	install.Namespace = "schematic"
	install.DryRun = true
	install.ClientOnly = true
	rel, err := install.Run(ch, vals)
	if err != nil {
		t.Fatalf("helm dry-run render: %v", err)
	}
	var objs []metav1.PartialObjectMetadata
	var dep appsv1.Deployment
	for _, doc := range releaseutil.SplitManifests(rel.Manifest) {
		var obj metav1.PartialObjectMetadata
		if err := k8syaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatal(err)
		}
		if obj.Kind == "" {
			continue
		}
		objs = append(objs, obj)
		if obj.Kind == "Deployment" {
			if err := k8syaml.Unmarshal([]byte(doc), &dep); err != nil {
				t.Fatal(err)
			}
		}
	}
	return objs, dep
}

func operatorChartKinds(objs []metav1.PartialObjectMetadata) []string {
	var kinds []string
	for _, obj := range objs {
		if !slices.Contains(kinds, obj.Kind) {
			kinds = append(kinds, obj.Kind)
		}
	}
	slices.Sort(kinds)
	return kinds
}

// TestOperatorChartForADeployment: with operator.deployment set the operator
// runs in tac-<deployment> as the ServiceAccount operator, which
// deploymentproxy's Role binds by name, and the chart holds nothing the
// pipelines own: no Namespace, nothing cluster-scoped, no Role or RoleBinding.
// Without it the chart renders what it always has.
func TestOperatorChartForADeployment(t *testing.T) {
	objs, dep := renderOperatorChart(t, map[string]interface{}{
		"operator": map[string]interface{}{"deployment": "prod", "managedNamespaces": []interface{}{"other"}},
	})
	for _, obj := range objs {
		if obj.Namespace != "tac-prod" {
			t.Errorf("%s %s in namespace %q, want tac-prod", obj.Kind, obj.Name, obj.Namespace)
		}
		switch obj.Kind {
		case "Namespace", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding":
			t.Errorf("chart renders %s %s for a deployment", obj.Kind, obj.Name)
		case "ServiceAccount":
			if obj.Name != "operator" {
				t.Errorf("ServiceAccount is %s, want operator", obj.Name)
			}
		}
	}
	if want := []string{"ConfigMap", "Deployment", "Secret", "ServiceAccount"}; !slices.Equal(operatorChartKinds(objs), want) {
		t.Errorf("deployment render kinds = %v, want %v", operatorChartKinds(objs), want)
	}
	if got := dep.Spec.Template.Spec.ServiceAccountName; got != "operator" {
		t.Errorf("Deployment runs as %q, want operator", got)
	}

	objs, dep = renderOperatorChart(t, map[string]interface{}{})
	if want := []string{"ClusterRole", "ClusterRoleBinding", "ConfigMap", "Deployment", "Namespace", "Role", "RoleBinding", "Secret", "ServiceAccount"}; !slices.Equal(operatorChartKinds(objs), want) {
		t.Errorf("default render kinds = %v, want %v", operatorChartKinds(objs), want)
	}
	for _, obj := range objs {
		if obj.Namespace != "" && obj.Namespace != "schematic" {
			t.Errorf("default render: %s %s in namespace %q, want schematic", obj.Kind, obj.Name, obj.Namespace)
		}
	}
	if got := dep.Spec.Template.Spec.ServiceAccountName; got != "schematic-operator" {
		t.Errorf("default render: Deployment runs as %q, want schematic-operator", got)
	}
}
