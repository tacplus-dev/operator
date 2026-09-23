package bootstrap

import (
	"fmt"
	"testing"
)

// registryPinned builds a registry.json in which every named service is
// tenant_owned and `pinned` additionally carries pinned_tenant, exactly as
// tacplus emits the pair from the file-level (spec.service) annotation.
func registryPinned(pinned map[string]string, unpinned []string) []byte {
	node := func(name, tenant string) string {
		return fmt.Sprintf(`{"name":%q,"full_path":"services/%s","services":[{"name":%q,"package":%q,"grpc_service":"%s.%s","tenant_owned":true,"pinned_tenant":%q}]}`,
			name, name, name+"Service", name, name, name+"Service", tenant)
	}
	var nodes []string
	for n, tenant := range pinned {
		nodes = append(nodes, node(n, tenant))
	}
	for _, n := range unpinned {
		nodes = append(nodes, node(n, ""))
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

// TestPinnedTenantServicesReadsTheAnnotation pins the field name the operator
// reads, for the same reason TestTenantOwnedServicesReadsTheAnnotation pins
// tenant_owned: a rename on the tacplus side must fail here rather than
// silently turn a pinned service back into a per-tenant one.
func TestPinnedTenantServicesReadsTheAnnotation(t *testing.T) {
	withRegistry(t, registryPinned(map[string]string{"profile": "people"}, []string{"docmanager"}))

	pinned, err := pinnedTenantServices()
	if err != nil {
		t.Fatal(err)
	}
	if pinned["profile"] != "people" {
		t.Errorf("profile pinned tenant = %q, want \"people\": %v", pinned["profile"], pinned)
	}
	if _, ok := pinned["docmanager"]; ok {
		t.Errorf("docmanager read as pinned, but it declares no pinned_tenant: %v", pinned)
	}
}

// TestDeployedServiceInstancesPinsToOneTenant: the delegate side of the pin.
// A pinned service has exactly one instance, in its tenant, so kernel mints one
// delegate for it — not one per configured tenant, which is what would split
// the rows the service exists to hold.
func TestDeployedServiceInstancesPinsToOneTenant(t *testing.T) {
	withRegistry(t, registryPinned(map[string]string{"profile": "people"}, []string{"docmanager"}))
	cfg := tenantTestConfig()
	cfg.Tenants = []string{"acme", "beta", "people"}
	cfg.Topology.AppServices = map[string]int32{"profile": 1, "docmanager": 1}

	instances, err := DeployedServiceInstances(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var profiles, docs []ServiceInstance
	for _, inst := range instances {
		switch inst.Service {
		case "profile":
			profiles = append(profiles, inst)
		case "docmanager":
			docs = append(docs, inst)
		}
	}

	if len(profiles) != 1 {
		t.Fatalf("pinned profile instances = %d, want 1: %+v", len(profiles), profiles)
	}
	if profiles[0].Tenant != "people" {
		t.Errorf("pinned profile instance tenant = %q, want \"people\"", profiles[0].Tenant)
	}
	if got := profiles[0].InstanceName(); got != "profile-people" {
		t.Errorf("pinned profile instance name = %q, want \"profile-people\"", got)
	}

	// The unpinned tenant-owned service in the same fleet still fans out.
	if len(docs) != len(cfg.Tenants) {
		t.Errorf("unpinned docmanager instances = %d, want %d (one per tenant): %+v", len(docs), len(cfg.Tenants), docs)
	}
}
