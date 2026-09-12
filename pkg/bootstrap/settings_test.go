package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
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
// ["scotty", "scotty"]. LoadConfig must dedupe.
func TestLoadConfigDedupesOverlappingLlmCredentialServices(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		crypto   string
		want     []string
	}{
		{
			name:     "fully overlapping",
			settings: "fqdn: cluster.example.com\nllm_credential_services:\n  - scotty\n",
			crypto:   "llm_credential: sk-ant-secret-value\nllm_credential_services:\n  - scotty\n",
			want:     []string{"scotty"},
		},
		{
			name:     "partially overlapping",
			settings: "fqdn: cluster.example.com\nllm_credential_services:\n  - scotty\n  - tasks\n",
			crypto:   "llm_credential: sk-ant-secret-value\nllm_credential_services:\n  - scotty\n  - query\n",
			want:     []string{"scotty", "tasks", "query"},
		},
		{
			name:     "steady state: only crypto.yaml populates it",
			settings: "fqdn: cluster.example.com\n",
			crypto:   "llm_credential: sk-ant-secret-value\nllm_credential_services:\n  - scotty\n  - tasks\n",
			want:     []string{"scotty", "tasks"},
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
