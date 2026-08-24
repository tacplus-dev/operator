package bootstrap

import (
	"testing"

	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/grants"
)

// allServiceGrants used to reflect over TokenEntitlements looking for
// repeated-ENUM fields, one per service. The message carries
// `repeated string grants = 1`, so the loop matched nothing and returned an
// empty map -- `add-profile --all` wrote a profile with zero grants and printed
// success. An empty result is the bug, so assert it is not empty first.
func TestAllServiceGrantsIsNotEmpty(t *testing.T) {
	got := allServiceGrants()
	tokens, ok := got[entitlementGrantsField]
	if !ok {
		t.Fatalf("allServiceGrants() has no %q key: %v", entitlementGrantsField, got)
	}
	if len(tokens) == 0 {
		t.Fatalf("allServiceGrants() granted nothing -- --all would write an empty profile")
	}
	for _, token := range tokens {
		g, ok := grants.Lookup(token)
		if !ok {
			t.Errorf("token %q is not in the generated grant catalog", token)
			continue
		}
		if g.Capability != "ANY" {
			t.Errorf("token %q is not a package wildcard (capability %q)", token, g.Capability)
		}
		if len(grants.Expand(token)) == 0 {
			t.Errorf("token %q expands to no RPCs -- it grants nothing", token)
		}
	}
}
