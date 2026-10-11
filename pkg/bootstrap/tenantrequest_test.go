package bootstrap

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// handRule is a namespace rule tenants/<name>/oauth.yaml cannot express — the
// shape of the live Google binding — so approval must leave it alone.
const handRule = `
namespace := "/tenants/acme" if {
	input.claims.origin_iss == "https://accounts.google.com"
	input.claims.email == "someone@acme.example"
}
`

// operatorComment stands for the hand-written history operator.yaml carries;
// approval must not strip it.
const operatorComment = "# why the tenants are what they are\n"

// approveTestDir is a config dir with one tenant, acme, a tenant-owned dog,
// and — when withOPA — a services/opa policy whose namespace.rego carries a
// hand-written rule.
func approveTestDir(t *testing.T, withOPA bool) string {
	t.Helper()
	withRegistry(t, registryMarking([]string{"dog"}, nil))
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "operator.yaml"),
		"namespace: schematic\n"+operatorComment+"tenants:\n  - acme\ntopology:\n  kernel_nodes: 1\n  app_services:\n    dog: 1\n")
	writeTestFile(t, filepath.Join(dir, "tenants", "acme", "oauth.yaml"),
		"providers:\n  - enable: true\n    issuer_url: https://idp.acme.example\n    client_id: acme-web\n    client_secret: s\n")
	if withOPA {
		writeTestFile(t, filepath.Join(OPAPolicyDir(dir), "tenants", "namespace.rego"),
			tenantNamespaceRegoHeader+handRule+tenantNamespaceRegoFooter)
	}
	return dir
}

func fileText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestApproveTenantCreatesTheShip: approval lists the tenant in operator.yaml,
// installs the request as its oauth.yaml, deploys its tenant-owned dog, and
// appends its IdP rules to namespace.rego without touching the hand rule.
func TestApproveTenantCreatesTheShip(t *testing.T) {
	dir := approveTestDir(t, true)
	request := "providers:\n  - enable: true\n    issuer_url: https://idp.beta.example\n    client_id: beta-web\n    client_secret: s\n"
	writeTestFile(t, TenantRequestPath(dir, "beta"), request)

	if err := ApproveTenant(dir, "beta"); err != nil {
		t.Fatalf("ApproveTenant: %v", err)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme", "beta"}; !reflect.DeepEqual(cfg.GetTenants(), want) {
		t.Errorf("tenants = %v, want %v", cfg.GetTenants(), want)
	}
	if got := fileText(t, filepath.Join(dir, OperatorSettingsFile)); !strings.Contains(got, operatorComment) {
		t.Errorf("approval stripped operator.yaml's comments:\n%s", got)
	}
	if got := fileText(t, filepath.Join(TenantConfigDir(dir, "beta"), "oauth.yaml")); got != request {
		t.Errorf("tenants/beta/oauth.yaml = %q, want the request %q", got, request)
	}
	if _, err := os.Stat(TenantRequestPath(dir, "beta")); !os.IsNotExist(err) {
		t.Errorf("request still pending after approval: %v", err)
	}

	instances, err := DeployedServiceInstances(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var deployed []string
	for _, inst := range instances {
		deployed = append(deployed, inst.InstanceName())
	}
	if !strings.Contains(strings.Join(deployed, ","), "dog-beta") {
		t.Errorf("approved tenant's dog is not deployed: %v", deployed)
	}

	rego := fileText(t, filepath.Join(OPAPolicyDir(dir), "tenants", "namespace.rego"))
	if !strings.Contains(rego, handRule) {
		t.Errorf("approval dropped the hand-written rule:\n%s", rego)
	}
	idp := TenantIdp{IssuerURL: "https://idp.beta.example", ClientID: "beta-web", TenantNamespace: "/tenants/beta"}
	for _, want := range []string{exactNamespaceRule(idp), issuerNamespaceRule(idp)} {
		if !strings.Contains(rego, want) {
			t.Errorf("namespace.rego missing %q:\n%s", want, rego)
		}
	}
}

// TestApproveTenantWithoutOPADirOrIdp: a request with no providers is a
// tenant with no IdP yet, and a config dir with no services/opa gets its policy
// generated at deploy time, so approval creates no policy directory.
func TestApproveTenantWithoutOPADirOrIdp(t *testing.T) {
	dir := approveTestDir(t, false)
	writeTestFile(t, TenantRequestPath(dir, "beta"), "providers: []\n")

	if err := ApproveTenant(dir, "beta"); err != nil {
		t.Fatalf("ApproveTenant: %v", err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme", "beta"}; !reflect.DeepEqual(cfg.GetTenants(), want) {
		t.Errorf("tenants = %v, want %v", cfg.GetTenants(), want)
	}
	if _, err := os.Stat(OPAPolicyDir(dir)); !os.IsNotExist(err) {
		t.Errorf("approval created %s: %v", OPAPolicyDir(dir), err)
	}
}

// TestApproveTenantRefusesAndChangesNothing: every refusal leaves the config
// dir as it was — operator.yaml, the pending request, and namespace.rego.
func TestApproveTenantRefusesAndChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, tenant, request, wantErr string
	}{
		{"invalid name", "Beta_1", "providers: []\n", "tenant name"},
		{"reserved name", "people", "providers: []\n", "reserved"},
		{"existing tenant", "acme", "providers: []\n", "already a tenant"},
		{"object name too long", strings.Repeat("b", 60), "providers: []\n", "object name"},
		{"no request", "beta", "", "no request"},
		{"unparseable request", "beta", "providers: [\n", "parsing"},
		{"IdP claimed by another tenant", "beta",
			"providers:\n  - enable: true\n    issuer_url: https://idp.acme.example\n    client_id: acme-web\n    client_secret: s\n",
			"claimed by both"},
		{"issuer already in namespace.rego", "beta",
			"providers:\n  - enable: true\n    issuer_url: https://accounts.google.com\n    client_id: beta-web\n    client_secret: s\n",
			"by hand"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := approveTestDir(t, true)
			if tc.request != "" {
				writeTestFile(t, TenantRequestPath(dir, tc.tenant), tc.request)
			}
			operatorBefore := fileText(t, filepath.Join(dir, OperatorSettingsFile))
			regoPath := filepath.Join(OPAPolicyDir(dir), "tenants", "namespace.rego")
			regoBefore := fileText(t, regoPath)

			err := ApproveTenant(dir, tc.tenant)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ApproveTenant error = %v, want one containing %q", err, tc.wantErr)
			}
			if got := fileText(t, filepath.Join(dir, OperatorSettingsFile)); got != operatorBefore {
				t.Errorf("operator.yaml changed:\n%s", got)
			}
			if got := fileText(t, regoPath); got != regoBefore {
				t.Errorf("namespace.rego changed:\n%s", got)
			}
			if tc.request != "" && tc.tenant != "acme" {
				if got := fileText(t, TenantRequestPath(dir, tc.tenant)); got != tc.request {
					t.Errorf("pending request = %q, want it untouched %q", got, tc.request)
				}
				if _, err := os.Stat(filepath.Join(TenantConfigDir(dir, tc.tenant), "oauth.yaml")); !os.IsNotExist(err) {
					t.Errorf("refused request was installed: %v", err)
				}
			}
		})
	}
}
