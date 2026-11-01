package helm

import (
	"encoding/base64"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	protoMailbox "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/mailbox"
	protoOauth "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/oauth"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
	protoOperatorV1 "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operatorv1"
)

// serviceSpec.SecretEnvVars delivers a service's own Secrets message to its
// pods only, never through kernel's combined seed. Its values come from the
// uni-config's secrets (#17): the operator renders one Secret per service from
// them, and nothing else writes it. The pods still read it as env until the
// generated Secrets reader takes a file (tacplus-dev/tacplus#262).

func secretEnvTestSpec(statefulSet bool, value string) serviceSpec {
	return serviceSpec{
		Name: "widget", Replicas: 2, Image: "widget:test",
		HasGRPC: true, ListenPort: 8443,
		IsStatefulSet:      statefulSet,
		HasPVC:             statefulSet,
		Storage:            "1Gi",
		ServiceAccountName: "widget",
		SecretEnvVars: []secretEnvVar{{
			EnvName: "PROTO_SERVICES_WIDGET_SECRETS_API_KEY",
			Key:     "api-key",
			Value:   value,
		}},
	}
}

// Both workload paths read the Secret, and the chart renders it with the value
// the uni-config holds.
func TestServiceSecretsAreRenderedFromTheUniConfig(t *testing.T) {
	for name, statefulSet := range map[string]bool{"statefulset": true, "deployment": false} {
		t.Run(name, func(t *testing.T) {
			g := New("test", "1.2", nil, nil, nil)
			charts := &Charts{Files: map[string][]byte{}}
			if err := g.addService(charts, &protoOperator.Config{Namespace: "schematic"}, secretEnvTestSpec(statefulSet, "s3cret")); err != nil {
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
				"name: widget-secrets",
				"key: api-key",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("%s does not read the value from the Secret (%q missing):\n%s", workload, want, body)
				}
			}
			secret := chartFile(t, charts, "templates/widget/secrets.yaml")
			for _, want := range []string{
				"kind: Secret",
				"name: widget-secrets",
				"api-key: \"" + base64.StdEncoding.EncodeToString([]byte("s3cret")) + "\"",
			} {
				if !strings.Contains(secret, want) {
					t.Errorf("templates/widget/secrets.yaml lacks %q:\n%s", want, secret)
				}
			}
		})
	}
}

// End to end through helm, from the uni-config's Secrets message: oauth's and
// mailbox's Secrets reach their own Secret and their own workloads, and no
// pipeline-written Secret or manifest of them is left.
func TestServiceSecretsComeFromTheUniConfigSecrets(t *testing.T) {
	cfg := &protoOperator.Config{
		Namespace:        "schematic",
		Topology:         &protoOperator.Topology{KernelNodes: 1, OauthNodes: 2, MailboxNodes: 1},
		OptionalServices: &protoOperator.OptionalServices{Mailbox: true, Messaging: true},
	}
	g := New("test", "1.2", nil, nil, nil)
	g.SetSecrets(&protoOperatorV1.Secrets{
		Oauth:   &protoOauth.Secrets{AppleSigninKeyP8: "apple-key"},
		Mailbox: &protoMailbox.Secrets{GmailServiceAccountJson: `{"client_email":"crew@example.com"}`},
	})
	charts, err := g.Generate(cfg, 1, false)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	manifests := renderedManifests(t, charts)

	for name, want := range map[string]map[string]string{
		"oauth-secrets":   {"apple_signin_key_p8": "apple-key"},
		"mailbox-secrets": {"gmail_service_account_json": `{"client_email":"crew@example.com"}`},
	} {
		var s corev1.Secret
		decodeManifest(t, manifests, "Secret/"+name, &s)
		for k, v := range want {
			if got := string(s.Data[k]); got != v {
				t.Errorf("Secret %s key %s = %q, want %q", name, k, got, v)
			}
		}
	}
	for key, doc := range manifests {
		if strings.Contains(doc, "pipeline-secrets") {
			t.Errorf("%s still names a pipeline-written Secret or its manifest:\n%s", key, doc)
		}
	}
	for path, data := range charts.Files {
		if path == "values.yaml" && (strings.Contains(string(data), "apple-key") || strings.Contains(string(data), "crew@example.com")) {
			t.Error("values.yaml carries a secret value; secret values never travel in helm values")
		}
	}
}

// A changed secret rolls the pods that read it, and only those: the Secret's
// content is a checksum on that service's pod template.
func TestServiceSecretChangeRollsOnlyItsService(t *testing.T) {
	render := func(key string) map[string]string {
		g := New("test", "1.2", nil, nil, nil)
		g.SetSecrets(&protoOperatorV1.Secrets{Oauth: &protoOauth.Secrets{AppleSigninKeyP8: key}})
		charts, err := g.Generate(&protoOperator.Config{
			Namespace: "schematic",
			Topology:  &protoOperator.Topology{KernelNodes: 1, OauthNodes: 1, OpaNodes: 1},
		}, 1, false)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		return renderedManifests(t, charts)
	}
	first, second := render("key-one"), render("key-two")

	podAnnotations := func(manifests map[string]string, key string) map[string]string {
		var d appsv1.Deployment
		decodeManifest(t, manifests, key, &d)
		return d.Spec.Template.Annotations
	}
	if a, b := podAnnotations(first, "Deployment/oauth0"), podAnnotations(second, "Deployment/oauth0"); a["checksum/secrets"] == "" || a["checksum/secrets"] == b["checksum/secrets"] {
		t.Errorf("oauth0's pod template did not change with its secret: %v vs %v", a, b)
	}
	if a, b := podAnnotations(first, "Deployment/opa0"), podAnnotations(second, "Deployment/opa0"); len(a) != len(b) || a["checksum/secrets"] != b["checksum/secrets"] {
		t.Errorf("opa0's pod template changed with oauth's secret: %v vs %v", a, b)
	}
	for k := range podAnnotations(first, "Deployment/opa0") {
		if k == "checksum/secrets" {
			t.Error("opa0 reads no secret but carries a secrets checksum")
		}
	}
}

// No service secrets in the uni-config still renders each declarer's Secret,
// its keys present and empty, which the services read as "not configured".
func TestServiceSecretsAbsentRenderEmptyKeys(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology:  &protoOperator.Topology{KernelNodes: 1, OauthNodes: 1},
	})
	var s corev1.Secret
	decodeManifest(t, renderedManifests(t, charts), "Secret/oauth-secrets", &s)
	if v, ok := s.Data["apple_signin_key_p8"]; !ok || len(v) != 0 {
		t.Errorf("oauth-secrets apple_signin_key_p8 = %q (present %v), want present and empty", v, ok)
	}
}

// EnvName must be the name tacplus's generated SecretsFromEnvVar reads, or the
// service starts with the secret silently empty. Checked against the generated
// Go helper rather than trusting the literal in enabledServices.
func TestSecretEnvVarNamesAreTheGeneratedOnes(t *testing.T) {
	generated := map[string]map[string]string{
		"oauth":   protoOauth.SecretsToEnvVar(&protoOauth.Secrets{}),
		"mailbox": protoMailbox.SecretsToEnvVar(&protoMailbox.Secrets{}),
	}
	g := New("test", "1.2", nil, nil, nil)
	cfg := &protoOperator.Config{
		Topology:         &protoOperator.Topology{OauthNodes: 1, MailboxNodes: 1},
		OptionalServices: &protoOperator.OptionalServices{Mailbox: true, Messaging: true},
	}
	declared := map[string]bool{}
	for _, svc := range g.enabledServices(cfg, effectiveTopology(cfg, 1)) {
		for _, e := range svc.SecretEnvVars {
			declared[svc.Name] = true
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
	for svc := range generated {
		if !declared[svc] {
			t.Errorf("%s declares no SecretEnvVars entry", svc)
		}
	}
}
