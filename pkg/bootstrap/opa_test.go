package bootstrap

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"gitlab.com/seabeam/schematic/platform/services/operator/model/grants"
	protoFilesystem "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/filesystem"
)

func TestLoadOPAPolicyIsStable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.rego"), []byte("package entitlements\n"), 0644); err != nil {
		t.Fatal(err)
	}

	first, err := LoadOPAPolicy(dir, "data.entitlements.eval")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOPAPolicy(dir, "data.entitlements.eval")
	if err != nil {
		t.Fatal(err)
	}

	if first.GetCreated() != stablePolicyCreated {
		t.Fatalf("created = %d, want %d", first.GetCreated(), stablePolicyCreated)
	}
	if first.GetCreated() < 1735689600 {
		t.Fatalf("created = %d, want at least 1735689600", first.GetCreated())
	}
	if !proto.Equal(first, second) {
		t.Fatalf("loading the same OPA policy twice produced different protos:\nfirst=%v\nsecond=%v", first, second)
	}
}

func TestLoadOptionalOPAPolicy(t *testing.T) {
	missing, err := LoadOptionalOPAPolicy(filepath.Join(t.TempDir(), "missing"), "data.proxypolicy.eval")
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatalf("missing optional policy = %v, want nil", missing)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.rego"), []byte("package proxypolicy\n"), 0644); err != nil {
		t.Fatal(err)
	}
	policy, err := LoadOptionalOPAPolicy(dir, "data.proxypolicy.eval")
	if err != nil {
		t.Fatal(err)
	}
	if policy == nil {
		t.Fatal("policy = nil, want loaded policy")
	}
	if got, want := policy.GetEntryPoint(), "data.proxypolicy.eval"; got != want {
		t.Fatalf("entry_point = %q, want %q", got, want)
	}
}

// This test used to require ANTHROPIC_ANY in defaultMainRego. anthropic left the
// platform, the generator's own comment says so, and the rego stopped naming it
// -- so the assertion had been failing, pinning a symbol the catalog does not
// know and that expands to nothing. Assert what the rego must actually satisfy:
// every grant it hands out is a real, non-empty package wildcard.
func TestDefaultOPAPolicyUsesGeneratedGrantNames(t *testing.T) {
	if strings.Contains(defaultMainRego, "_RPCS_ANY") {
		t.Fatalf("defaultMainRego contains legacy *_RPCS_ANY grants")
	}
	tokens := regexp.MustCompile(`"([A-Z0-9_]+_ANY)"`).FindAllStringSubmatch(defaultMainRego, -1)
	if len(tokens) == 0 {
		t.Fatal("defaultMainRego grants no wildcards at all")
	}
	for _, m := range tokens {
		token := m[1]
		g, ok := grants.Lookup(token)
		if !ok {
			t.Errorf("defaultMainRego grants %q, which is not in the generated catalog", token)
			continue
		}
		if g.Capability != "ANY" {
			t.Errorf("defaultMainRego grants %q, which is not a package wildcard", token)
		}
		if len(grants.Expand(token)) == 0 {
			t.Errorf("defaultMainRego grants %q, which expands to no RPCs", token)
		}
	}
}

// The generated IDP policy must be fail-CLOSED on a missing groups claim.
// It used to carry a `roles := {"admin"} if { trusted_issuer; not
// input.claims.groups }` rule, so a token from any trusted issuer that simply
// omitted the claim was handed the admin role -- and with it KERNEL_ANY, the
// cluster's administrative surface and its own token-minting authority. Assert
// the general shape rather than the one deleted rule: `default roles := set()`
// is the only answer for a token that asserts nothing, and every rule that
// produces roles must read the groups claim to do it.
func TestDefaultIdpPolicyFailsClosedWithoutGroupsClaim(t *testing.T) {
	bundle := DefaultOPABundle([]string{"https://idp.example.com"}, nil)
	var claims string
	for _, sub := range bundle.GetDirectories() {
		if sub.GetName() != "idp" {
			continue
		}
		for _, f := range sub.GetFiles() {
			if f.GetName() == "claims.rego" {
				claims = string(f.GetContent())
			}
		}
	}
	if claims == "" {
		t.Fatal("generated bundle has no idp/claims.rego")
	}

	if !strings.Contains(claims, "default roles := set()") {
		t.Error("idp/claims.rego does not default roles to the empty set")
	}

	// Split on the rule heads so each `roles := ...` body can be inspected on
	// its own. The head is line-anchored, which is what keeps the `user_roles`
	// comprehension inside a body -- and the indented `default roles` line --
	// from reading as rules of their own. The first chunk is the preamble.
	chunks := strings.Split(claims, "\nroles := ")
	rules := 0
	for _, chunk := range chunks[1:] {
		body, _, ok := strings.Cut(chunk, "\n}")
		if !ok {
			t.Fatalf("unterminated roles rule in idp/claims.rego:\n%s", chunk)
		}
		rules++
		if !strings.Contains(body, "input.claims.groups") {
			t.Errorf("a roles rule grants roles without reading the groups claim:\nroles := %s\n}", body)
		}
		if strings.Contains(body, "not input.claims.groups") {
			t.Errorf("a roles rule grants roles BECAUSE the groups claim is absent:\nroles := %s\n}", body)
		}
	}
	if rules == 0 {
		t.Error("idp/claims.rego has no roles rule at all")
	}
}

// The generated policy must be able to name a tenant for a configured IDP.
// Kernel refuses to mint a user token whose namespace the policy did not
// supply, so without this rule every login in an operator-bootstrapped cluster
// fails; before the fail-closed kernel change, the same silence minted every
// IdP user at the cluster ROOT instead. Both halves have to hold together.
func TestTenantNamespaceRuleForConfiguredIdp(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "operator.yaml"), "tenants:\n  - acme\n  - beta\n")
	writeTestFile(t, filepath.Join(dir, "tenants", "acme", "oauth.yaml"),
		"providers:\n"+
			"  - enable: true\n"+
			"    issuer_url: https://idp.acme.example\n"+
			"    client_id: acme-web\n"+
			"    client_secret: s\n")
	// beta shares nothing with acme, and carries a disabled provider that must
	// not reach the policy.
	writeTestFile(t, filepath.Join(dir, "tenants", "beta", "oauth.yaml"),
		"providers:\n"+
			"  - enable: true\n"+
			"    issuer_url: https://idp.beta.example\n"+
			"    client_id: beta-web\n"+
			"    client_secret: s\n"+
			"  - enable: false\n"+
			"    issuer_url: https://old.beta.example\n"+
			"    client_id: retired\n"+
			"    client_secret: s\n")

	idps, err := LoadTenantIdps(dir)
	if err != nil {
		t.Fatalf("LoadTenantIdps: %v", err)
	}
	want := []TenantIdp{
		{IssuerURL: "https://idp.acme.example", ClientID: "acme-web", TenantNamespace: "/tenants/acme"},
		{IssuerURL: "https://idp.beta.example", ClientID: "beta-web", TenantNamespace: "/tenants/beta"},
	}
	if !reflect.DeepEqual(idps, want) {
		t.Fatalf("LoadTenantIdps = %+v, want %+v", idps, want)
	}

	rego := bundleFile(t, DefaultOPABundle(nil, idps), "tenants", "namespace.rego")

	if !strings.Contains(rego, `default namespace := ""`) {
		t.Error("tenants/namespace.rego does not default the namespace to empty")
	}
	if strings.Contains(rego, `namespace := "/"`) {
		t.Error("tenants/namespace.rego falls back to the cluster root")
	}
	for _, want := range []string{
		"namespace := \"/tenants/acme\" if {\n\tinput.claims.origin_iss == \"https://idp.acme.example\"\n\taud_matches(\"acme-web\")\n}",
		"namespace := \"/tenants/beta\" if {\n\tinput.claims.origin_iss == \"https://idp.beta.example\"\n\taud_matches(\"beta-web\")\n}",
	} {
		if !strings.Contains(rego, want) {
			t.Errorf("tenants/namespace.rego is missing rule:\n%s\n\ngot:\n%s", want, rego)
		}
	}
	if strings.Contains(rego, "retired") {
		t.Error("a disabled provider reached the generated policy")
	}

	// The eval rules must actually read it, or the rule above is dead weight.
	main := bundleFile(t, DefaultOPABundle(nil, idps), "", "main.rego")
	if !strings.Contains(main, "import data.entitlements.tenants") ||
		!strings.Contains(main, `"namespace": tenants.namespace`) {
		t.Errorf("main.rego does not put the tenant namespace in the eval result:\n%s", main)
	}
}

// An issuer shared by two tenants gets exact (issuer, client_id) rules only.
// The audience-less issuer shortcut would otherwise have to pick one of them.
func TestSharedIssuerGetsNoIssuerOnlyRule(t *testing.T) {
	rego := bundleFile(t, DefaultOPABundle(nil, []TenantIdp{
		{IssuerURL: "https://shared.example", ClientID: "a", TenantNamespace: "/tenants/acme"},
		{IssuerURL: "https://shared.example", ClientID: "b", TenantNamespace: "/tenants/beta"},
		{IssuerURL: "https://solo.example", ClientID: "c", TenantNamespace: "/tenants/acme"},
	}), "tenants", "namespace.rego")

	if strings.Contains(rego, "namespace := \"/tenants/acme\" if {\n\tinput.claims.origin_iss == \"https://shared.example\"\n}") ||
		strings.Contains(rego, "namespace := \"/tenants/beta\" if {\n\tinput.claims.origin_iss == \"https://shared.example\"\n}") {
		t.Errorf("an issuer serving two tenants got an issuer-only rule:\n%s", rego)
	}
	if !strings.Contains(rego, "namespace := \"/tenants/acme\" if {\n\tinput.claims.origin_iss == \"https://solo.example\"\n}") {
		t.Errorf("an issuer serving one tenant got no issuer-only rule:\n%s", rego)
	}
}

// Two tenants claiming the same (issuer, client_id) would render a policy OPA
// rejects at eval time ("complete rules must not produce multiple outputs").
func TestDuplicateIdpKeyAcrossTenantsIsRejected(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "operator.yaml"), "tenants:\n  - acme\n  - beta\n")
	body := "providers:\n  - enable: true\n    issuer_url: https://idp.example\n    client_id: shared\n    client_secret: s\n"
	writeTestFile(t, filepath.Join(dir, "tenants", "acme", "oauth.yaml"), body)
	writeTestFile(t, filepath.Join(dir, "tenants", "beta", "oauth.yaml"), body)

	if _, err := LoadTenantIdps(dir); err == nil {
		t.Fatal("two tenants claiming one IDP key was accepted")
	}
}

// generate renders namespace.rego from tenants/*/oauth.yaml when services/opa
// does not exist; a claim-matchers-only tenant must get its (issuer, claim)
// rule there too, and its shared issuer costs a provider tenant on the same
// issuer its issuer-only rule.
func TestTenantNamespaceRuleForClaimMatcher(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "operator.yaml"), "tenants:\n  - acme\n  - beta\n")
	writeTestFile(t, filepath.Join(dir, "tenants", "acme", "oauth.yaml"),
		"providers: []\n"+
			"claim_matchers:\n"+
			"  - provider_issuer_url: https://accounts.google.com\n"+
			"    claim_key: hd\n"+
			"    claim_value: acme.com\n")
	writeTestFile(t, filepath.Join(dir, "tenants", "beta", "oauth.yaml"),
		"providers:\n"+
			"  - enable: true\n"+
			"    issuer_url: https://accounts.google.com\n"+
			"    client_id: beta-web\n"+
			"    client_secret: s\n")

	idps, err := LoadTenantIdps(dir)
	if err != nil {
		t.Fatalf("LoadTenantIdps: %v", err)
	}
	want := []TenantIdp{
		{IssuerURL: "https://accounts.google.com", ClaimKey: "hd", ClaimValue: "acme.com", TenantNamespace: "/tenants/acme"},
		{IssuerURL: "https://accounts.google.com", ClientID: "beta-web", TenantNamespace: "/tenants/beta"},
	}
	if !reflect.DeepEqual(idps, want) {
		t.Fatalf("LoadTenantIdps = %+v, want %+v", idps, want)
	}

	rego := bundleFile(t, DefaultOPABundle(nil, idps), "tenants", "namespace.rego")
	for _, want := range []string{
		"namespace := \"/tenants/acme\" if {\n\tinput.claims.provider_iss == \"https://accounts.google.com\"\n\tinput.claims[\"hd\"] == \"acme.com\"\n}",
		"namespace := \"/tenants/beta\" if {\n\tinput.claims.origin_iss == \"https://accounts.google.com\"\n\taud_matches(\"beta-web\")\n}",
	} {
		if !strings.Contains(rego, want) {
			t.Errorf("tenants/namespace.rego is missing rule:\n%s\n\ngot:\n%s", want, rego)
		}
	}
	if strings.Contains(rego, "\tinput.claims.origin_iss == \"https://accounts.google.com\"\n}") {
		t.Errorf("an issuer shared with a claim matcher got an issuer-only rule:\n%s", rego)
	}
}

// Two tenants claiming the same (issuer, claim, value) would render a policy
// OPA rejects at eval time, as two tenants claiming one provider would.
func TestDuplicateClaimMatcherAcrossTenantsIsRejected(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "operator.yaml"), "tenants:\n  - acme\n  - beta\n")
	body := "claim_matchers:\n  - provider_issuer_url: https://accounts.google.com\n    claim_key: hd\n    claim_value: acme.com\n"
	writeTestFile(t, filepath.Join(dir, "tenants", "acme", "oauth.yaml"), body)
	writeTestFile(t, filepath.Join(dir, "tenants", "beta", "oauth.yaml"), body)

	if _, err := LoadTenantIdps(dir); err == nil {
		t.Fatal("two tenants claiming one claim matcher was accepted")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// bundleFile returns the content of subdir/name in the bundle ("" subdir = root).
func bundleFile(t *testing.T, bundle *protoFilesystem.Directory, subdir, name string) string {
	t.Helper()
	files := bundle.GetFiles()
	if subdir != "" {
		files = nil
		for _, d := range bundle.GetDirectories() {
			if d.GetName() == subdir {
				files = d.GetFiles()
			}
		}
	}
	for _, f := range files {
		if f.GetName() == name {
			return string(f.GetContent())
		}
	}
	t.Fatalf("generated bundle has no %s/%s", subdir, name)
	return ""
}
