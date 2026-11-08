package bootstrap

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	wrapperX509 "github.com/tacplus-dev/operator/tacplus/chassis/x509"
	protoKernel "github.com/tacplus-dev/operator/tacplus/types/services/kernel"
	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// deploymentFixtureConfig is a deployment, prod, with one tenant.
func deploymentFixtureConfig() *protoOperator.Config {
	return &protoOperator.Config{
		Deployment: "prod",
		Fqdn:       "prod.example.com",
		Tenants:    []string{"acme"},
		TenantIds:  map[string]string{"acme": "a1b2c3d4"},
		Topology: &protoOperator.Topology{
			KernelNodes:   2,
			GatewayNodes:  1,
			EventbusNodes: 1,
			OauthNodes:    1,
			OpaNodes:      1,
			StratumNodes:  2,
		},
	}
}

// writeDeploymentFixture writes a config dir for cfg, its crypto minted as
// init cluster mints it.
func writeDeploymentFixture(t *testing.T, cfg *protoOperator.Config) string {
	t.Helper()
	dir := t.TempDir()
	if err := GenerateCrypto(cfg, int(cfg.GetTopology().GetKernelNodes()), 0, cfg.Fqdn, "local.dev"); err != nil {
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
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Every endpoint the operator writes names the service in full: a tenant
// namespace's pod resolves a short name in its own namespace, never in
// tac-<deployment>.
func TestDeploymentConfigsUseFullServiceNames(t *testing.T) {
	dc, err := BuildDeployConfigs(writeDeploymentFixture(t, deploymentFixtureConfig()))
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := protoKernel.ConfigFromBytes(dc.BootstrapYAMLs[0])
	if err != nil {
		t.Fatal(err)
	}
	var members []string
	for _, m := range kernel.GetMembers() {
		members = append(members, m.GetHost())
	}
	want := []string{"kernel-0.kernel.tac-prod.svc.cluster.local", "kernel-1.kernel.tac-prod.svc.cluster.local"}
	if !slices.Equal(members, want) {
		t.Errorf("kernel members = %v, want %v", members, want)
	}
	for name, cfgYAML := range map[string][]byte{"gateway": dc.GatewayConfig, "eventbus": dc.EventbusConfig, "operator": dc.OperatorBootstrapConfig} {
		for _, host := range want {
			if !bytes.Contains(cfgYAML, []byte("host: "+host)) {
				t.Errorf("%s config does not name kernel endpoint %s:\n%s", name, host, cfgYAML)
			}
		}
		if bytes.Contains(cfgYAML, []byte("host: kernel-0.kernel\n")) {
			t.Errorf("%s config still names a short kernel endpoint:\n%s", name, cfgYAML)
		}
	}
}

// Kernel's certificate covers the full names its clients dial, and a cert
// minted before the deployment had its namespace is reissued.
func TestKernelCertCoversFullServiceNames(t *testing.T) {
	cfg := deploymentFixtureConfig()
	if err := GenerateCrypto(cfg, 2, 0, cfg.Fqdn, "local.dev"); err != nil {
		t.Fatal(err)
	}
	for i, host := range []string{"kernel-0.kernel.tac-prod.svc.cluster.local", "kernel-1.kernel.tac-prod.svc.cluster.local"} {
		cc, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.NodeTls[i])
		if err != nil {
			t.Fatal(err)
		}
		if names := cc.Leaf().X509Certificate().DNSNames; !slices.Contains(names, host) || !slices.Contains(names, "kernel.tac-prod.svc.cluster.local") {
			t.Errorf("kernel-%d cert SANs %v lack %s", i, names, host)
		}
	}
	uri, err := KernelServiceSpiffeURI("local.dev")
	if err != nil {
		t.Fatal(err)
	}
	if KernelTLSNeedsRegeneration(cfg, 2, 0, uri) {
		t.Error("a fresh kernel cert is reissued")
	}
	moved := proto.Clone(cfg).(*protoOperator.Config)
	moved.Deployment = "dev"
	if !KernelTLSNeedsRegeneration(moved, 2, 0, uri) {
		t.Error("a kernel cert for tac-prod is kept for tac-dev")
	}
}

// Each delegate's Kubernetes matcher names the namespace its instance runs
// in: a tenant instance its tenant namespace, a shared service tac-<deployment>,
// and the operator its own service account there, operator.
func TestDelegatesMatchTheNamespaceEachInstanceRunsIn(t *testing.T) {
	withRegistry(t, registryMarking([]string{"docmanager"}, []string{"tasks"}))
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)
	cfg := tenantTestConfig()
	cfg.Namespace = ""
	cfg.Deployment = "prod"
	cfg.TenantIds = map[string]string{"acme": "a1b2c3d4", "beta": "b1c2d3e4"}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}
	got := map[string][2]string{} // name|tenant -> namespace, service account
	for _, d := range loadDelegates(t, kernelPath) {
		if len(d.KubernetesServiceAccounts) != 1 {
			t.Fatalf("delegate %+v: want exactly one matcher", d)
		}
		ksa := d.KubernetesServiceAccounts[0]
		got[d.Name+"|"+d.TenantNamespace] = [2]string{ksa.KubernetesNamespace, ksa.KubernetesServiceAccount}
	}
	for key, want := range map[string][2]string{
		"docmanager|/tenants/acme": {"tac-prod-a1b2c3d4", "docmanager-acme"},
		"docmanager|/tenants/beta": {"tac-prod-b1c2d3e4", "docmanager-beta"},
		"tasks|":                   {"tac-prod", "tasks"},
		"gateway|":                 {"tac-prod", "gateway"},
		"operator|":                {"tac-prod", "operator"},
	} {
		if got[key] != want {
			t.Errorf("delegate %s matches %v, want %v", key, got[key], want)
		}
	}
}

// Kernel keeps the tid the operator rendered the tenant's namespace with.
func TestKernelBootstrapListsTenantsWithTheirTids(t *testing.T) {
	dc, err := BuildDeployConfigs(writeDeploymentFixture(t, deploymentFixtureConfig()))
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := protoKernel.ConfigFromBytes(dc.BootstrapYAMLs[0])
	if err != nil {
		t.Fatal(err)
	}
	tenants := kernel.GetTenants()
	if len(tenants) != 1 || tenants[0].GetName() != "acme" || tenants[0].GetTid() != "a1b2c3d4" {
		t.Errorf("kernel tenants = %v, want acme with tid a1b2c3d4", tenants)
	}
}

func TestInvalidDeploymentFailsBuild(t *testing.T) {
	cfg := deploymentFixtureConfig()
	dir := writeDeploymentFixture(t, cfg)
	bad := proto.Clone(cfg).(*protoOperator.Config)
	bad.TenantIds = nil
	if err := SaveSettings(dir, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildDeployConfigs(dir); err == nil {
		t.Error("a tenant with no tid in a deployment built")
	}
}

// A tenant kernel records carries its tid to the chart.
func TestRecordTenantsCarryTheirTids(t *testing.T) {
	cfg := deploymentFixtureConfig()
	state := &protoKernel.ConfigState{Tenants: []*protoKernel.Tenant{
		{Name: "acme", Status: protoKernel.TenantStatus_TENANT_STATUS_ACTIVE, Tid: "a1b2c3d4"},
		{Name: "zeta", Status: protoKernel.TenantStatus_TENANT_STATUS_ACTIVE, Tid: "z1y2x3w4"},
		{Name: "people", Status: protoKernel.TenantStatus_TENANT_STATUS_ACTIVE, Tid: "00000001"},
	}}
	tenants, tids, err := recordTenants(cfg, state)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tenants, []string{"zeta"}) || tids["zeta"] != "z1y2x3w4" || len(tids) != 1 {
		t.Errorf("recordTenants = %v %v, want [zeta] with its tid", tenants, tids)
	}
	// In a deployment a record with no tid has no namespace to render into.
	state.Tenants[1].Tid = ""
	if tenants, _, err := recordTenants(cfg, state); err == nil || len(tenants) != 0 {
		t.Errorf("a record tenant without a tid was deployed: %v, %v", tenants, err)
	}
}

// The provider says when kernel's records were read: only then may the
// reconciler delete a tenant's namespace.
func TestProviderReportsWhetherRecordsWereRead(t *testing.T) {
	dir := writeDeploymentFixture(t, deploymentFixtureConfig())
	unread, err := NewServiceConfigProvider(dir, func() *protoKernel.ConfigState { return nil })()
	if err != nil {
		t.Fatal(err)
	}
	if unread.TenantsComplete {
		t.Error("no records read, yet TenantsComplete")
	}
	read, err := NewServiceConfigProvider(dir, func() *protoKernel.ConfigState {
		return &protoKernel.ConfigState{Tenants: []*protoKernel.Tenant{
			{Name: "people", Status: protoKernel.TenantStatus_TENANT_STATUS_ACTIVE, Tid: "00000001"},
			{Name: "zeta", Status: protoKernel.TenantStatus_TENANT_STATUS_ACTIVE, Tid: "z1y2x3w4"},
		}}
	})()
	if err != nil {
		t.Fatal(err)
	}
	if !read.TenantsComplete || read.TenantIds["zeta"] != "z1y2x3w4" {
		t.Errorf("records read: TenantsComplete = %v, TenantIds = %v", read.TenantsComplete, read.TenantIds)
	}
}
