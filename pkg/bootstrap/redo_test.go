package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tacplus-dev/operator/pkg/helm"
	protoKernel "github.com/tacplus-dev/operator/tacplus/types/services/kernel"
)

// redo is a tenant service of the reserved restore tenant (Jody, 2026-10-26):
// one instance, redo-restore, whose kernel delegate is restore's and matches
// the service account in restore's namespace. It is deployed with Stratum,
// whose durability it is.
func TestEnsureServiceDelegatesGivesRedoTheRestoreTenant(t *testing.T) {
	withRegistry(t, registryPinned(map[string]string{"redo": "restore"}, []string{"docmanager"}))
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)
	cfg := tenantTestConfig()
	cfg.Namespace = ""
	cfg.Deployment = "prod"
	cfg.TenantIds = map[string]string{"acme": "a1b2c3d4", "beta": "b1c2d3e4"}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	var redo []SourceDelegate
	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name == "redo" {
			redo = append(redo, d)
		}
	}
	if len(redo) != 1 {
		t.Fatalf("redo delegates = %d, want 1, in restore: %+v", len(redo), redo)
	}
	if redo[0].TenantNamespace != "/tenants/restore" {
		t.Errorf("redo delegate tenant_namespace = %q, want /tenants/restore", redo[0].TenantNamespace)
	}
	ksa := redo[0].KubernetesServiceAccounts
	if len(ksa) != 1 || ksa[0].KubernetesNamespace != "tac-prod-00000002" || ksa[0].KubernetesServiceAccount != "redo-restore" {
		t.Errorf("redo delegate KSAs = %+v, want redo-restore in tac-prod-00000002", ksa)
	}
}

func TestNoRedoDelegateWithoutStratum(t *testing.T) {
	withRegistry(t, registryPinned(map[string]string{"redo": "restore"}, nil))
	cfg := tenantTestConfig()
	cfg.Topology.StratumNodes = 0
	instances, err := DeployedServiceInstances(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range instances {
		if inst.Service == "redo" {
			t.Errorf("redo instance %+v deployed with no Stratum", inst)
		}
	}
}

// The holder carries redo's Config with data_dir where the chart mounts the
// volume, and the chart gets redo-restore's bootstrap config.
func TestDeployConfigsCarryRedo(t *testing.T) {
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
	kernelCfg, err := protoKernel.ConfigFromBytes(dc.BootstrapYAMLs[0])
	if err != nil {
		t.Fatal(err)
	}
	rc := kernelCfg.GetConfigs().GetRedoConfig()
	if rc == nil {
		t.Fatal("kernel's services holder has no redo_config")
	}
	if rc.GetDataDir() != helm.RedoDataDir {
		t.Errorf("redo data_dir = %q, want %q, where the chart mounts its volume", rc.GetDataDir(), helm.RedoDataDir)
	}
	vals := ServiceConfigValues(dc)
	entry, ok := vals["redo-restore"].(map[string]interface{})
	if !ok || entry["config"] == "" {
		t.Errorf("chart values carry no redo-restore config: %v", vals["redo-restore"])
	}
}

// The generator renders redo in restore and its delegate is restore's; redo's
// proto must agree, or kernel would mint it an identity its own registry entry
// contradicts.
func TestEmbeddedRegistryPinsRedoToRestore(t *testing.T) {
	owned, err := tenantOwnedServices()
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := pinnedTenantServices()
	if err != nil {
		t.Fatal(err)
	}
	if !owned["redo"] || pinned["redo"] != "restore" {
		t.Errorf("registry.json: redo tenant_owned=%t pinned_tenant=%q, want true and restore", owned["redo"], pinned["redo"])
	}
}
