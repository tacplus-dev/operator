package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// withRegistry swaps the embedded tacplus registry for a fixture and restores
// it when the test ends.
//
// The registry the operator embeds today marks nothing tenant_owned — no
// service in platform/proto declares the annotation yet, and proto3 omits a
// false bool from registry.json entirely — so the only way to exercise the
// tenant-owned branch against the real reader is to hand it a registry that
// does. The fixture is shaped exactly like a real registry node tree.
func withRegistry(t *testing.T, data []byte) {
	t.Helper()
	saved := registryJSON
	registryJSON = data
	t.Cleanup(func() { registryJSON = saved })
}

// registryMarking builds a registry.json whose "services/<name>" nodes carry
// the tenant_owned annotation exactly as tacplus emits it: the flag is
// file-level, so it lands on every ServiceInfo the service's package produces.
func registryMarking(tenantOwned []string, shared []string) []byte {
	node := func(name string, owned bool) string {
		return fmt.Sprintf(`{"name":%q,"full_path":"services/%s","services":[{"name":%q,"package":%q,"grpc_service":"%s.%s","tenant_owned":%t}]}`,
			name, name, name+"Service", name, name, name+"Service", owned)
	}
	var nodes []string
	for _, n := range tenantOwned {
		nodes = append(nodes, node(n, true))
	}
	for _, n := range shared {
		nodes = append(nodes, node(n, false))
	}
	out := `{"name":"root","full_path":"","children":[`
	for i, n := range nodes {
		if i > 0 {
			out += ","
		}
		out += n
	}
	return []byte(out + `]}`)
}

func tenantTestConfig() *protoOperator.Config {
	return &protoOperator.Config{
		Namespace: "schematic",
		Tenants:   []string{"acme", "beta"},
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			EventbusNodes: 1,
			GatewayNodes:  1,
			OpaNodes:      1,
			OauthNodes:    1,
			AppServices:   map[string]int32{"docmanager": 1, "tasks": 1},
		},
	}
}

// TestTenantOwnedServicesReadsTheAnnotation pins the field name the operator
// reads. If tacplus ever renames ServiceInfo.tenant_owned, this fails here
// rather than silently deploying every tenant-owned service as shared.
func TestTenantOwnedServicesReadsTheAnnotation(t *testing.T) {
	withRegistry(t, registryMarking([]string{"docmanager"}, []string{"tasks"}))

	owned, err := tenantOwnedServices()
	if err != nil {
		t.Fatal(err)
	}
	if !owned["docmanager"] {
		t.Errorf("docmanager not read as tenant_owned: %v", owned)
	}
	if owned["tasks"] {
		t.Errorf("tasks read as tenant_owned, but its annotation is false: %v", owned)
	}
}

// TestDeployedServiceInstancesExpandsOnlyTenantOwnedServices verifies the
// (service, tenant) fan-out: the tenant-owned app service gets one instance per
// tenant, every other deployed service keeps exactly one shared instance.
func TestDeployedServiceInstancesExpandsOnlyTenantOwnedServices(t *testing.T) {
	withRegistry(t, registryMarking([]string{"docmanager"}, []string{"tasks"}))
	cfg := tenantTestConfig()

	instances, err := DeployedServiceInstances(cfg)
	if err != nil {
		t.Fatal(err)
	}

	byService := map[string][]ServiceInstance{}
	for _, inst := range instances {
		byService[inst.Service] = append(byService[inst.Service], inst)
	}

	docs := byService["docmanager"]
	if len(docs) != 2 {
		t.Fatalf("docmanager instances = %d, want 2 (one per tenant): %+v", len(docs), docs)
	}
	want := []struct{ name, namespace string }{
		{"docmanager-acme", "/tenants/acme"},
		{"docmanager-beta", "/tenants/beta"},
	}
	for i, w := range want {
		if docs[i].InstanceName() != w.name || docs[i].TenantNamespace() != w.namespace {
			t.Errorf("docmanager instance %d = (%q, %q), want (%q, %q)",
				i, docs[i].InstanceName(), docs[i].TenantNamespace(), w.name, w.namespace)
		}
	}

	// Every other deployed service is unchanged: one instance, no tenant. redo
	// is restore's one instance (TestEnsureServiceDelegatesGivesRedoTheRestoreTenant)
	// and stratum one per tenant (TestEnsureServiceDelegatesGivesEachTenantItsStratum).
	for svc, insts := range byService {
		if svc == "docmanager" || svc == redoServiceName || svc == stratumServiceName {
			continue
		}
		if len(insts) != 1 {
			t.Errorf("shared service %q has %d instances, want 1: %+v", svc, len(insts), insts)
		}
		if insts[0].Tenant != "" || insts[0].InstanceName() != svc || insts[0].TenantNamespace() != "" {
			t.Errorf("shared service %q instance = %+v, want the bare service with no tenant", svc, insts[0])
		}
	}
}

// TestEnsureServiceDelegatesOnePerTenantForTenantOwnedService is the delegate
// half of the goal: one delegate per (tenant, tenant-owned service), all
// sharing the service's name — because the delegate name is the service path
// auth_services patterns match — and separated by tenant_namespace, which is
// how kernel keys them since goal item 2.
func TestEnsureServiceDelegatesOnePerTenantForTenantOwnedService(t *testing.T) {
	withRegistry(t, registryMarking([]string{"docmanager"}, []string{"tasks"}))
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)
	cfg := tenantTestConfig()

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	var docs []SourceDelegate
	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name == "docmanager" {
			docs = append(docs, d)
		}
	}
	if len(docs) != 2 {
		t.Fatalf("delegates named docmanager = %d, want 2 (one per tenant); the name-keyed map collapsed them: %+v", len(docs), docs)
	}

	got := map[string]string{} // tenant_namespace -> service account
	for _, d := range docs {
		if len(d.KubernetesServiceAccounts) != 1 {
			t.Fatalf("delegate %+v: want exactly one kubernetes_service_accounts matcher", d)
		}
		ksa := d.KubernetesServiceAccounts[0]
		if ksa.KubernetesNamespace != "schematic" {
			t.Errorf("delegate %+v KSA namespace = %q, want %q", d, ksa.KubernetesNamespace, "schematic")
		}
		got[d.TenantNamespace] = ksa.KubernetesServiceAccount
	}
	want := map[string]string{
		"/tenants/acme": "docmanager-acme",
		"/tenants/beta": "docmanager-beta",
	}
	for ns, sa := range want {
		if got[ns] != sa {
			t.Errorf("delegate tenant_namespace %q -> service account %q, want %q (got %v)", ns, got[ns], sa, got)
		}
	}
}

// TestEnsureServiceDelegatesLeavesSharedServicesAlone is criterion 3: a service
// registry.json does NOT mark tenant_owned keeps exactly the single shared
// delegate it gets today, even in a cluster that lists tenants. Stratum and
// redo are the platform's tenant instances
// (TestEnsureServiceDelegatesGivesEachTenantItsStratum,
// TestEnsureServiceDelegatesGivesRedoTheRestoreTenant).
func TestEnsureServiceDelegatesLeavesSharedServicesAlone(t *testing.T) {
	withRegistry(t, registryMarking([]string{"docmanager"}, []string{"tasks"}))
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)
	cfg := tenantTestConfig()

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	counts := map[string]int{}
	for _, d := range loadDelegates(t, kernelPath) {
		counts[d.Name]++
		if d.Name == "docmanager" || d.Name == redoServiceName || d.Name == stratumServiceName {
			continue
		}
		if d.TenantNamespace != "" {
			t.Errorf("shared delegate %q has tenant_namespace %q, want empty", d.Name, d.TenantNamespace)
		}
		wantKSA := delegateServiceAccountName(cfg, ServiceInstance{Service: d.Name})
		if len(d.KubernetesServiceAccounts) != 1 || d.KubernetesServiceAccounts[0].KubernetesServiceAccount != wantKSA {
			t.Errorf("shared delegate %q KSAs = %+v, want the single %q", d.Name, d.KubernetesServiceAccounts, wantKSA)
		}
	}

	var multi []string
	for name, n := range counts {
		if name != "docmanager" && name != stratumServiceName && n != 1 {
			multi = append(multi, fmt.Sprintf("%s=%d", name, n))
		}
	}
	sort.Strings(multi)
	if len(multi) > 0 {
		t.Errorf("shared services got more than one delegate each: %v", multi)
	}
	if counts["tasks"] != 1 {
		t.Errorf("tasks delegates = %d, want 1 (it is not tenant_owned)", counts["tasks"])
	}
}

// TestEnsureServiceDelegatesTenantOwnedIsIdempotent guards the re-key: with the
// map keyed by name alone, the second run saw one docmanager and rewrote the
// other tenant's matcher onto it.
func TestEnsureServiceDelegatesTenantOwnedIsIdempotent(t *testing.T) {
	withRegistry(t, registryMarking([]string{"docmanager"}, []string{"tasks"}))
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)
	cfg := tenantTestConfig()

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}
	first := loadDelegates(t, kernelPath)
	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}
	second := loadDelegates(t, kernelPath)

	if len(first) != len(second) {
		t.Fatalf("delegate count changed on re-run: %d -> %d", len(first), len(second))
	}
	for i := range first {
		if fmt.Sprintf("%+v", first[i]) != fmt.Sprintf("%+v", second[i]) {
			t.Errorf("delegate %d changed on re-run:\n first = %+v\nsecond = %+v", i, first[i], second[i])
		}
	}
}

// TestEnsureServiceDelegatesAddsATenantToAnExistingTenantOwnedService is the
// direct proof of the re-key. kernel.yaml already holds acme's docmanager
// delegate; a second tenant is added to operator.yaml. With `existing` keyed by
// name alone, beta's pass FINDS acme's delegate and rewrites its service-account
// matcher to docmanager-beta — one delegate, the wrong tenant, and acme's pod
// silently unable to authenticate.
func TestEnsureServiceDelegatesAddsATenantToAnExistingTenantOwnedService(t *testing.T) {
	withRegistry(t, registryMarking([]string{"docmanager"}, []string{"tasks"}))
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, []SourceDelegate{{
		Name:               "docmanager",
		TenantNamespace:    "/tenants/acme",
		AuthorizedServices: []string{"docmanager", "kernel"},
		KubernetesServiceAccounts: []SourceKubernetesServiceAccount{{
			KubernetesNamespace:      "schematic",
			KubernetesServiceAccount: "docmanager-acme",
		}},
	}})
	cfg := tenantTestConfig()

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name != "docmanager" {
			continue
		}
		if len(d.KubernetesServiceAccounts) != 1 {
			t.Fatalf("delegate %+v: want exactly one kubernetes_service_accounts matcher", d)
		}
		got[d.TenantNamespace] = d.KubernetesServiceAccounts[0].KubernetesServiceAccount
	}
	want := map[string]string{
		"/tenants/acme": "docmanager-acme",
		"/tenants/beta": "docmanager-beta",
	}
	if len(got) != len(want) {
		t.Fatalf("docmanager delegates by tenant_namespace = %v, want %v", got, want)
	}
	for ns, sa := range want {
		if got[ns] != sa {
			t.Errorf("tenant_namespace %q -> service account %q, want %q", ns, got[ns], sa)
		}
	}
}

// TestTenantListComesFromOperatorYAML closes the loop on the source of truth:
// the tenants are read out of operator.yaml by the ordinary config loader, not
// from a second config file, kernel state, or a live query.
func TestTenantListComesFromOperatorYAML(t *testing.T) {
	withRegistry(t, registryMarking([]string{"docmanager"}, nil))
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)

	operatorYAML := `fqdn: cluster.example.com
namespace: schematic
tenants:
  - acme
  - beta
topology:
  kernel_nodes: 1
  gateway_nodes: 1
  app_services:
    docmanager: 1
`
	if err := os.WriteFile(filepath.Join(dir, OperatorSettingsFile), []byte(operatorYAML), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.GetTenants(); len(got) != 2 || got[0] != "acme" || got[1] != "beta" {
		t.Fatalf("operator.yaml tenants = %v, want [acme beta]", got)
	}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}
	var namespaces []string
	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name == "docmanager" {
			namespaces = append(namespaces, d.TenantNamespace)
		}
	}
	sort.Strings(namespaces)
	if len(namespaces) != 2 || namespaces[0] != "/tenants/acme" || namespaces[1] != "/tenants/beta" {
		t.Fatalf("docmanager delegate tenant_namespaces = %v, want [/tenants/acme /tenants/beta]", namespaces)
	}
}

// Stratum is per tenant (#2): one delegate per tenant that has a Stratum,
// every listed tenant but restore, and people, each with its tenant's
// namespace and the stratum-<tenant> service account its StatefulSet runs as.
// No shared /system delegate: stratum only runs as a tenant's
// (spiffe://<td>/tenants/<tid>/services/stratum).
func TestEnsureServiceDelegatesGivesEachTenantItsStratum(t *testing.T) {
	withRegistry(t, registryMarking([]string{"stratum", "docmanager"}, []string{"tasks"}))
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)
	cfg := tenantTestConfig()
	cfg.Namespace = ""
	cfg.Deployment = "prod"
	cfg.Tenants = append(cfg.Tenants, "restore")
	cfg.TenantIds = map[string]string{"acme": "a1b2c3d4", "beta": "b1c2d3e4"}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	stratum := map[string]SourceDelegate{}
	docs := 0
	for _, d := range loadDelegates(t, kernelPath) {
		switch d.Name {
		case "stratum":
			stratum[d.TenantNamespace] = d
		case "docmanager":
			docs++
		}
	}
	want := map[string][2]string{
		"/tenants/acme":   {"tac-prod-a1b2c3d4", "stratum-acme"},
		"/tenants/beta":   {"tac-prod-b1c2d3e4", "stratum-beta"},
		"/tenants/people": {"tac-prod-00000001", "stratum-people"},
	}
	if len(stratum) != len(want) {
		t.Fatalf("stratum delegates = %v, want one each for %v", stratum, want)
	}
	for tns, w := range want {
		d, ok := stratum[tns]
		if !ok {
			t.Errorf("no stratum delegate for %s", tns)
			continue
		}
		if ksa := d.KubernetesServiceAccounts; len(ksa) != 1 || ksa[0].KubernetesNamespace != w[0] || ksa[0].KubernetesServiceAccount != w[1] {
			t.Errorf("%s stratum delegate KSAs = %+v, want %s in %s", tns, ksa, w[1], w[0])
		}
	}
	if docs != 3 {
		t.Errorf("docmanager delegates = %d, want 3: a tenant-owned app service still fans out to every listed tenant", docs)
	}
}
