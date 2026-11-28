package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tacplus-dev/operator/pkg/helm"
	protoKernel "github.com/tacplus-dev/operator/tacplus/types/services/kernel"
)

// redo is one /system service (Jody, 2026-11-07, tacplus-dev/roadmap
// decisions/s2s-never-crosses-tenants-2026-10-31.md, addendum: "It leaves the
// restore tenant"): one instance, redo, whose kernel delegate is the /system
// one (no tenant_namespace) and matches the service account redo in the
// platform namespace. It is deployed with Stratum, whose durability it is:
// always, as the people tenant always has a Stratum.
func TestEnsureServiceDelegatesGivesRedoTheSystemDelegate(t *testing.T) {
	withRegistry(t, registryMarking([]string{"docmanager"}, []string{"redo"}))
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
		t.Fatalf("redo delegates = %d, want 1, the /system one: %+v", len(redo), redo)
	}
	if redo[0].TenantNamespace != "" {
		t.Errorf("redo delegate tenant_namespace = %q, want none (/system)", redo[0].TenantNamespace)
	}
	ksa := redo[0].KubernetesServiceAccounts
	if len(ksa) != 1 || ksa[0].KubernetesNamespace != "tac-prod" || ksa[0].KubernetesServiceAccount != "redo" {
		t.Errorf("redo delegate KSAs = %+v, want redo in tac-prod", ksa)
	}
}

// Every tenant's Stratum, people's too, is admitted to call redo: tenant ->
// /system, the edge stratum's S2S client dependency on redolog.Redo gives its
// delegate in the embedded registry.
func TestEveryStratumDelegateIsAuthorizedForRedo(t *testing.T) {
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)
	cfg := tenantTestConfig()
	cfg.Namespace = ""
	cfg.Deployment = "prod"
	cfg.TenantIds = map[string]string{"acme": "a1b2c3d4", "beta": "b1c2d3e4"}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name != stratumServiceName {
			continue
		}
		seen[d.TenantNamespace] = true
		var redo bool
		for _, as := range d.AuthorizedServices {
			redo = redo || as == redoServiceName
		}
		if !redo {
			t.Errorf("stratum delegate %s authorized_services %v: no redo", d.TenantNamespace, d.AuthorizedServices)
		}
	}
	for _, ns := range []string{"/tenants/acme", "/tenants/beta", "/tenants/people"} {
		if !seen[ns] {
			t.Errorf("no stratum delegate for %s: %v", ns, seen)
		}
	}
}

// The holder carries redo's Config with data_dir where the chart mounts the
// volume, and the chart gets redo's bootstrap config.
func TestDeployConfigsCarryRedo(t *testing.T) {
	dir := t.TempDir()
	cfg := stratumTestConfig(t)
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
	entry, ok := vals["redo"].(map[string]interface{})
	if !ok || entry["config"] == "" {
		t.Errorf("chart values carry no redo config: %v", vals["redo"])
	}
}

// The generator renders redo in the platform namespace and its delegate is
// the /system one; redo's proto must agree (tacplus-dev/redo#19), or kernel
// would mint it an identity its own registry entry contradicts.
func TestEmbeddedRegistryHasRedoAsASystemService(t *testing.T) {
	owned, err := tenantOwnedServices()
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := pinnedTenantServices()
	if err != nil {
		t.Fatal(err)
	}
	if owned["redo"] || pinned["redo"] != "" {
		t.Errorf("registry.json: redo tenant_owned=%t pinned_tenant=%q, want false and none: redo is a /system service", owned["redo"], pinned["redo"])
	}
}
