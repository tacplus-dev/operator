package bootstrap

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	protoTenantConfig "gitlab.com/seabeam/schematic/platform/services/operator/model/types/generated/tenant_config"
	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// recordTenantsTestDir is a deployable config dir with one tenant, acme, whose
// tenant-owned dog already has its kernel.yaml delegate.
func recordTenantsTestDir(t *testing.T) string {
	t.Helper()
	withRegistry(t, registryMarking([]string{"dog"}, nil))
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "console.seabeam.cloud",
		SpiffePrefix: "local.dev",
		Tenants:      []string{"acme"},
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			AppServices: map[string]int32{"dog": 1},
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
	kernelYAML, err := yaml.Marshal(&KernelSourceConfig{Delegates: []SourceDelegate{{
		Name:               "dog",
		TenantNamespace:    "/tenants/acme",
		AuthorizedServices: []string{"dog", "kernel"},
		KubernetesServiceAccounts: []SourceKubernetesServiceAccount{{
			KubernetesNamespace:      "schematic",
			KubernetesServiceAccount: "dog-acme",
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "services", "kernel.yaml"), string(kernelYAML))
	return dir
}

// longTenant is 60 characters: a valid DNS-1123 label, but dog-<it>0 is 65.
var longTenant = strings.Repeat("a", 56) + "-com"

// recordTenantsTestState is kernel's ConfigState after a PromoteAccount made
// thescottsweb-com, next to records the operator must not deploy anew: the
// reserved people tenant, a suspended tenant, acme (operator.yaml lists it
// already) and a tenant whose name, a valid label of its own, makes dog's
// object name ("dog-<tenant>0") longer than Kubernetes allows.
func recordTenantsTestState() *protoKernel.ConfigState {
	active := protoKernel.TenantStatus_TENANT_STATUS_ACTIVE
	state := &protoKernel.ConfigState{Tenants: []*protoKernel.Tenant{
		{Name: "thescottsweb-com", Status: active},
		{Name: "people", Status: active},
		{Name: "dormant", Status: protoKernel.TenantStatus_TENANT_STATUS_SUSPENDED},
		{Name: "acme", Status: active},
		{Name: longTenant, Status: active},
	}}
	for _, tn := range state.Tenants {
		state.TenantConfigs = append(state.TenantConfigs, &protoKernel.TenantConfigEntry{
			TenantName: tn.Name,
			Config:     &protoTenantConfig.TenantConfig{},
		})
	}
	return state
}

// TestServiceConfigProviderPicksUpRecordTenants: a tenant kernel records as
// ACTIVE that operator.yaml does not list comes back for the chart and gets the
// kernel delegate approve-tenant would have written for its dog, in memory —
// no tenant file is written for it.
func TestServiceConfigProviderPicksUpRecordTenants(t *testing.T) {
	dir := recordTenantsTestDir(t)
	before := dirFiles(t, dir)

	provider := NewServiceConfigProvider(dir, recordTenantsTestState)
	sc, err := provider()
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"thescottsweb-com"}; !reflect.DeepEqual(sc.Tenants, want) {
		t.Errorf("Tenants = %v, want %v", sc.Tenants, want)
	}
	if sc.RefusedTenants == nil || !strings.Contains(sc.RefusedTenants.Error(), longTenant) {
		t.Errorf("RefusedTenants = %v, want the refusal of %s", sc.RefusedTenants, longTenant)
	}

	kernel0, ok := sc.Values["kernel0"].(map[string]interface{})
	if !ok {
		t.Fatalf("no kernel0 values: %v", sc.Values)
	}
	kernelCfg, err := protoKernel.ConfigFromBytes([]byte(kernel0["config"].(string)))
	if err != nil {
		t.Fatal(err)
	}
	dogs := map[string]int{}
	for _, d := range kernelCfg.GetDelegates() {
		if d.GetName() == "dog" {
			dogs[d.GetTenantNamespace()]++
		}
	}
	if want := map[string]int{"/tenants/acme": 1, "/tenants/thescottsweb-com": 1}; !reflect.DeepEqual(dogs, want) {
		t.Errorf("kernel dog delegates by tenant namespace = %v, want %v", dogs, want)
	}

	// The files approve-tenant writes stay as they were. (The provider itself
	// rewrites crypto.yaml and ca.pem every pass, with or without records.)
	after := dirFiles(t, dir)
	for _, f := range []string{"operator.yaml", filepath.Join("services", "kernel.yaml")} {
		if after[f] != before[f] {
			t.Errorf("%s changed", f)
		}
	}
	for f := range after {
		if strings.HasPrefix(f, "tenants"+string(filepath.Separator)) || strings.HasPrefix(f, TenantRequestsDir+string(filepath.Separator)) {
			t.Errorf("%s written", f)
		}
	}
}

// TestServiceConfigProviderWithoutRecords: no records source is today's
// provider — only operator.yaml's tenants.
func TestServiceConfigProviderWithoutRecords(t *testing.T) {
	dir := recordTenantsTestDir(t)
	sc, err := NewServiceConfigProvider(dir, nil)()
	if err != nil {
		t.Fatal(err)
	}
	if sc.Tenants != nil || sc.RefusedTenants != nil {
		t.Errorf("Tenants = %v, RefusedTenants = %v, want neither", sc.Tenants, sc.RefusedTenants)
	}
}

// TestRecordTenantsIsSorted: ConfigState lists tenants in whatever order
// kernel's map yields them, and the chart renders tenants in list order, so an
// unsorted list would re-render — and upgrade the release — on every pass.
func TestRecordTenantsIsSorted(t *testing.T) {
	withRegistry(t, registryMarking([]string{"dog"}, nil))
	cfg := &protoOperator.Config{Topology: &protoOperator.Topology{AppServices: map[string]int32{"dog": 1}}}
	active := protoKernel.TenantStatus_TENANT_STATUS_ACTIVE
	state := &protoKernel.ConfigState{Tenants: []*protoKernel.Tenant{
		{Name: "zeta", Status: active}, {Name: "alpha", Status: active}, {Name: "mid", Status: active},
	}}
	got, err := recordTenants(cfg, state)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "mid", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("recordTenants = %v, want %v", got, want)
	}
}
