package bootstrap

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The embedded registry.json is a copy of model/registry.json (see
// registry_embed.go). model/ is what this package compiles against, so it is
// always present when these tests run; if the copy has drifted from it, the
// binary built from this tree would compute delegate authorizations from a
// registry that no longer matches the fleet it was generated for.
func TestEmbeddedRegistryMatchesGeneratedModel(t *testing.T) {
	path := filepath.Join("..", "..", "model", "registry.json")
	generated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v (model/ must be generated for this package to build at all)", path, err)
	}
	if !bytes.Equal(generated, registryJSON) {
		t.Fatalf("pkg/bootstrap/registry.json (%d bytes) differs from %s (%d bytes): run `go generate ./pkg/bootstrap` (or `just generate`) so the embedded registry matches the generated model",
			len(registryJSON), path, len(generated))
	}
}
