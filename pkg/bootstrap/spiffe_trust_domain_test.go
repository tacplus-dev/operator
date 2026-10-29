package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	wrapperX509 "gitlab.com/seabeam/schematic/platform/services/operator/model/chassis/x509"
	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	protoKmaker "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kmaker"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// writeTrustDomainFixture writes a config dir whose operator.yaml carries
// spiffePrefix and whose kernel.yaml is kernelYAML. Its crypto is minted for
// local.dev, as a fleet's is before it names its own trust domain.
func writeTrustDomainFixture(t *testing.T, spiffePrefix, kernelYAML string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "console.seabeam.cloud",
		SpiffePrefix: spiffePrefix,
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			GatewayNodes:  1,
			EventbusNodes: 1,
			OauthNodes:    1,
			OpaNodes:      1,
			KmakerNodes:   1,
		},
	}
	if err := GenerateCrypto(cfg, 1, 0, cfg.Fqdn, "local.dev"); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte(kernelYAML), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// assertOneTrustDomain checks that kernel, every service config, kmaker and
// kernel's certificate SAN all carry want, and nothing carries local.dev.
func assertOneTrustDomain(t *testing.T, dc *DeployConfigs, want string) {
	t.Helper()
	kernelConfig, err := protoKernel.ConfigFromBytes(dc.BootstrapYAMLs[0])
	if err != nil {
		t.Fatalf("parsing kernel config: %v", err)
	}
	if got := kernelConfig.GetSpiffeTrustDomain(); got != want {
		t.Errorf("kernel spiffe_trust_domain = %q, want %q", got, want)
	}

	kmakerConfig, err := protoKmaker.ConfigFromBytes(dc.KmakerConfig)
	if err != nil {
		t.Fatalf("parsing kmaker config: %v", err)
	}
	if got := kmakerConfig.GetSpiffePrefix(); got != want {
		t.Errorf("kmaker spiffe_prefix = %q, want %q", got, want)
	}

	configs := map[string]string{"operator": string(dc.OperatorBootstrapConfig)}
	for name, v := range ServiceConfigValues(dc) {
		entry, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if s, _ := entry["config"].(string); s != "" {
			configs[name] = s
		}
	}
	for _, svc := range []string{"gateway", "eventbus", "oauth", "opa"} {
		if !strings.Contains(configs[svc], "spiffe_trust_domain: "+want) {
			t.Errorf("%s config lacks spiffe_trust_domain: %s:\n%s", svc, want, configs[svc])
		}
	}
	for name, s := range configs {
		if strings.Contains(s, "local.dev") {
			t.Errorf("%s config still names local.dev", name)
		}
	}

	chain, err := wrapperX509.NewImmutableX509CertChain(dc.Cfg.Pki.NodeTls[0])
	if err != nil {
		t.Fatal(err)
	}
	wantSAN := "spiffe://" + want + "/system/services/kernel"
	var sans []string
	for _, u := range chain.Leaf().X509Certificate().URIs {
		sans = append(sans, u.String())
	}
	if len(sans) != 1 || sans[0] != wantSAN {
		t.Errorf("kernel certificate URI SANs = %v, want [%s]", sans, wantSAN)
	}
}

// spiffe_prefix in operator.yaml is the fleet's trust domain: set there alone,
// it reaches kernel, every service config, kmaker and kernel's certificate.
func TestSpiffePrefixIsTheTrustDomainOfEveryComponent(t *testing.T) {
	dir := writeTrustDomainFixture(t, "example.tacplus.dev", "{}\n")
	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertOneTrustDomain(t, dc, "example.tacplus.dev")
}

// kernel.yaml's spiffe_trust_domain, set alone, is the one value too: kmaker
// gets it as well, rather than falling back to local.dev.
func TestKernelSpiffeTrustDomainAloneIsTheTrustDomainOfEveryComponent(t *testing.T) {
	dir := writeTrustDomainFixture(t, "", "spiffe_trust_domain: example.tacplus.dev\n")
	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertOneTrustDomain(t, dc, "example.tacplus.dev")
}

// Both set and agreeing is accepted.
func TestSpiffePrefixAndKernelTrustDomainAgreeing(t *testing.T) {
	dir := writeTrustDomainFixture(t, "example.tacplus.dev", "spiffe_trust_domain: example.tacplus.dev\n")
	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertOneTrustDomain(t, dc, "example.tacplus.dev")
}

// Both set and differing is refused, naming both files and values, instead of
// splitting the fleet across two trust domains.
func TestSpiffePrefixAndKernelTrustDomainMismatchIsRefused(t *testing.T) {
	dir := writeTrustDomainFixture(t, "example.tacplus.dev", "spiffe_trust_domain: other.tacplus.dev\n")
	_, err := BuildDeployConfigs(dir)
	if err == nil {
		t.Fatal("differing spiffe_prefix and kernel.yaml spiffe_trust_domain built without error")
	}
	for _, want := range []string{"spiffe_prefix", "example.tacplus.dev", "services/kernel.yaml", "other.tacplus.dev"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}
