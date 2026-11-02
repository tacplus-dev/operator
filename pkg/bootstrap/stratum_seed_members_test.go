package bootstrap

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	protoKernel "github.com/tacplus-dev/operator/tacplus/types/services/kernel"
)

// stratum's Config.seed_members is assert_required since stratum#2: a member
// joins only the addresses it names, and refuses to start without them. Kernel
// seeds stratum's ConfigState from its services holder, so the operator writes
// the members it renders there: every pod of the stratum and stratum-search
// StatefulSets, on the Hazelcast port.
func TestKernelBootstrapCarriesStratumSeedMembers(t *testing.T) {
	dir := t.TempDir()
	cfg := stratumArchiveTestConfig(t)
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
	want := []string{
		"stratum-0.stratum.schematic.svc.cluster.local:5001",
		"stratum-1.stratum.schematic.svc.cluster.local:5001",
		"stratum-2.stratum.schematic.svc.cluster.local:5001",
		"stratum-search-0.stratum-search.schematic.svc.cluster.local:5001",
	}
	if got := kernelCfg.GetConfigs().GetStratumConfig().GetSeedMembers(); !reflect.DeepEqual(got, want) {
		t.Errorf("stratum seed_members = %q, want %q", got, want)
	}
}
