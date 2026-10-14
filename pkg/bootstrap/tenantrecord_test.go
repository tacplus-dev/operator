package bootstrap

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protoTenantConfig "gitlab.com/seabeam/schematic/platform/services/operator/model/types/generated/tenant_config"
	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	protoOauth "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/oauth"
)

// betaOauth is the IdP the approved record for beta carries.
func betaOauth() *protoOauth.OAuthTenantConfig {
	return &protoOauth.OAuthTenantConfig{Providers: []*protoOauth.OAuthProvider{{
		Enable:       true,
		IssuerUrl:    "https://idp.beta.example",
		ClientId:     "beta-web",
		ClientSecret: "s",
	}}}
}

// kernelState is a ConfigState holding tenant with status and, when
// withConfig, its config entry carrying oauth.
func kernelState(tenant string, status protoKernel.TenantStatus, withConfig bool, oauth *protoOauth.OAuthTenantConfig) *protoKernel.ConfigState {
	state := &protoKernel.ConfigState{Tenants: []*protoKernel.Tenant{{Name: tenant, Status: status}}}
	if withConfig {
		state.TenantConfigs = []*protoKernel.TenantConfigEntry{{
			TenantName: tenant,
			Config:     &protoTenantConfig.TenantConfig{Oauth: oauth},
		}}
	}
	return state
}

// dirFiles returns every file under dir by its path relative to dir.
func dirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestApproveTenantRecordsMatchesApproveTenant: an approved kernel record
// leaves the config dir byte-for-byte as approve-tenant leaves it for the same
// request filed by hand — operator.yaml tenants (and so the tenant's dogs),
// tenants/<name>/oauth.yaml, namespace.rego and kernel.yaml's delegates.
func TestApproveTenantRecordsMatchesApproveTenant(t *testing.T) {
	request, err := protoOauth.OAuthTenantConfigToYAML(betaOauth())
	if err != nil {
		t.Fatal(err)
	}
	cliDir := approveTestDir(t, true)
	writeTestFile(t, TenantRequestPath(cliDir, "beta"), string(request))
	if err := ApproveTenant(cliDir, "beta"); err != nil {
		t.Fatalf("ApproveTenant: %v", err)
	}

	recordDir := approveTestDir(t, true)
	approved, err := ApproveTenantRecords(recordDir, kernelState("beta", protoKernel.TenantStatus_TENANT_STATUS_ACTIVE, true, betaOauth()))
	if err != nil {
		t.Fatalf("ApproveTenantRecords: %v", err)
	}
	if want := []string{"beta"}; !reflect.DeepEqual(approved, want) {
		t.Errorf("approved = %v, want %v", approved, want)
	}

	want, got := dirFiles(t, cliDir), dirFiles(t, recordDir)
	if !reflect.DeepEqual(got, want) {
		for path := range want {
			if got[path] != want[path] {
				t.Errorf("%s differs:\nrecord path:\n%s\napprove-tenant:\n%s", path, got[path], want[path])
			}
		}
		for path := range got {
			if _, ok := want[path]; !ok {
				t.Errorf("record path wrote %s, approve-tenant did not", path)
			}
		}
	}
}

// TestApproveTenantRecordsWithoutOauthIsAnEmptyRequest: a config entry with no
// oauth slice is the tenant-zero request, `providers: []`.
func TestApproveTenantRecordsWithoutOauthIsAnEmptyRequest(t *testing.T) {
	dir := approveTestDir(t, false)
	if _, err := ApproveTenantRecords(dir, kernelState("beta", protoKernel.TenantStatus_TENANT_STATUS_ACTIVE, true, nil)); err != nil {
		t.Fatalf("ApproveTenantRecords: %v", err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme", "beta"}; !reflect.DeepEqual(cfg.GetTenants(), want) {
		t.Errorf("tenants = %v, want %v", cfg.GetTenants(), want)
	}
	installed, err := protoOauth.OAuthTenantConfigFromBytes([]byte(fileText(t, filepath.Join(TenantConfigDir(dir, "beta"), "oauth.yaml"))))
	if err != nil {
		t.Fatal(err)
	}
	if len(installed.GetProviders()) != 0 || len(installed.GetClaimMatchers()) != 0 {
		t.Errorf("installed oauth.yaml = %v, want an empty request", installed)
	}
}

// TestApproveTenantRecordsIsIdempotent: replaying the same ConfigState — every
// Watch event, every restart — changes nothing once the tenant is approved.
func TestApproveTenantRecordsIsIdempotent(t *testing.T) {
	dir := approveTestDir(t, true)
	state := kernelState("beta", protoKernel.TenantStatus_TENANT_STATUS_ACTIVE, true, betaOauth())
	if _, err := ApproveTenantRecords(dir, state); err != nil {
		t.Fatalf("first ApproveTenantRecords: %v", err)
	}
	before := dirFiles(t, dir)

	approved, err := ApproveTenantRecords(dir, state)
	if err != nil {
		t.Fatalf("second ApproveTenantRecords: %v", err)
	}
	if len(approved) != 0 {
		t.Errorf("replay approved %v again", approved)
	}
	if after := dirFiles(t, dir); !reflect.DeepEqual(after, before) {
		t.Errorf("replay changed the config dir:\nbefore %v\nafter  %v", before, after)
	}
}

// TestApproveTenantRecordsIgnoresUnapproved: a tenant that is not ACTIVE, or
// whose config entry has not landed yet, is not approved — the config dir is
// untouched.
func TestApproveTenantRecordsIgnoresUnapproved(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     protoKernel.TenantStatus
		withConfig bool
	}{
		{"suspended", protoKernel.TenantStatus_TENANT_STATUS_SUSPENDED, true},
		{"unspecified status", protoKernel.TenantStatus_TENANT_STATUS_UNSPECIFIED, true},
		{"active without config entry", protoKernel.TenantStatus_TENANT_STATUS_ACTIVE, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := approveTestDir(t, true)
			before := dirFiles(t, dir)
			approved, err := ApproveTenantRecords(dir, kernelState("beta", tc.status, tc.withConfig, betaOauth()))
			if err != nil {
				t.Fatalf("ApproveTenantRecords: %v", err)
			}
			if len(approved) != 0 {
				t.Errorf("approved %v", approved)
			}
			if after := dirFiles(t, dir); !reflect.DeepEqual(after, before) {
				t.Errorf("config dir changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// TestApproveTenantRecordsRefusalLeavesNoTrace: a record approve-tenant
// refuses leaves the config dir exactly as it was — no request left behind to
// block the next pass — and does not stop another approved tenant.
func TestApproveTenantRecordsRefusalLeavesNoTrace(t *testing.T) {
	dir := approveTestDir(t, true)
	before := dirFiles(t, dir)
	claimsAcme := &protoOauth.OAuthTenantConfig{Providers: []*protoOauth.OAuthProvider{{
		Enable: true, IssuerUrl: "https://idp.acme.example", ClientId: "acme-web", ClientSecret: "s",
	}}}
	state := kernelState("beta", protoKernel.TenantStatus_TENANT_STATUS_ACTIVE, true, claimsAcme)

	approved, err := ApproveTenantRecords(dir, state)
	if err == nil || !strings.Contains(err.Error(), "claimed by both") {
		t.Fatalf("ApproveTenantRecords error = %v, want the IdP refusal", err)
	}
	if len(approved) != 0 {
		t.Errorf("approved %v", approved)
	}
	if after := dirFiles(t, dir); !reflect.DeepEqual(after, before) {
		t.Errorf("refusal changed the config dir:\nbefore %v\nafter  %v", before, after)
	}

	other := kernelState("gamma", protoKernel.TenantStatus_TENANT_STATUS_ACTIVE, true, nil)
	state.Tenants = append(state.Tenants, other.Tenants...)
	state.TenantConfigs = append(state.TenantConfigs, other.TenantConfigs...)
	approved, err = ApproveTenantRecords(dir, state)
	if err == nil {
		t.Fatal("beta's refusal was dropped")
	}
	if want := []string{"gamma"}; !reflect.DeepEqual(approved, want) {
		t.Errorf("approved = %v, want %v despite beta's refusal", approved, want)
	}
}

// TestApproveTenantRecordsLeavesAPendingRequestAlone: a request already filed
// by hand for the same name is not overwritten by the record; a person decides.
func TestApproveTenantRecordsLeavesAPendingRequestAlone(t *testing.T) {
	dir := approveTestDir(t, true)
	pending := "providers: []\n"
	writeTestFile(t, TenantRequestPath(dir, "beta"), pending)
	before := dirFiles(t, dir)

	_, err := ApproveTenantRecords(dir, kernelState("beta", protoKernel.TenantStatus_TENANT_STATUS_ACTIVE, true, betaOauth()))
	if err == nil || !strings.Contains(err.Error(), "already pending") {
		t.Fatalf("ApproveTenantRecords error = %v, want the pending-request refusal", err)
	}
	if after := dirFiles(t, dir); !reflect.DeepEqual(after, before) {
		t.Errorf("config dir changed:\nbefore %v\nafter  %v", before, after)
	}
}
