package helm

import (
	"sort"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/releaseutil"
	appsv1 "k8s.io/api/apps/v1"
	k8syaml "sigs.k8s.io/yaml"
)

// operatorPodEnvAllowed is the env the operator's own pod may carry: none of it
// configuration (#17). KERNEL_K8S_TOKEN_FILE stays only until the chassis
// reads its token path from config (tacplus-dev/tacplus#262), and then goes.
var operatorPodEnvAllowed = map[string]string{
	"KERNEL_K8S_TOKEN_FILE": "tacplus-dev/tacplus#262",
}

// The operator's own chart (helm/) gives the operator exactly one input, the
// uni-config Secret the deployment's pipeline applies (#17): no ConfigMap or
// Secret of config rendered from helm values, no config bundle, and no
// configuration in the pod's environment.
func TestOperatorChartReadsOnlyTheUniConfigSecret(t *testing.T) {
	ch, err := helmloader.Load("../../helm")
	if err != nil {
		t.Fatalf("loading operator chart: %v", err)
	}
	install := action.NewInstall(new(action.Configuration))
	install.ReleaseName = "schematic-operator"
	install.Namespace = "schematic"
	install.DryRun = true
	install.ClientOnly = true
	rel, err := install.Run(ch, map[string]interface{}{})
	if err != nil {
		t.Fatalf("helm dry-run render: %v", err)
	}

	var dep appsv1.Deployment
	var kinds []string
	for _, doc := range releaseutil.SplitManifests(rel.Manifest) {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := k8syaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, meta.Kind+"/"+meta.Metadata.Name)
		if meta.Kind == "Deployment" {
			if err := k8syaml.Unmarshal([]byte(doc), &dep); err != nil {
				t.Fatal(err)
			}
		}
		if meta.Kind == "ConfigMap" || meta.Kind == "Secret" {
			t.Errorf("the chart renders %s/%s: the operator's config and secrets come only from the uni-config Secret", meta.Kind, meta.Metadata.Name)
		}
	}
	sort.Strings(kinds)
	if dep.Name == "" {
		t.Fatalf("no Deployment rendered: %v", kinds)
	}

	pod := dep.Spec.Template.Spec
	if len(pod.InitContainers) != 0 {
		t.Errorf("the operator pod has init containers %v; nothing is unpacked into it", pod.InitContainers)
	}
	var uniConfigVolume string
	for _, v := range pod.Volumes {
		switch {
		case v.Secret != nil && v.Secret.SecretName == "uni-config":
			uniConfigVolume = v.Name
		case v.Secret != nil:
			t.Errorf("volume %s mounts Secret %s; the uni-config is the only one", v.Name, v.Secret.SecretName)
		case v.ConfigMap != nil:
			t.Errorf("volume %s mounts ConfigMap %s; the uni-config is the operator's only input", v.Name, v.ConfigMap.Name)
		}
	}
	if uniConfigVolume == "" {
		t.Fatalf("the operator pod does not mount the uni-config Secret: %+v", pod.Volumes)
	}

	if len(pod.Containers) != 1 {
		t.Fatalf("operator pod has %d containers, want 1", len(pod.Containers))
	}
	c := pod.Containers[0]
	var mountPath string
	for _, m := range c.VolumeMounts {
		if m.Name == uniConfigVolume {
			mountPath = m.MountPath
		}
	}
	if want := "--uni-config=" + mountPath + "/uni-config"; mountPath == "" || !contains(c.Args, want) {
		t.Errorf("operator args %v do not read the mounted uni-config (%s)", c.Args, want)
	}
	for _, e := range c.Env {
		if _, ok := operatorPodEnvAllowed[e.Name]; !ok {
			t.Errorf("the operator pod sets env %s; configuration comes only from the uni-config", e.Name)
		}
	}
	for _, e := range c.EnvFrom {
		t.Errorf("the operator pod takes env from %+v; configuration comes only from the uni-config", e)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if strings.TrimSpace(v) == s {
			return true
		}
	}
	return false
}
