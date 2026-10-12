package helm

import (
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/releaseutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
