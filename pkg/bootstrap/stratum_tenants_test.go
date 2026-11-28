package bootstrap

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	protoKernel "github.com/tacplus-dev/operator/tacplus/types/services/kernel"
	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// stratumTestConfig is a one-namespace fleet with two tenants, acme and beta.
func stratumTestConfig(t *testing.T) *protoOperator.Config {
	t.Helper()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "console.example.com",
		SpiffePrefix: "local.dev",
		Tenants:      []string{"acme", "beta"},
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
		},
	}
	if err := GenerateCrypto(cfg, 1, 0, cfg.Fqdn, cfg.SpiffePrefix); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// writeStratumConfigDir saves cfg to dir, with kernelYAML as services/kernel.yaml.
func writeStratumConfigDir(t *testing.T, dir string, cfg *protoOperator.Config, kernelYAML string) {
	t.Helper()
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "services", "kernel.yaml"), []byte(kernelYAML), 0644); err != nil {
		t.Fatal(err)
	}
}

// stratumConfigs saves cfg to dir and returns the bootstrap config under every
// values key, from both paths that feed the chart: the reconciler's
// (ServiceConfigValues) and `apply --bundle`'s (BundleServiceValues). They must
// agree, so a mismatch fails here.
func stratumConfigs(t *testing.T, dir string, cfg *protoOperator.Config) map[string]string {
	t.Helper()
	writeStratumConfigDir(t, dir, cfg, "{}\n")
	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := BuildBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	fromBundle := BundleServiceValues(bundle)

	out := map[string]string{}
	for key, v := range ServiceConfigValues(dc) {
		got, _ := v.(map[string]interface{})["config"].(string)
		want, _ := fromBundle[key].(map[string]interface{})["config"].(string)
		if got != want {
			t.Errorf("%s: ServiceConfigValues and BundleServiceValues disagree", key)
		}
		out[key] = got
	}
	return out
}

// bootstrapTenants returns kernel's bootstrap tenants, by name, from dc's first
// kernel bootstrap.
func bootstrapTenants(t *testing.T, dc *DeployConfigs) map[string]*protoKernel.BootstrapTenant {
	t.Helper()
	kernel, err := protoKernel.ConfigFromBytes(dc.BootstrapYAMLs[0])
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*protoKernel.BootstrapTenant{}
	for _, bt := range kernel.GetTenants() {
		out[bt.GetName()] = bt
	}
	return out
}

// Each tenant's Stratum gets its bootstrap under its own instance,
// stratum-<tenant>: every listed tenant but restore, and people. There is no
// shared stratum, and no search members.
func TestEveryTenantsStratumGetsItsBootstrap(t *testing.T) {
	cfg := stratumTestConfig(t)
	cfg.Tenants = append(cfg.Tenants, "restore")
	configs := stratumConfigs(t, t.TempDir(), cfg)
	for _, key := range []string{"stratum-acme", "stratum-beta", "stratum-people"} {
		if configs[key] == "" {
			t.Errorf("no %s bootstrap", key)
		}
	}
	for _, key := range []string{"stratum", "stratum-search", "stratum-restore"} {
		if _, ok := configs[key]; ok {
			t.Errorf("a %s bootstrap is rendered", key)
		}
	}
}

// stratum declares spec.member_port (tacplus#212), so its generated
// ServiceManager refuses to start without grpcconfig.member_port: on seatrials
// (2026-11-27) every stratum pod ended "grpcconfig.member_port.listen_address
// and target_address are required". Every tenant's Stratum bootstrap carries
// it: the member port on the port the chart exposes and the seed_members name,
// Hazelcast behind it on loopback. Nothing of the old shared cluster's
// (disk_persistence) is left.
func TestStratumBootstrapCarriesMemberPort(t *testing.T) {
	configs := stratumConfigs(t, t.TempDir(), stratumTestConfig(t))
	for _, key := range []string{"stratum-acme", "stratum-beta", "stratum-people"} {
		var fields map[string]any
		if err := yaml.Unmarshal([]byte(configs[key]), &fields); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"listen_address": map[string]any{"host": "0.0.0.0", "port": 5001},
			"target_address": map[string]any{"host": "127.0.0.1", "port": 5701},
		}
		if got := fields["member_port"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s member_port = %v, want %v", key, got, want)
		}
		if _, ok := fields["disk_persistence"]; ok {
			t.Errorf("%s carries disk_persistence", key)
		}
	}
}

// Option B (Jody, 2026-11-03; kernel#34): each tenant's Stratum seed members
// are state the operator derives, delivered through kernel's bootstrap
// tenants, which kernel writes into the tenant's TenantConfig.stratum at every
// boot. In a deployment every member is named in full, in its tenant's
// namespace; people is listed for its seeds alone (no tid: kernel has its
// record), and restore, which has no Stratum, not at all.
func TestBootstrapTenantsCarryEachTenantsStratumSeeds(t *testing.T) {
	cfg := deploymentFixtureConfig()
	cfg.Tenants = []string{"acme", "beta", "restore"}
	cfg.TenantIds = map[string]string{"acme": "a1b2c3d4", "beta": "b1c2d3e4"}
	cfg.Topology.StratumMembers = 2
	dc, err := BuildDeployConfigs(writeDeploymentFixture(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	got := bootstrapTenants(t, dc)
	want := map[string]struct {
		tid   string
		seeds []string
	}{
		"acme": {"a1b2c3d4", []string{
			"stratum-acme-0.stratum-acme.tac-prod-a1b2c3d4.svc.cluster.local:5001",
			"stratum-acme-1.stratum-acme.tac-prod-a1b2c3d4.svc.cluster.local:5001",
		}},
		"beta": {"b1c2d3e4", []string{
			"stratum-beta-0.stratum-beta.tac-prod-b1c2d3e4.svc.cluster.local:5001",
			"stratum-beta-1.stratum-beta.tac-prod-b1c2d3e4.svc.cluster.local:5001",
		}},
		"people": {"", []string{
			"stratum-people-0.stratum-people.tac-prod-00000001.svc.cluster.local:5001",
			"stratum-people-1.stratum-people.tac-prod-00000001.svc.cluster.local:5001",
		}},
	}
	if len(got) != len(want) {
		t.Errorf("bootstrap tenants = %v, want %d", got, len(want))
	}
	for name, w := range want {
		bt, ok := got[name]
		if !ok {
			t.Errorf("no bootstrap tenant %q", name)
			continue
		}
		if bt.GetTid() != w.tid {
			t.Errorf("%s tid = %q, want %q", name, bt.GetTid(), w.tid)
		}
		if !slices.Equal(bt.GetStratumSeedMembers(), w.seeds) {
			t.Errorf("%s stratum_seed_members = %v, want %v", name, bt.GetStratumSeedMembers(), w.seeds)
		}
	}
	if _, ok := got["restore"]; ok {
		t.Error("restore is a bootstrap tenant: it has no Stratum, and kernel refuses it")
	}
}

// In the one-namespace layout every tenant's Stratum is in that namespace,
// named for its tenant, and a tenant without a tid is listed without one:
// kernel keeps its record, or mints it one.
func TestBootstrapTenantsCarryStratumSeedsInTheOneNamespace(t *testing.T) {
	dir := t.TempDir()
	writeStratumConfigDir(t, dir, stratumTestConfig(t), "{}\n")
	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := bootstrapTenants(t, dc)
	for _, name := range []string{"acme", "beta", "people"} {
		bt, ok := got[name]
		if !ok {
			t.Errorf("no bootstrap tenant %q", name)
			continue
		}
		if bt.GetTid() != "" {
			t.Errorf("%s tid = %q, want none", name, bt.GetTid())
		}
		want := []string{"stratum-" + name + "-0.stratum-" + name + ".schematic.svc.cluster.local:5001"}
		if !slices.Equal(bt.GetStratumSeedMembers(), want) {
			t.Errorf("%s stratum_seed_members = %v, want %v", name, bt.GetStratumSeedMembers(), want)
		}
	}
}

// topology.stratum_members is 1 to 64: kernel carries at most 64 seed members
// a tenant. 0 is unset, which is 1.
func TestStratumMembersOutOfRangeIsRefused(t *testing.T) {
	for _, n := range []int32{-1, 65} {
		cfg := deploymentFixtureConfig()
		cfg.Topology.StratumMembers = n
		_, err := BuildDeployConfigs(writeDeploymentFixture(t, cfg))
		if err == nil || !strings.Contains(err.Error(), "stratum_members") {
			t.Errorf("stratum_members %d: err = %v, want it refused", n, err)
		}
	}
}
