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
// (kernel#34, then #2). What is left in the holder has no required field, so
// its entry is defaulted like every other service's, whether or not the fleet
// renders stratum: it is no longer dropped for want of members to name.
func TestKernelBootstrapCarriesStratumHolderDefaults(t *testing.T) {
	for name, stratumNodes := range map[string]int32{"stratum rendered": 3, "no stratum": 0} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := stratumArchiveTestConfig(t)
			cfg.Topology.StratumNodes = stratumNodes
			if stratumNodes == 0 {
				cfg.Topology.StratumSearchNodes = 0
			}
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
