package bootstrap

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tacplus-dev/operator/pkg/deployment"
	protoKernel "github.com/tacplus-dev/operator/tacplus/types/services/kernel"
	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// coopDeploymentConfig is deploymentFixtureConfig with a second tenant and
// the coop in app_services.
func coopDeploymentConfig() *protoOperator.Config {
	cfg := deploymentFixtureConfig()
	cfg.Tenants = []string{"acme", "globex"}
	cfg.TenantIds = map[string]string{"acme": "a1b2c3d4", "globex": "g1h2i3j4"}
	cfg.Topology.AppServices = map[string]int32{"coop": 1}
	return cfg
}

// writeCoopDeploymentFixture writes coopDeploymentConfig's config dir with a
// services/coop.yaml of body after the mode every coop Config must define.
func writeCoopDeploymentFixture(t *testing.T, body string) string {
	t.Helper()
	dir := writeDeploymentFixture(t, coopDeploymentConfig())
	if err := os.WriteFile(filepath.Join(dir, "services", "coop.yaml"), []byte("mode: MODE_JOBS\n"+body), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func coopPigeonsNamespaces(t *testing.T, dc *DeployConfigs) map[string]string {
	t.Helper()
	kernel, err := protoKernel.ConfigFromBytes(dc.BootstrapYAMLs[0])
	if err != nil {
		t.Fatal(err)
	}
	return kernel.GetConfigs().GetCoopConfig().GetPigeonsNamespaces()
}

// The coop reads its typed Config from kernel's services holder, so that is
// where it learns each tenant's pigeons namespace: the tenant namespaces the
// chart grants its Roles in (deployment.PigeonsNamespaces), keyed by the
// tenant's authorization namespace (tacplus-dev/coop#17).
func TestCoopConfigNamesEachTenantsPigeonsNamespace(t *testing.T) {
	RegisterAppServices([]string{"coop"})
	t.Cleanup(func() { RegisterAppServices(nil) })
	cfg := coopDeploymentConfig()
	dc, err := BuildDeployConfigs(writeCoopDeploymentFixture(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	got := coopPigeonsNamespaces(t, dc)
	want := map[string]string{
		"/tenants/acme":   "tac-prod-a1b2c3d4",
		"/tenants/globex": "tac-prod-g1h2i3j4",
	}
	if !maps.Equal(got, want) {
		t.Errorf("coop_config.pigeons_namespaces = %v, want %v", got, want)
	}
	// The same namespaces as the pigeons RBAC, from the one source.
	rbac := map[string]string{}
	for tenant, ns := range deployment.PigeonsNamespaces(cfg, "") {
		rbac["/tenants/"+tenant] = ns
	}
	if !maps.Equal(got, rbac) {
		t.Errorf("coop_config.pigeons_namespaces = %v, the pigeons RBAC's namespaces %v", got, rbac)
	}
}

// A tenant kernel's records hold, created at run time, gets its Role from the
// reconciler's merged config; the coop must be told its namespace too.
func TestCoopConfigNamesRecordTenantsPigeonsNamespace(t *testing.T) {
	RegisterAppServices([]string{"coop"})
	t.Cleanup(func() { RegisterAppServices(nil) })
	dir := writeCoopDeploymentFixture(t, "")
	dc, err := buildDeployConfigs(dir, nil, []string{"zeta"}, map[string]string{"zeta": "z1y2x3w4"})
	if err != nil {
		t.Fatal(err)
	}
	if got := coopPigeonsNamespaces(t, dc)["/tenants/zeta"]; got != "tac-prod-z1y2x3w4" {
		t.Errorf("record tenant zeta's pigeons namespace = %q, want tac-prod-z1y2x3w4", got)
	}
}

// services/coop.yaml's other fields reach the coop beside the map.
func TestCoopConfigKeepsCoopYAMLBesideThePigeonsNamespaces(t *testing.T) {
	RegisterAppServices([]string{"coop"})
	t.Cleanup(func() { RegisterAppServices(nil) })
	dc, err := BuildDeployConfigs(writeCoopDeploymentFixture(t, "max_live_runs: 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := protoKernel.ConfigFromBytes(dc.BootstrapYAMLs[0])
	if err != nil {
		t.Fatal(err)
	}
	coop := kernel.GetConfigs().GetCoopConfig()
	if coop.GetMaxLiveRuns() != 5 || len(coop.GetPigeonsNamespaces()) != 2 {
		t.Errorf("coop_config = %v, want coop.yaml's max_live_runs and both tenants' namespaces", coop)
	}
}

// With a deployment the operator renders the coop's pigeons namespaces; a
// namespace coop.yaml names of its own would be stale (fleet-seatrials'
// schematic-pigeons, which nothing creates), so generate refuses it rather
// than overwrite it silently.
func TestCoopYAMLPigeonsNamespaceBesideADeploymentIsRefused(t *testing.T) {
	RegisterAppServices([]string{"coop"})
	t.Cleanup(func() { RegisterAppServices(nil) })
	for field, yaml := range map[string]string{
		"pigeons_namespace":  "pigeons_namespace: schematic-pigeons\n",
		"pigeons_namespaces": "pigeons_namespaces:\n  /tenants/acme: somewhere\n",
	} {
		_, err := BuildDeployConfigs(writeCoopDeploymentFixture(t, yaml))
		if err == nil {
			t.Errorf("services/coop.yaml setting %s beside a deployment built", field)
			continue
		}
		if !strings.Contains(err.Error(), "services/coop.yaml") || !strings.Contains(err.Error(), field) {
			t.Errorf("refusing %s: error %q does not name services/coop.yaml and the field", field, err)
		}
	}
}
