package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildConfigOutputWritesKernelBootstrapPrivate: the kernel bootstrap
// configs carry the node's TLS private key and its signing CA key. They were
// written 0644 while every sibling service config on the same path was written
// 0600, so the key material was world-readable on the operator host.
func TestBuildConfigOutputWritesKernelBootstrapPrivate(t *testing.T) {
	dir := generatedOutputFixture(t)

	if err := BuildConfigOutput(dir); err != nil {
		t.Fatalf("building config output: %v", err)
	}

	for _, name := range []string{"bootstrap0.yaml", "bootstrap1.yaml", "bootstrap2.yaml"} {
		path := filepath.Join(dir, "configs", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		// Guard the premise: this test is only meaningful while these files
		// actually contain key material.
		// The gateway external TLS key is inlined as literal PEM; tls.key and
		// signing_ca.key ride along in the same file.
		for _, marker := range []string{"-----BEGIN EC PRIVATE KEY-----", "external_key_pem", "signing_ca"} {
			if !strings.Contains(string(data), marker) {
				t.Fatalf("%s does not contain %q — the permission guard below is testing the wrong file", name, marker)
			}
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("configs/%s mode = %#o, want 0600 (it holds kernel's TLS and signing keys)", name, got)
		}
	}
}
