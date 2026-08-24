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
