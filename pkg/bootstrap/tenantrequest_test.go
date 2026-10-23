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

// handRoleRule is a hand-written role grant in grants/static.rego; generated
// role rules must leave it alone.
const handRoleRule = `
roles := {"ci"} if {
	input.claims.sub == "ci-bot"
}
`

// operatorComment stands for the hand-written history operator.yaml carries;
// approval must not strip it.
const operatorComment = "# why the tenants are what they are\n"

// approveTestDir is a config dir with one tenant, acme, a tenant-owned dog,
// and — when withOPA — a services/opa policy whose namespace.rego and
// grants/static.rego each carry a hand-written rule.
func approveTestDir(t *testing.T, withOPA bool) string {
	t.Helper()
	withRegistry(t, registryMarking([]string{"dog"}, nil))
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "operator.yaml"),
		"namespace: schematic\n"+operatorComment+"tenants:\n  - acme\ntopology:\n  kernel_nodes: 1\n  app_services:\n    dog: 1\n")
	writeTestFile(t, filepath.Join(dir, "tenants", "acme", "oauth.yaml"),
		"providers:\n  - enable: true\n    issuer_url: https://idp.acme.example\n    client_id: acme-web\n    client_secret: s\n")
	setupKernelYAML(t, dir, approveTestDelegates())
	if withOPA {
		writeTestFile(t, filepath.Join(OPAPolicyDir(dir), "tenants", "namespace.rego"),
			tenantNamespaceRegoHeader+handRule+tenantNamespaceRegoFooter)
		writeTestFile(t, filepath.Join(OPAPolicyDir(dir), "grants", "static.rego"), defaultGrantsRego+handRoleRule)
	}
	return dir
}

// approveTestDelegates is approveTestDir's kernel.yaml: acme's dog delegate,
// deliberately stale (generate would reconcile its authorized_services), and a
// hand-added delegate no service deploys. Approval must leave both alone.
func approveTestDelegates() []SourceDelegate {
	return []SourceDelegate{
		{
			Name:               "dog",
			TenantNamespace:    "/tenants/acme",
			AuthorizedServices: []string{"dog", "stale"},
			KubernetesServiceAccounts: []SourceKubernetesServiceAccount{{
				KubernetesNamespace:      "schematic",
				KubernetesServiceAccount: "dog-acme",
			}},
		},
		{
			Name:               "apikey-ci",
			AuthorizedServices: []string{"dog"},
			CustomClaims:       map[string]string{"role": "ci"},
		},
	}
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

// TestApproveTenantRendersClaimMatcherRule: a Captain-filed request has no
// providers and one claim matcher on a shared cluster IdP. Approval appends
// the (issuer, claim) rule even though the hand rule already names that
// issuer, and adds no issuer-only rule for the shared issuer.
func TestApproveTenantRendersClaimMatcherRule(t *testing.T) {
	dir := approveTestDir(t, true)
	writeTestFile(t, TenantRequestPath(dir, "beta"),
		"providers: []\nclaim_matchers:\n  - provider_issuer_url: https://accounts.google.com\n    claim_key: hd\n    claim_value: beta.example\n")

	if err := ApproveTenant(dir, "beta"); err != nil {
		t.Fatalf("ApproveTenant: %v", err)
	}

	rego := fileText(t, filepath.Join(OPAPolicyDir(dir), "tenants", "namespace.rego"))
	if !strings.Contains(rego, handRule) {
		t.Errorf("approval dropped the hand-written rule:\n%s", rego)
	}
	want := "namespace := \"/tenants/beta\" if {\n\tinput.claims.provider_iss == \"https://accounts.google.com\"\n\tinput.claims[\"hd\"] == \"beta.example\"\n}"
	if !strings.Contains(rego, want) {
		t.Errorf("namespace.rego missing claim rule:\n%s\n\ngot:\n%s", want, rego)
	}
	if strings.Contains(rego, "\tinput.claims.origin_iss == \"https://accounts.google.com\"\n}") {
		t.Errorf("a shared cluster issuer got an issuer-only rule:\n%s", rego)
	}
}

// TestApproveTenantRefusesClaimMatcherOnIssuerOnlyIssuer: an issuer-only rule
// would match the claim-matched tenant's callers too, and two complete rules
// with different values fail at eval, so approval refuses and writes nothing.
func TestApproveTenantRefusesClaimMatcherOnIssuerOnlyIssuer(t *testing.T) {
	dir := approveTestDir(t, true)
	regoPath := filepath.Join(OPAPolicyDir(dir), "tenants", "namespace.rego")
	writeTestFile(t, regoPath, tenantNamespaceRegoHeader+issuerNamespaceRule(TenantIdp{
		IssuerURL: "https://accounts.google.com", TenantNamespace: "/tenants/acme",
	})+tenantNamespaceRegoFooter)
	regoBefore := fileText(t, regoPath)
	writeTestFile(t, TenantRequestPath(dir, "beta"),
		"claim_matchers:\n  - provider_issuer_url: https://accounts.google.com\n    claim_key: hd\n    claim_value: beta.example\n")

	if err := ApproveTenant(dir, "beta"); err == nil || !strings.Contains(err.Error(), "issuer-only") {
		t.Fatalf("ApproveTenant error = %v, want one containing %q", err, "issuer-only")
	}
	if got := fileText(t, regoPath); got != regoBefore {
		t.Errorf("namespace.rego changed:\n%s", got)
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
		{"public mail domain", "beta",
			"claim_matchers:\n  - provider_issuer_url: https://accounts.google.com\n    claim_key: email_domain\n    claim_value: gmail.com\n",
			"public mail domain"},
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

// TestApproveTenantWritesExactlyTheNewTenantsDelegates: approval adds one
// kernel delegate per tenant-owned service for the new tenant — the same
// delegate generate would write for it — and changes nothing else in
// kernel.yaml: acme's stale delegate and the hand-added one stay as they were.
func TestApproveTenantWritesExactlyTheNewTenantsDelegates(t *testing.T) {
	dir := approveTestDir(t, false)
	writeTestFile(t, TenantRequestPath(dir, "beta"), "providers: []\n")
	kernelPath := filepath.Join(dir, "services", "kernel.yaml")

	if err := ApproveTenant(dir, "beta"); err != nil {
		t.Fatalf("ApproveTenant: %v", err)
	}

	got := loadDelegates(t, kernelPath)
	before := approveTestDelegates()
	if len(got) != len(before)+1 {
		t.Fatalf("kernel.yaml delegates after approval = %+v, want the %d existing plus one for dog-beta", got, len(before))
	}
	if !reflect.DeepEqual(got[:len(before)], before) {
		t.Errorf("approval changed existing delegates:\n got %+v\nwant %+v", got[:len(before)], before)
	}

	// The oracle is generate's own writer, run over the approved config.
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	oracleDir := t.TempDir()
	oraclePath := setupKernelYAML(t, oracleDir, nil)
	if err := EnsureServiceDelegates(oracleDir, cfg); err != nil {
		t.Fatal(err)
	}
	var want *SourceDelegate
	for _, d := range loadDelegates(t, oraclePath) {
		if d.Name == "dog" && d.TenantNamespace == "/tenants/beta" {
			want = &d
		}
	}
	if want == nil {
		t.Fatal("guard: generate writes no dog delegate for /tenants/beta")
	}
	if added := got[len(before)]; !reflect.DeepEqual(added, *want) {
		t.Errorf("approval added %+v, want generate's delegate %+v", added, *want)
	}
}

// syncTestDir is approveTestDir with acme's provider rules already in
// namespace.rego and a second tenant, beta, approved with an email_domain
// matcher on Google — the state an existing cluster is in before one of its
// tenants gains a claim matcher.
func syncTestDir(t *testing.T) string {
	t.Helper()
	dir := approveTestDir(t, true)
	if err := SyncTenantRules(dir, "acme"); err != nil {
		t.Fatalf("SyncTenantRules: %v", err)
	}
	writeTestFile(t, TenantRequestPath(dir, "beta"),
		"providers: []\nclaim_matchers:\n  - provider_issuer_url: https://accounts.google.com\n    claim_key: email_domain\n    claim_value: beta.example\n")
	if err := ApproveTenant(dir, "beta"); err != nil {
		t.Fatalf("ApproveTenant: %v", err)
	}
	return dir
}

// acmeOAuth is acme's oauth.yaml from approveTestDir plus claimMatchers.
func acmeOAuth(claimMatchers string) string {
	return "providers:\n  - enable: true\n    issuer_url: https://idp.acme.example\n    client_id: acme-web\n    client_secret: s\n" + claimMatchers
}

// TestSyncTenantRulesAppendsNewClaimMatcher: an existing tenant's new
// email_domain matcher is appended as exactly its claim rule; everything the
// file held — hand-written rules and the tenant's own provider rules — stays
// byte-for-byte, and a second run changes nothing.
func TestSyncTenantRulesAppendsNewClaimMatcher(t *testing.T) {
	dir := syncTestDir(t)
	regoPath := filepath.Join(OPAPolicyDir(dir), "tenants", "namespace.rego")
	regoBefore := fileText(t, regoPath)
	if !strings.Contains(regoBefore, handRule) {
		t.Fatalf("fixture lost the hand-written rule:\n%s", regoBefore)
	}
	writeTestFile(t, filepath.Join(TenantConfigDir(dir, "acme"), "oauth.yaml"),
		acmeOAuth("claim_matchers:\n  - provider_issuer_url: https://accounts.google.com\n    claim_key: email_domain\n    claim_value: acme.example\n"))

	if err := SyncTenantRules(dir, "acme"); err != nil {
		t.Fatalf("SyncTenantRules: %v", err)
	}
	want := regoBefore + claimNamespaceRule(TenantIdp{
		IssuerURL: "https://accounts.google.com", ClaimKey: "email_domain", ClaimValue: "acme.example",
		TenantNamespace: "/tenants/acme",
	})
	regoAfter := fileText(t, regoPath)
	if regoAfter != want {
		t.Fatalf("namespace.rego =\n%s\n\nwant\n%s", regoAfter, want)
	}

	if err := SyncTenantRules(dir, "acme"); err != nil {
		t.Fatalf("second SyncTenantRules: %v", err)
	}
	if got := fileText(t, regoPath); got != regoAfter {
		t.Errorf("second run changed namespace.rego:\n%s", got)
	}
}

// TestSyncTenantRulesReplacesAChangedProviderIssuer: a matcher moved from the
// Microsoft organizations endpoint to common leaves only the common rule for
// acme; acme's provider rules, the hand-written rule and beta's rule stay
// byte-for-byte, and a second run changes nothing.
func TestSyncTenantRulesReplacesAChangedProviderIssuer(t *testing.T) {
	dir := syncTestDir(t)
	regoPath := filepath.Join(OPAPolicyDir(dir), "tenants", "namespace.rego")
	regoBefore := fileText(t, regoPath)
	matcher := func(issuer string) string {
		return "claim_matchers:\n  - provider_issuer_url: " + issuer + "\n    claim_key: email_domain\n    claim_value: acme.example\n"
	}
	rule := func(issuer string) string {
		return claimNamespaceRule(TenantIdp{
			IssuerURL: issuer, ClaimKey: "email_domain", ClaimValue: "acme.example", TenantNamespace: "/tenants/acme",
		})
	}
	const organizations = "https://login.microsoftonline.com/organizations/v2.0"
	const common = "https://login.microsoftonline.com/common/v2.0"
	oauthPath := filepath.Join(TenantConfigDir(dir, "acme"), "oauth.yaml")

	writeTestFile(t, oauthPath, acmeOAuth(matcher(organizations)))
	if err := SyncTenantRules(dir, "acme"); err != nil {
		t.Fatalf("SyncTenantRules (organizations): %v", err)
	}
	if got, want := fileText(t, regoPath), regoBefore+rule(organizations); got != want {
		t.Fatalf("namespace.rego =\n%s\n\nwant\n%s", got, want)
	}

	writeTestFile(t, oauthPath, acmeOAuth(matcher(common)))
	if err := SyncTenantRules(dir, "acme"); err != nil {
		t.Fatalf("SyncTenantRules (common): %v", err)
	}
	regoAfter := fileText(t, regoPath)
	if want := regoBefore + rule(common); regoAfter != want {
		t.Fatalf("namespace.rego =\n%s\n\nwant\n%s", regoAfter, want)
	}
	if strings.Contains(regoAfter, organizations) {
		t.Errorf("the organizations rule lingers:\n%s", regoAfter)
	}

	if err := SyncTenantRules(dir, "acme"); err != nil {
		t.Fatalf("second SyncTenantRules: %v", err)
	}
	if got := fileText(t, regoPath); got != regoAfter {
		t.Errorf("second run changed namespace.rego:\n%s", got)
	}
}

// TestSyncTenantRulesRefusesAndChangesNothing: every refusal leaves
// namespace.rego as it was.
func TestSyncTenantRulesRefusesAndChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, tenant, claimMatchers, wantErr string
	}{
		{"public mail domain", "acme",
			"claim_matchers:\n  - provider_issuer_url: https://accounts.google.com\n    claim_key: email_domain\n    claim_value: gmail.com\n",
			"public mail domain"},
		{"domain routed to another tenant", "acme",
			"claim_matchers:\n  - provider_issuer_url: https://accounts.google.com\n    claim_key: email_domain\n    claim_value: beta.example\n",
			"claimed by both"},
		{"hd of another tenant's email_domain", "acme",
			"claim_matchers:\n  - provider_issuer_url: https://accounts.google.com\n    claim_key: hd\n    claim_value: beta.example\n",
			"claimed by both"},
		{"unknown tenant", "nobody", "", "not a tenant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := syncTestDir(t)
			writeTestFile(t, filepath.Join(TenantConfigDir(dir, "acme"), "oauth.yaml"), acmeOAuth(tc.claimMatchers))
			regoPath := filepath.Join(OPAPolicyDir(dir), "tenants", "namespace.rego")
			regoBefore := fileText(t, regoPath)

			err := SyncTenantRules(dir, tc.tenant)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("SyncTenantRules error = %v, want one containing %q", err, tc.wantErr)
			}
			if got := fileText(t, regoPath); got != regoBefore {
				t.Errorf("namespace.rego changed:\n%s", got)
			}
		})
	}
}

// wantUserRoleRule is the role rule an email_domain matcher on issuer for domain
// renders for ns, spelled out so a change to its shape is a test change.
func wantUserRoleRule(ns, issuer, domain string) string {
	return "\n# Generated: a verified " + domain + " address routed into " + ns + " is a user.\n" +
		"roles := {\"user\"} if {\n\tinput.claims.provider_iss == \"" + issuer + "\"\n\tinput.claims[\"email_domain\"] == \"" + domain + "\"\n}\n"
}

// TestApproveTenantGrantsUserForEmailDomain: approving a tenant with an
// email_domain matcher appends its user role rule to grants/static.rego and
// leaves the hand-written grant alone; an hd matcher grants no role.
func TestApproveTenantGrantsUserForEmailDomain(t *testing.T) {
	dir := approveTestDir(t, true)
	writeTestFile(t, TenantRequestPath(dir, "beta"),
		"providers: []\nclaim_matchers:\n"+
			"  - provider_issuer_url: https://accounts.google.com\n    claim_key: email_domain\n    claim_value: beta.example\n"+
			"  - provider_issuer_url: https://accounts.google.com\n    claim_key: hd\n    claim_value: beta-hd.example\n")

	if err := ApproveTenant(dir, "beta"); err != nil {
		t.Fatalf("ApproveTenant: %v", err)
	}

	got := fileText(t, filepath.Join(OPAPolicyDir(dir), "grants", "static.rego"))
	want := defaultGrantsRego + handRoleRule + wantUserRoleRule("/tenants/beta", "https://accounts.google.com", "beta.example")
	if got != want {
		t.Errorf("static.rego =\n%s\n\nwant\n%s", got, want)
	}
}

// TestSyncTenantRulesGrantsUserForEmailDomain: an existing tenant's new
// email_domain matcher appends its user role rule; beta's and the hand-written
// rule stay byte-for-byte, and a second run changes nothing.
func TestSyncTenantRulesGrantsUserForEmailDomain(t *testing.T) {
	dir := syncTestDir(t)
	staticPath := filepath.Join(OPAPolicyDir(dir), "grants", "static.rego")
	before := fileText(t, staticPath)
	if want := defaultGrantsRego + handRoleRule + wantUserRoleRule("/tenants/beta", "https://accounts.google.com", "beta.example"); before != want {
		t.Fatalf("fixture static.rego =\n%s\n\nwant\n%s", before, want)
	}
	writeTestFile(t, filepath.Join(TenantConfigDir(dir, "acme"), "oauth.yaml"),
		acmeOAuth("claim_matchers:\n  - provider_issuer_url: https://accounts.google.com\n    claim_key: email_domain\n    claim_value: acme.example\n"))

	if err := SyncTenantRules(dir, "acme"); err != nil {
		t.Fatalf("SyncTenantRules: %v", err)
	}
	after := fileText(t, staticPath)
	if want := before + wantUserRoleRule("/tenants/acme", "https://accounts.google.com", "acme.example"); after != want {
		t.Fatalf("static.rego =\n%s\n\nwant\n%s", after, want)
	}

	if err := SyncTenantRules(dir, "acme"); err != nil {
		t.Fatalf("second SyncTenantRules: %v", err)
	}
	if got := fileText(t, staticPath); got != after {
		t.Errorf("second run changed static.rego:\n%s", got)
	}
}

// TestSyncTenantRulesRemovesStaleUserRoleRules: a matcher moved from the
// Microsoft organizations endpoint to common leaves only the common role rule
// for acme, and dropping the matcher drops acme's role rule; beta's and the
// hand-written rule stay byte-for-byte throughout.
func TestSyncTenantRulesRemovesStaleUserRoleRules(t *testing.T) {
	dir := syncTestDir(t)
	staticPath := filepath.Join(OPAPolicyDir(dir), "grants", "static.rego")
	before := fileText(t, staticPath)
	const organizations = "https://login.microsoftonline.com/organizations/v2.0"
	const common = "https://login.microsoftonline.com/common/v2.0"
	oauthPath := filepath.Join(TenantConfigDir(dir, "acme"), "oauth.yaml")
	matcher := func(issuer string) string {
		return "claim_matchers:\n  - provider_issuer_url: " + issuer + "\n    claim_key: email_domain\n    claim_value: acme.example\n"
	}

	for _, step := range []struct{ oauth, want string }{
		{acmeOAuth(matcher(organizations)), before + wantUserRoleRule("/tenants/acme", organizations, "acme.example")},
		{acmeOAuth(matcher(common)), before + wantUserRoleRule("/tenants/acme", common, "acme.example")},
		{acmeOAuth(""), before},
	} {
		writeTestFile(t, oauthPath, step.oauth)
		if err := SyncTenantRules(dir, "acme"); err != nil {
			t.Fatalf("SyncTenantRules: %v", err)
		}
		if got := fileText(t, staticPath); got != step.want {
			t.Fatalf("static.rego =\n%s\n\nwant\n%s", got, step.want)
		}
	}
}
