package bootstrap

import (
	"os"
	"path/filepath"
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
	if !proto.Equal(first, second) {
		t.Fatalf("loading the same OPA policy twice produced different protos:\nfirst=%v\nsecond=%v", first, second)
	}
}
