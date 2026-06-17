package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
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

func TestDefaultOPAPolicyUsesGeneratedGrantNames(t *testing.T) {
	if !strings.Contains(defaultMainRego, `"ANTHROPIC_ANY"`) {
		t.Fatalf("defaultMainRego missing ANTHROPIC_ANY")
	}
	if strings.Contains(defaultMainRego, "_RPCS_ANY") {
		t.Fatalf("defaultMainRego contains legacy *_RPCS_ANY grants")
	}
}
