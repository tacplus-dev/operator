package helm

import (
	"reflect"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/releaseutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8syaml "sigs.k8s.io/yaml"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// harnessTestConfig is a dev-shaped fleet with the harness and its destructive
// profile enabled: three kernels and the blobstore pair, the pods the chaos
// Role names.
func harnessTestConfig(destructive bool) *protoOperator.Config {
	return &protoOperator.Config{
		Namespace: "schematic",
		Fqdn:      "dev.seabeam.net",
		Topology: &protoOperator.Topology{
			KernelNodes:    3,
			KmakerNodes:    1,
			BlobstoreNodes: 2,
		},
		OptionalServices: &protoOperator.OptionalServices{
			Blobstore:          true,
			Harness:            true,
			HarnessDestructive: destructive,
		},
		ImageDigests: map[string]string{
			"kernel":  "sha256:" + strings.Repeat("ab", 32),
			"gateway": "sha256:" + strings.Repeat("cd", 32),
		},
	}
}

// renderedManifests renders the chart through helm, exactly as the
// reconciler's dry-run does, and returns each object's manifest keyed by
// "<Kind>/<name>". The harness assertions are about objects, not template text,
// so they read what Kubernetes would receive.
func renderedManifests(t *testing.T, charts *Charts) map[string]string {
	t.Helper()
	buffered := make([]*helmloader.BufferedFile, 0, len(charts.Files))
	for name, data := range charts.Files {
		buffered = append(buffered, &helmloader.BufferedFile{Name: name, Data: data})
	}
	ch, err := helmloader.LoadFiles(buffered)
	if err != nil {
		t.Fatalf("loading chart: %v", err)
	}
	install := action.NewInstall(new(action.Configuration))
	install.ReleaseName = "schematic"
	install.Namespace = "schematic"
	install.DryRun = true
	install.ClientOnly = true
	rel, err := install.Run(ch, map[string]interface{}{})
	if err != nil {
		t.Fatalf("helm dry-run render: %v", err)
	}
	out := map[string]string{}
	for _, doc := range releaseutil.SplitManifests(rel.Manifest) {
		var head struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := k8syaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatalf("decoding rendered manifest: %v\n%s", err, doc)
		}
		out[head.Kind+"/"+head.Metadata.Name] = doc
	}
	return out
}

func decodeManifest(t *testing.T, manifests map[string]string, key string, into any) {
	t.Helper()
	doc, ok := manifests[key]
	if !ok {
		t.Fatalf("rendered chart has no %s", key)
	}
	if err := k8syaml.UnmarshalStrict([]byte(doc), into); err != nil {
		t.Fatalf("decoding %s: %v\n%s", key, err, doc)
	}
}

// TestHarnessCronJobsAreSuspendedTemplatesWithNoTokenAutomount covers D27 §2.2:
// each profile is a CronJob that never fires on its own (the pipeline runs it
// with `kubectl create job --from=cronjob/<name>`), its pod mounts no ambient
// Kubernetes token and no kernel-audience token, it reads its config from the
// generated PROTO_CLIENTS_HARNESS_CONFIG_* names, and it takes its accounts
// from the pipeline-written Secret harness-accounts, which the chart must not
// render itself.
func TestHarnessCronJobsAreSuspendedTemplatesWithNoTokenAutomount(t *testing.T) {
	manifests := renderedManifests(t, generateTestChart(t, harnessTestConfig(true)))

	wantDigests := "gateway@sha256:" + strings.Repeat("cd", 32) + ",kernel@sha256:" + strings.Repeat("ab", 32)
	for _, tc := range []struct {
		cronJob, serviceAccount, profile string
	}{
		{"harness", "harness", "gate"},
		{"harness-destructive", "harness-chaos", "destructive"},
	} {
		var cj batchv1.CronJob
		decodeManifest(t, manifests, "CronJob/"+tc.cronJob, &cj)

		if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
			t.Errorf("%s: suspend = %v, want true; the CronJob is a template the pipeline runs, never a schedule", tc.cronJob, cj.Spec.Suspend)
		}
		job := cj.Spec.JobTemplate.Spec
		if job.BackoffLimit == nil || *job.BackoffLimit != 0 {
			t.Errorf("%s: backoffLimit = %v, want 0; a retried run would hide a red result", tc.cronJob, job.BackoffLimit)
		}
		pod := job.Template.Spec
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
			t.Errorf("%s: automountServiceAccountToken = %v, want false", tc.cronJob, pod.AutomountServiceAccountToken)
		}
		if pod.ServiceAccountName != tc.serviceAccount {
			t.Errorf("%s: serviceAccountName = %q, want %q", tc.cronJob, pod.ServiceAccountName, tc.serviceAccount)
		}
		if pod.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("%s: restartPolicy = %q, want Never", tc.cronJob, pod.RestartPolicy)
		}
		for _, v := range pod.Volumes {
			if v.Projected == nil {
				continue
			}
			for _, src := range v.Projected.Sources {
				if src.ServiceAccountToken != nil && src.ServiceAccountToken.Audience == kernelTokenAudience() {
					t.Errorf("%s: volume %q projects a kernel-audience token", tc.cronJob, v.Name)
				}
			}
		}
		if len(pod.Containers) != 1 {
			t.Fatalf("%s: %d containers, want 1", tc.cronJob, len(pod.Containers))
		}
		c := pod.Containers[0]
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		for name, want := range map[string]string{
			"PROTO_CLIENTS_HARNESS_CONFIG_FQDN":             "dev.seabeam.net",
			"PROTO_CLIENTS_HARNESS_CONFIG_PROFILE":          tc.profile,
			"PROTO_CLIENTS_HARNESS_CONFIG_EXPECTED_DIGESTS": wantDigests,
		} {
			if env[name] != want {
				t.Errorf("%s: env %s = %q, want %q", tc.cronJob, name, env[name], want)
			}
		}
		if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil || c.EnvFrom[0].SecretRef.Name != "harness-accounts" {
			t.Errorf("%s: envFrom = %+v, want exactly the Secret harness-accounts", tc.cronJob, c.EnvFrom)
		} else if opt := c.EnvFrom[0].SecretRef.Optional; opt != nil && *opt {
			t.Errorf("%s: harness-accounts is optional; a missing Secret must fail the pod at start", tc.cronJob)
		}
	}

	// The gate pod has no Kubernetes API token at all: it holds no RBAC, so it
	// has nothing to use one for.
	var gate batchv1.CronJob
	decodeManifest(t, manifests, "CronJob/harness", &gate)
	if vols := gate.Spec.JobTemplate.Spec.Template.Spec.Volumes; len(vols) != 0 {
		t.Errorf("harness (gate) mounts volumes %+v; it must carry no token", vols)
	}

	if _, ok := manifests["Secret/harness-accounts"]; ok {
		t.Error("the chart renders Secret harness-accounts; it is pipeline-written and the operator never holds the passwords")
	}
	for _, sa := range []string{"harness", "harness-chaos"} {
		if _, ok := manifests["ServiceAccount/"+sa]; !ok {
			t.Errorf("no ServiceAccount %s rendered", sa)
		}
	}
}

// TestHarnessChaosRoleIsExactlyDeleteOnNamedPodsAndPVCs covers D27 §2.2: the
// destructive profile may delete exactly the blobstore and kernel pods and the
// blobstore PVCs, by name, and nothing else — no list, no get, no wildcard. The
// gate ServiceAccount is bound to no Role at all.
func TestHarnessChaosRoleIsExactlyDeleteOnNamedPodsAndPVCs(t *testing.T) {
	manifests := renderedManifests(t, generateTestChart(t, harnessTestConfig(true)))

	var role rbacv1.Role
	decodeManifest(t, manifests, "Role/harness-chaos", &role)
	want := []rbacv1.PolicyRule{
		{
			APIGroups:     []string{""},
			Resources:     []string{"pods"},
			Verbs:         []string{"delete"},
			ResourceNames: []string{"blobstore-0", "blobstore-1", "kernel-0", "kernel-1", "kernel-2"},
		},
		{
			APIGroups:     []string{""},
			Resources:     []string{"persistentvolumeclaims"},
			Verbs:         []string{"delete"},
			ResourceNames: []string{"data-blobstore-0", "data-blobstore-1"},
		},
	}
	if !reflect.DeepEqual(role.Rules, want) {
		t.Errorf("harness-chaos rules =\n%+v\nwant\n%+v", role.Rules, want)
	}

	var binding rbacv1.RoleBinding
	decodeManifest(t, manifests, "RoleBinding/harness-chaos", &binding)
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != "harness-chaos" {
		t.Errorf("harness-chaos binding roleRef = %+v", binding.RoleRef)
	}
	wantSubjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: "harness-chaos", Namespace: "schematic"}}
	if !reflect.DeepEqual(binding.Subjects, wantSubjects) {
		t.Errorf("harness-chaos binding subjects = %+v, want %+v", binding.Subjects, wantSubjects)
	}

	for key, doc := range manifests {
		if !strings.HasSuffix(key[:strings.Index(key, "/")], "Binding") {
			continue
		}
		var b struct {
			Subjects []rbacv1.Subject `json:"subjects"`
		}
		if err := k8syaml.Unmarshal([]byte(doc), &b); err != nil {
			t.Fatalf("decoding %s: %v", key, err)
		}
		for _, s := range b.Subjects {
			if s.Kind == "ServiceAccount" && s.Name == "harness" {
				t.Errorf("%s binds the gate ServiceAccount harness; it must hold no RBAC", key)
			}
		}
	}
}

// TestHarnessDestructiveAbsentWhenNotEnabled: pilot runs the gate profile only,
// so with harness_destructive off nothing destructive — CronJob, ServiceAccount
// or Role — may be rendered.
func TestHarnessDestructiveAbsentWhenNotEnabled(t *testing.T) {
	charts := generateTestChart(t, harnessTestConfig(false))
	if _, ok := charts.Files["templates/harness/cronjob.yaml"]; !ok {
		t.Fatal("the gate CronJob is missing; the negative check below would pass vacuously")
	}
	for path, body := range charts.Files {
		for _, forbidden := range []string{"harness-destructive", "harness-chaos", "profile-destructive"} {
			if strings.Contains(path, forbidden) || strings.Contains(string(body), forbidden) {
				t.Errorf("%s carries %q while optional_services.harness_destructive is false", path, forbidden)
			}
		}
	}
}

// TestHarnessRendersNothingWhenDisabled: the destructive flag alone enables
// nothing; it is a refinement of optional_services.harness.
func TestHarnessRendersNothingWhenDisabled(t *testing.T) {
	cfg := harnessTestConfig(true)
	cfg.OptionalServices.Harness = false
	for path := range generateTestChart(t, cfg).Files {
		if strings.Contains(path, "harness") {
			t.Errorf("harness rendered %s while optional_services.harness is false", path)
		}
	}
}
