package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// llm_credential is a real API key. It used to land in operator.yaml, which the
// chart renders into the schematic-operator-config ConfigMap -- unencrypted at
// rest and readable by anything in the namespace. It belongs in crypto.yaml,
// which renders into the schematic-operator-crypto Secret.
func TestSaveSettingsKeepsLlmCredentialOutOfOperatorYAML(t *testing.T) {
	dir := t.TempDir()

	cfg := &protoOperator.Config{
		Fqdn:                  "cluster.example.com",
		LlmCredential:         "sk-ant-secret-value",
		LlmCredentialServices: []string{"tasks", "query"},
	}

	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatalf("SaveCrypto: %v", err)
	}

	settings := readFileString(t, filepath.Join(dir, OperatorSettingsFile))
	for _, needle := range []string{"llm_credential", "LlmCredential", "sk-ant-secret-value"} {
		if strings.Contains(settings, needle) {
			t.Errorf("%s contains %q:\n%s", OperatorSettingsFile, needle, settings)
		}
	}

	crypto := readFileString(t, filepath.Join(dir, OperatorCryptoFile))
	for _, needle := range []string{"llm_credential", "sk-ant-secret-value", "tasks", "query"} {
		if !strings.Contains(crypto, needle) {
			t.Errorf("%s missing %q:\n%s", OperatorCryptoFile, needle, crypto)
		}
	}

	// The merge of both files must still yield the whole config.
	got, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.GetFqdn() != cfg.Fqdn {
		t.Errorf("fqdn = %q, want %q", got.GetFqdn(), cfg.Fqdn)
	}
	if got.GetLlmCredential() != cfg.LlmCredential {
		t.Errorf("llm_credential = %q, want %q", got.GetLlmCredential(), cfg.LlmCredential)
	}
	if strings.Join(got.GetLlmCredentialServices(), ",") != strings.Join(cfg.LlmCredentialServices, ",") {
		t.Errorf("llm_credential_services = %v, want %v", got.GetLlmCredentialServices(), cfg.LlmCredentialServices)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// A stale operator.yaml written before llm_credential_services moved to
// crypto.yaml still carries the field. proto.Merge appends repeated fields, so
// loading it alongside a post-migration crypto.yaml used to yield
// ["agent", "agent"]. LoadConfig must dedupe.
func TestLoadConfigDedupesOverlappingLlmCredentialServices(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		crypto   string
		want     []string
	}{
		{
			name:     "fully overlapping",
			settings: "fqdn: cluster.example.com\nllm_credential_services:\n  - agent\n",
			crypto:   "llm_credential: sk-ant-secret-value\nllm_credential_services:\n  - agent\n",
			want:     []string{"agent"},
		},
		{
			name:     "partially overlapping",
			settings: "fqdn: cluster.example.com\nllm_credential_services:\n  - agent\n  - tasks\n",
			crypto:   "llm_credential: sk-ant-secret-value\nllm_credential_services:\n  - agent\n  - query\n",
			want:     []string{"agent", "tasks", "query"},
		},
		{
			name:     "steady state: only crypto.yaml populates it",
			settings: "fqdn: cluster.example.com\n",
			crypto:   "llm_credential: sk-ant-secret-value\nllm_credential_services:\n  - agent\n  - tasks\n",
			want:     []string{"agent", "tasks"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFileString(t, filepath.Join(dir, OperatorSettingsFile), tc.settings)
			writeFileString(t, filepath.Join(dir, OperatorCryptoFile), tc.crypto)

			got, err := LoadConfig(dir)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if strings.Join(got.GetLlmCredentialServices(), ",") != strings.Join(tc.want, ",") {
				t.Errorf("llm_credential_services = %v, want %v", got.GetLlmCredentialServices(), tc.want)
			}
			// The rest of the merge must be unaffected.
			if got.GetFqdn() != "cluster.example.com" {
				t.Errorf("fqdn = %q, want %q", got.GetFqdn(), "cluster.example.com")
			}
			if got.GetLlmCredential() != "sk-ant-secret-value" {
				t.Errorf("llm_credential = %q, want %q", got.GetLlmCredential(), "sk-ant-secret-value")
			}
		})
	}
}

func writeFileString(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// images.yaml is the release manifest scripts/promote.sh writes. LoadConfig
// must merge its image_digests in, and SaveSettings must not copy them into
// operator.yaml: a pin left there would outlive the manifest that set it.
func TestImageDigestsLoadFromImagesYAMLOnly(t *testing.T) {
	dir := t.TempDir()
	digest := "sha256:" + strings.Repeat("cd", 32)
	manifest := "image_digests:\n  kernel: " + digest + "\n"
	if err := os.WriteFile(filepath.Join(dir, OperatorImagesFile), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, &protoOperator.Config{Fqdn: "cluster.example.com"}); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.GetImageDigests()["kernel"]; got != digest {
		t.Fatalf("image_digests[kernel] = %q, want %q", got, digest)
	}

	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, OperatorSettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "image_digests") {
		t.Errorf("operator.yaml carries image_digests:\n%s", data)
	}
}

// tls.letsencrypt retired with the in-deployment Let's Encrypt path
// (tacplus-dev/roadmap decisions/certificates-by-pipeline-2026-11-03.md): the
// pipeline delivers the edge certificate and the gateway serves only the chain
// it is given. Its field number is reserved, so ConfigFromBytes would drop the
// key and quietly start a deployment that asked for letsencrypt without one.
// LoadConfig refuses it instead, naming the field and the decision; false is
// refused too, because the key no longer means anything either way.
func TestLoadConfigRefusesRetiredLetsencrypt(t *testing.T) {
	for _, settings := range []string{
		"fqdn: cluster.example.com\ntls:\n  letsencrypt: true\n",
		"fqdn: cluster.example.com\ntls:\n  letsencrypt: true\n  email: ops@example.com\n",
		"fqdn: cluster.example.com\ntls:\n  letsencrypt: false\n",
		`{"fqdn": "cluster.example.com", "tls": {"letsencrypt": true}}`,
	} {
		dir := t.TempDir()
		writeFileString(t, filepath.Join(dir, OperatorSettingsFile), settings)

		_, err := LoadConfig(dir)
		if err == nil {
			t.Fatalf("LoadConfig accepted an operator.yaml that sets tls.letsencrypt:\n%s", settings)
		}
		for _, want := range []string{OperatorSettingsFile, "tls.letsencrypt", "decisions/certificates-by-pipeline-2026-11-03.md"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to name %q", err, want)
			}
		}
	}

	// A tls block without the key still loads.
	dir := t.TempDir()
	writeFileString(t, filepath.Join(dir, OperatorSettingsFile), "fqdn: cluster.example.com\ntls: {}\n")
	if _, err := LoadConfig(dir); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}

// external_ip retired with the gateway's LoadBalancer Service (#22;
// tacplus-dev/roadmap decisions/network-exposure-2026-11-03.md): only the
// cloud's proxy load balancer is public and it holds the address, the gateway
// behind it as a NEG. Its field number is reserved, so ConfigFromBytes would
// drop the key and quietly start a deployment whose public address its
// operator.yaml still names. LoadConfig refuses it instead, empty too.
func TestLoadConfigRefusesRetiredExternalIP(t *testing.T) {
	for _, settings := range []string{
		"fqdn: cluster.example.com\nexternal_ip: 146.148.80.132\nport: 443\n",
		"fqdn: cluster.example.com\nexternal_ip: \"\"\n",
		`{"fqdn": "cluster.example.com", "externalIp": "146.148.80.132"}`,
	} {
		dir := t.TempDir()
		writeFileString(t, filepath.Join(dir, OperatorSettingsFile), settings)

		_, err := LoadConfig(dir)
		if err == nil {
			t.Fatalf("LoadConfig accepted an operator.yaml that sets external_ip:\n%s", settings)
		}
		for _, want := range []string{OperatorSettingsFile, "external_ip", "decisions/network-exposure-2026-11-03.md"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to name %q", err, want)
			}
		}
	}
}

// The shared Stratum's topology retired with per-tenant Stratum (#2;
// tacplus-dev/roadmap decisions/stratum-per-tenant-2026-10-25.md): no writers,
// no search members, no disk persistence. Their field numbers are reserved, so
// ConfigFromBytes would drop the keys, and a deployment that asked for three
// writers would quietly get one member per tenant. LoadConfig refuses them,
// naming the key and stratum_members, which replaces them.
func TestLoadConfigRefusesRetiredStratumTopology(t *testing.T) {
	for _, tc := range []struct{ settings, key string }{
		{"fqdn: cluster.example.com\ntopology:\n  kernel_nodes: 3\n  stratum_nodes: 3\n", "stratum_nodes"},
		{"fqdn: cluster.example.com\ntopology:\n  stratum_nodes: 0\n", "stratum_nodes"},
		{"fqdn: cluster.example.com\ntopology:\n  stratum_search_nodes: 2\n", "stratum_search_nodes"},
		{`{"fqdn": "cluster.example.com", "topology": {"stratumNodes": 3}}`, "stratum_nodes"},
		{"fqdn: cluster.example.com\nstratum_disk_persistence:\n  dir: /app/data\n", "stratum_disk_persistence"},
		{`{"fqdn": "cluster.example.com", "stratumDiskPersistence": {}}`, "stratum_disk_persistence"},
	} {
		dir := t.TempDir()
		writeFileString(t, filepath.Join(dir, OperatorSettingsFile), tc.settings)

		_, err := LoadConfig(dir)
		if err == nil {
			t.Fatalf("LoadConfig accepted an operator.yaml that sets %s:\n%s", tc.key, tc.settings)
		}
		for _, want := range []string{OperatorSettingsFile, tc.key, "stratum_members"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to name %q", err, want)
			}
		}
	}

	// stratum_members loads.
	dir := t.TempDir()
	writeFileString(t, filepath.Join(dir, OperatorSettingsFile), "fqdn: cluster.example.com\ntopology:\n  stratum_members: 3\n")
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.GetTopology().GetStratumMembers(); got != 3 {
		t.Errorf("topology.stratum_members = %d, want 3", got)
	}
}
