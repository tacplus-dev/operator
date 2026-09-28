package helm

import (
	"bytes"
	"strings"
	"testing"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// serviceSpec.SecretEnvVars is the generic per-pod secret delivery mechanism: a
// field that must not travel in kernel's combined seed is rendered into one
// Secret per service and projected as a secretKeyRef env var.
//
// NO SERVICE DECLARES ONE TODAY. blobstore declared token_hmac_key here until
// that key was deleted outright (2026-09-27: an upload token is raw randomness
// verified by a record lookup, so there is no key), and llm_credential — which
// has the same exposure and is the next declarer — needs a proto change in the
// application repo first. So these tests drive the mechanism from a SYNTHETIC
// spec rather than through Generate: without them the plumbing would have no
// coverage at all until that wiring lands.

func secretEnvTestSpec(statefulSet bool) serviceSpec {
	return serviceSpec{
		Name: "widget", Replicas: 1, Image: "widget:test",
		HasGRPC: true, ListenPort: 8443,
		IsStatefulSet:      statefulSet,
		HasPVC:             statefulSet,
		Storage:            "1Gi",
		ServiceAccountName: "widget",
		SecretEnvVars: []secretEnvVar{{
			EnvName:     "PROTO_SERVICES_WIDGET_SECRETS_API_KEY",
			Key:         "api-key",
			ValuesField: "apiKey",
		}},
	}
}

func TestSecretEnvVarsRenderOneSecretPerService(t *testing.T) {
	got := string(secretEnvSecret(secretEnvTestSpec(true)))
	for _, want := range []string{
		"kind: Secret",
		"name: widget-secrets",
		"type: Opaque",
		// The value arrives from a helm value, so a chart written to disk holds
		// nothing: the real secret is supplied at install time by DeployConfigs.
		`api-key: {{ index .Values "widget" "apiKey" | quote }}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendered Secret is missing %q:\n%s", want, got)
		}
	}
	// One Secret per SERVICE, never per replica: a per-ordinal Secret would give
	// each pod a different value, and a field delivered this way is one a set of
	// pods has to agree on.
	if name := secretEnvSecretName(secretEnvTestSpec(true)); name != "widget-secrets" {
		t.Errorf("secret name = %q, want widget-secrets (no replica ordinal)", name)
	}
	// A service that declares none gets no Secret at all.
	if secretEnvSecret(serviceSpec{Name: "plain"}) != nil {
		t.Error("a service with no SecretEnvVars was given a Secret")
	}
}

// Both workload paths must project it. The StatefulSet path was the only one
// exercised while blobstore was the declarer, and the next declarer is a
// Deployment.
func TestSecretEnvVarsAreProjectedInBothWorkloadPaths(t *testing.T) {
	for name, statefulSet := range map[string]bool{"statefulset": true, "deployment": false} {
		t.Run(name, func(t *testing.T) {
			g := New("test", "1.2", nil, nil, nil)
			charts := &Charts{Files: map[string][]byte{}}
			if err := g.addService(charts, &protoOperator.Config{Namespace: "schematic"}, secretEnvTestSpec(statefulSet)); err != nil {
				t.Fatalf("addService: %v", err)
			}
			if _, ok := charts.Files["templates/widget/secret-env.yaml"]; !ok {
				t.Fatalf("no Secret was emitted; files: %v", chartFileNames(charts))
			}
			workload := "templates/widget/deployment.yaml"
			if statefulSet {
				workload = "templates/widget/statefulset.yaml"
			}
			body := chartFile(t, charts, workload)
			for _, want := range []string{
				"name: PROTO_SERVICES_WIDGET_SECRETS_API_KEY",
				"secretKeyRef",
				"name: widget-secrets",
				"key: api-key",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("%s does not read the value from the Secret (%q missing):\n%s", workload, want, body)
				}
			}
		})
	}
}

// values.yaml carries an EMPTY placeholder for every declared field. Without it
// a dry-run render nil-pointers on .Values.<svc>.<field>.
func TestSecretEnvVarsGetAnEmptyValuesPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	if err := valuesTmpl.Execute(&buf, map[string]any{
		"Namespace": "schematic",
		"Services":  []serviceSpec{secretEnvTestSpec(false)},
	}); err != nil {
		t.Fatalf("rendering values.yaml: %v", err)
	}
	if !strings.Contains(buf.String(), `apiKey: ""`) {
		t.Errorf("values.yaml has no empty placeholder for the declared field:\n%s", buf.String())
	}
}
