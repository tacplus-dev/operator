package helm

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	protoOauth "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/oauth"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// serviceSpec.SecretEnvVars is the generic per-pod secret delivery mechanism
// (D25): a field that must not travel in kernel's combined seed is written by
// the deployment pipeline from a GitLab CI/CD variable into one Secret per
// service, and projected as a secretKeyRef env var. The chart references that
// Secret and never renders it; it renders a manifest the pipeline iterates.

func secretEnvTestSpec(statefulSet bool) serviceSpec {
	return serviceSpec{
		Name: "widget", Replicas: 2, Image: "widget:test",
		HasGRPC: true, ListenPort: 8443,
		IsStatefulSet:      statefulSet,
		HasPVC:             statefulSet,
		Storage:            "1Gi",
		ServiceAccountName: "widget",
		SecretEnvVars: []secretEnvVar{{
			EnvName:        "PROTO_SERVICES_WIDGET_SECRETS_API_KEY",
			Key:            "api-key",
			GitLabVariable: "WIDGET_API_KEY",
		}},
	}
}

// Both workload paths must project the pipeline-written Secret, and neither may
// render it: helm must not own an object the pipeline writes, or every reconcile
// would overwrite the real value.
func TestSecretEnvVarsAreProjectedInBothWorkloadPathsAndNeverRendered(t *testing.T) {
	for name, statefulSet := range map[string]bool{"statefulset": true, "deployment": false} {
		t.Run(name, func(t *testing.T) {
			g := New("test", "1.2", nil, nil, nil)
			charts := &Charts{Files: map[string][]byte{}}
			if err := g.addService(charts, &protoOperator.Config{Namespace: "schematic"}, secretEnvTestSpec(statefulSet)); err != nil {
				t.Fatalf("addService: %v", err)
			}
			workload := "templates/widget/deployment.yaml"
			if statefulSet {
				workload = "templates/widget/statefulset.yaml"
			}
			body := chartFile(t, charts, workload)
			for _, want := range []string{
				"name: PROTO_SERVICES_WIDGET_SECRETS_API_KEY",
				"secretKeyRef",
				"name: widget-pipeline-secrets",
				"key: api-key",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("%s does not read the value from the Secret (%q missing):\n%s", workload, want, body)
				}
			}
			for path, data := range charts.Files {
				if path != workload && strings.Contains(string(data), "widget-pipeline-secrets") {
					t.Errorf("%s mentions the pipeline-written Secret; only the workload may reference it:\n%s", path, data)
				}
			}
		})
	}
}

// The manifest names the workloads to restart after a write: one StatefulSet,
// or one Deployment per replica.
func TestPipelineSecretsManifestNamesTheConsumingWorkloads(t *testing.T) {
	for name, tc := range map[string]struct {
		statefulSet bool
		want        []string
	}{
		"statefulset": {true, []string{"statefulset/widget"}},
		"deployment":  {false, []string{"deployment/widget0", "deployment/widget1"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := decodePipelineSecrets(t, mustPipelineSecretsConfigMap(t, []serviceSpec{secretEnvTestSpec(tc.statefulSet), {Name: "plain", Replicas: 1}}))
			if len(got) != 1 {
				t.Fatalf("manifest has %d Secrets, want 1 (a service with no SecretEnvVars gets none): %+v", len(got), got)
			}
			if got[0].Name != "widget-pipeline-secrets" || strings.Join(got[0].Workloads, ",") != strings.Join(tc.want, ",") {
				t.Errorf("manifest entry = %+v, want Secret widget-pipeline-secrets restarting %v", got[0], tc.want)
			}
			if e := got[0].Entries; len(e) != 1 || e[0].Key != "api-key" || e[0].GitLabVariable != "WIDGET_API_KEY" {
				t.Errorf("manifest entries = %+v, want key api-key from WIDGET_API_KEY", e)
			}
		})
	}
	if got := decodePipelineSecrets(t, mustPipelineSecretsConfigMap(t, []serviceSpec{{Name: "plain", Replicas: 1}})); got == nil || len(got) != 0 {
		t.Errorf("a fleet with no declarer must still publish an empty list, got %#v", got)
	}
}

// End to end through helm: the fleet's real declarers (oauth, the one this
// library knows) appear in the rendered manifest with their GitLab variables,
// every workload it names is one the chart renders and that workload reads the
// Secret, and no Secret object of that name is rendered.
func TestPipelineSecretsManifestCoversEveryDeclarer(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			OauthNodes:  2,
		},
	})
	manifests := renderedManifests(t, charts)

	var cm corev1.ConfigMap
	decodeManifest(t, manifests, "ConfigMap/"+pipelineSecretsConfigMapName, &cm)
	got := map[string]pipelineSecret{}
	var secrets []pipelineSecret
	if err := json.Unmarshal([]byte(cm.Data["secrets.json"]), &secrets); err != nil {
		t.Fatalf("secrets.json is not JSON: %v\n%s", err, cm.Data["secrets.json"])
	}
	for _, s := range secrets {
		got[s.Name] = s
	}

	want := map[string]struct{ key, variable string }{
		"oauth-pipeline-secrets": {"apple_signin_key_p8", "APPLE_SIGNIN_KEY_P8"},
	}
	if len(got) != len(want) {
		t.Errorf("manifest Secrets = %v, want exactly %v", secrets, want)
	}
	for name, w := range want {
		s, ok := got[name]
		if !ok {
			t.Errorf("manifest has no %s: %+v", name, secrets)
			continue
		}
		if len(s.Entries) != 1 || s.Entries[0].Key != w.key || s.Entries[0].GitLabVariable != w.variable {
			t.Errorf("%s entries = %+v, want key %s from %s", name, s.Entries, w.key, w.variable)
		}
		if _, rendered := manifests["Secret/"+name]; rendered {
			t.Errorf("the chart renders Secret %s; the pipeline owns it", name)
		}
		if len(s.Workloads) == 0 {
			t.Errorf("%s names no workload to restart", name)
		}
		for _, wl := range s.Workloads {
			kind, wlName, _ := strings.Cut(wl, "/")
			key := map[string]string{"deployment": "Deployment/", "statefulset": "StatefulSet/"}[kind] + wlName
			doc, ok := manifests[key]
			if !ok {
				t.Errorf("%s names %s, which the chart does not render", name, wl)
				continue
			}
			if !strings.Contains(doc, "name: "+name) {
				t.Errorf("%s names %s, which does not read the Secret:\n%s", name, wl, doc)
			}
		}
	}
	if strings.Contains(chartFile(t, charts, "values.yaml"), "appleSigninKeyP8") {
		t.Error("values.yaml still carries a secret field; secret values must never travel in helm values")
	}
}

// EnvName must be the name tacplus's generated SecretsFromEnvVar reads, or the
// service starts with the secret silently empty. Checked against the generated
// Go helper rather than trusting the literal in enabledServices.
func TestSecretEnvVarNamesAreTheGeneratedOnes(t *testing.T) {
	generated := map[string]map[string]string{
		"oauth": protoOauth.SecretsToEnvVar(&protoOauth.Secrets{}),
	}
	g := New("test", "1.2", nil, nil, nil)
	cfg := &protoOperator.Config{
		Topology: &protoOperator.Topology{OauthNodes: 1},
	}
	declared := 0
	for _, svc := range g.enabledServices(cfg, effectiveTopology(cfg, 1)) {
		for _, e := range svc.SecretEnvVars {
			declared++
			names, ok := generated[svc.Name]
			if !ok {
				t.Errorf("%s declares %s but this test has no generated helper for it; add one", svc.Name, e.EnvName)
				continue
			}
			if _, ok := names[e.EnvName]; !ok {
				t.Errorf("%s declares %s, which its generated SecretsToEnvVar does not emit", svc.Name, e.EnvName)
			}
		}
	}
	if declared == 0 {
		t.Fatal("no service declares a SecretEnvVars entry")
	}
}

func mustPipelineSecretsConfigMap(t *testing.T, svcs []serviceSpec) []byte {
	t.Helper()
	out, err := pipelineSecretsConfigMap(svcs)
	if err != nil {
		t.Fatalf("pipelineSecretsConfigMap: %v", err)
	}
	return out
}

// decodePipelineSecrets renders a lone ConfigMap template through helm and
// returns its parsed manifest.
func decodePipelineSecrets(t *testing.T, tmpl []byte) []pipelineSecret {
	t.Helper()
	g := New("test", "1.2", nil, nil, nil)
	charts := &Charts{Files: map[string][]byte{"templates/pipeline-secrets.yaml": tmpl}}
	if err := g.addChartYAML(charts, &protoOperator.Config{Namespace: "schematic"}); err != nil {
		t.Fatalf("addChartYAML: %v", err)
	}
	if err := g.addHelpers(charts); err != nil {
		t.Fatalf("addHelpers: %v", err)
	}
	charts.Files["values.yaml"] = []byte("global:\n  namespace: schematic\n")
	var cm corev1.ConfigMap
	decodeManifest(t, renderedManifests(t, charts), "ConfigMap/"+pipelineSecretsConfigMapName, &cm)
	var out []pipelineSecret
	if err := json.Unmarshal([]byte(cm.Data["secrets.json"]), &out); err != nil {
		t.Fatalf("secrets.json is not JSON: %v\n%s", err, cm.Data["secrets.json"])
	}
	return out
}
