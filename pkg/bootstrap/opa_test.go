package bootstrap

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/grants"
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
	bundle := DefaultOPABundle([]string{"https://idp.example.com"})
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
