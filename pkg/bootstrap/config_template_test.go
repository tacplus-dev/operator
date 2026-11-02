package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `init config <file>` applies a template file, as `export config` writes
// one, to the config dir: operator settings fill missing keys, rego files are
// written. Nothing is fetched.
func TestInitConfigAppliesALocalTemplateFile(t *testing.T) {
	dir := t.TempDir()
	prev := configDir
	configDir = dir
	defer func() { configDir = prev }()

	if err := os.WriteFile(filepath.Join(dir, "operator.yaml"), []byte("fqdn: cluster.example.com\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tmpl := filepath.Join(t.TempDir(), "dev1.yaml")
	if err := os.WriteFile(tmpl, []byte("operator:\n  fqdn: other.example.com\n  dev: true\nopa:\n  main.rego: package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := runInitConfig(initConfigCmd, []string{tmpl}); err != nil {
		t.Fatalf("init config: %v", err)
	}

	op, err := os.ReadFile(filepath.Join(dir, "operator.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(op), "fqdn: cluster.example.com") || !strings.Contains(string(op), "dev: true") {
		t.Errorf("operator.yaml = %q, want the existing fqdn kept and dev filled in", op)
	}
	rego, err := os.ReadFile(filepath.Join(dir, "services", "opa", "main.rego"))
	if err != nil || string(rego) != "package main" {
		t.Errorf("services/opa/main.rego = %q, %v", rego, err)
	}
}

func TestInitConfigRefusesAMissingTemplateFile(t *testing.T) {
	prev := configDir
	configDir = t.TempDir()
	defer func() { configDir = prev }()

	missing := filepath.Join(t.TempDir(), "absent.yaml")
	err := runInitConfig(initConfigCmd, []string{missing})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("init config of a missing file = %v, want an error naming it", err)
	}
}
