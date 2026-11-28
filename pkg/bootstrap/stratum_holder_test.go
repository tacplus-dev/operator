package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	protoKernel "github.com/tacplus-dev/operator/tacplus/types/services/kernel"
	protoStratum "github.com/tacplus-dev/operator/tacplus/types/services/stratum"
)

// stratum's seed_members left its deployment-wide Config for the tenant-scope
// StratumTenantConfig (stratum#26), delivered through TenantConfig.stratum
// from kernel's bootstrap tenants (kernel#34, #2). What is left in the holder has no required field, so
// its entry is defaulted like every other service's: it is no longer dropped
// for want of members to name.
func TestKernelBootstrapCarriesStratumHolderDefaults(t *testing.T) {
	for name, members := range map[string]int32{"one member a tenant": 0, "three members a tenant": 3} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := stratumTestConfig(t)
			cfg.Topology.StratumMembers = members
			if err := SaveSettings(dir, cfg); err != nil {
				t.Fatal(err)
			}
			if err := SaveCrypto(dir, cfg); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "services"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "services", "kernel.yaml"), []byte("{}\n"), 0644); err != nil {
				t.Fatal(err)
			}

			dc, err := BuildDeployConfigs(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(dc.BootstrapYAMLs) == 0 {
				t.Fatal("no kernel bootstrap YAMLs generated")
			}
			kernelCfg, err := protoKernel.ConfigFromBytes(dc.BootstrapYAMLs[0])
			if err != nil {
				t.Fatal(err)
			}
			sc := kernelCfg.GetConfigs().GetStratumConfig()
			if sc == nil {
				t.Fatal("kernel holder has no stratum entry")
			}
			if got, want := sc.GetHazelcastMemoryMb(), int32(protoStratum.DefaultHazelcastMemoryMb); got != want {
				t.Errorf("stratum hazelcast_memory_mb = %d, want default %d", got, want)
			}
		})
	}
}
