package bootstrap

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	protoKmaker "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kmaker"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// generateOnce runs the two steps of `generate` that write into the config
// dir: delegate reconciliation in services/kernel.yaml, then configs/.
func generateOnce(t *testing.T, dir string) {
	t.Helper()
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatalf("ensuring service delegates: %v", err)
	}
	if err := BuildConfigOutput(dir); err != nil {
		t.Fatalf("building config output: %v", err)
	}
}

func readConfigs(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "configs"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		out[e.Name()] = readBytes(t, filepath.Join(dir, "configs", e.Name()))
	}
	return out
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestGenerateNeverReMintsCrypto: generate re-minted kmaker's serving cert and
// key into configs/kmaker.yaml on every run, because that cert is not part of
// crypto.yaml. Each run clobbered the material the last one wrote, so the
// generated output was never stable and a tenant approved between runs could
// not be rolled out by generate alone. Crypto already on disk must be kept
// byte-for-byte; only the seed (delegates) may change.
func TestGenerateNeverReMintsCrypto(t *testing.T) {
	withRegistry(t, registryMarking([]string{"dog"}, nil))
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "console.seabeam.cloud",
		SpiffePrefix: "local.dev",
		Tenants:      []string{"acme"},
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			GatewayNodes:  1,
			EventbusNodes: 1,
			KmakerNodes:   1,
			AppServices:   map[string]int32{"dog": 1},
		},
	}
	if err := GenerateCrypto(cfg, 1, 0, cfg.Fqdn, cfg.SpiffePrefix); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "services", "kernel.yaml"), "{}\n")
	writeTestFile(t, filepath.Join(dir, "tenants", "acme", "oauth.yaml"), "providers: []\n")

	generateOnce(t, dir)
	first := readConfigs(t, dir)
	firstCrypto := readBytes(t, filepath.Join(dir, "crypto.yaml"))

	// Nothing changed in between, so nothing in the output may change.
	generateOnce(t, dir)
	for name, want := range first {
		if got := readConfigs(t, dir)[name]; !bytes.Equal(got, want) {
			t.Errorf("configs/%s changed on a no-op re-generate", name)
		}
	}

	// Approve a tenant and generate again: its delegate must reach the seed
	// without a hand edit, and no crypto may move.
	writeTestFile(t, TenantRequestPath(dir, "beta"), "providers: []\n")
	if err := ApproveTenant(dir, "beta"); err != nil {
		t.Fatalf("ApproveTenant: %v", err)
	}
	generateOnce(t, dir)
	second := readConfigs(t, dir)

	if !bytes.Equal(readBytes(t, filepath.Join(dir, "crypto.yaml")), firstCrypto) {
		t.Error("crypto.yaml changed across generate runs")
	}
	if !bytes.Equal(second["kmaker.yaml"], first["kmaker.yaml"]) {
		t.Error("configs/kmaker.yaml changed after approve-tenant + generate")
	}
	firstKmaker, err := protoKmaker.ConfigFromBytes(first["kmaker.yaml"])
	if err != nil {
		t.Fatal(err)
	}
	if firstKmaker.GetTls().GetKey() == nil {
		t.Fatal("kmaker.yaml carries no serving key — the stability check above is vacuous")
	}

	before, err := protoKernel.ConfigFromBytes(first["bootstrap0.yaml"])
	if err != nil {
		t.Fatal(err)
	}
	after, err := protoKernel.ConfigFromBytes(second["bootstrap0.yaml"])
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(before.GetTls(), after.GetTls()) || !proto.Equal(before.GetSigningCa(), after.GetSigningCa()) {
		t.Error("kernel node TLS or signing CA in bootstrap0.yaml changed across generate runs")
	}
	found := false
	for _, d := range after.GetDelegates() {
		if d.GetName() == "dog" && d.GetTenantNamespace() == tenantNamespacePrefix+"beta" {
			found = true
		}
	}
	if !found {
		t.Errorf("approved tenant beta has no dog delegate in bootstrap0.yaml; delegates: %v", after.GetDelegates())
	}
}
