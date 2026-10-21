package bootstrap

import (
	"testing"
)

// TestInitClusterOutputPassesApplyValidation runs 'init ca' then
// 'init cluster' into a fresh dir and puts the result through the same
// checks 'apply' runs before rendering, so init never hands the operator a
// config dir that apply rejects.
func TestInitClusterOutputPassesApplyValidation(t *testing.T) {
	dir := t.TempDir()

	prevDir, prevFQDN, prevPort, prevType := configDir, initClusterFQDN, initClusterPort, initClusterType
	t.Cleanup(func() {
		configDir, initClusterFQDN, initClusterPort, initClusterType = prevDir, prevFQDN, prevPort, prevType
	})
	configDir = dir
	initClusterFQDN = "console.example.test"
	initClusterPort = 443
	initClusterType = "small"

	if err := runInitCA(nil, nil); err != nil {
		t.Fatalf("init ca: %v", err)
	}
	if err := runInitCluster(nil, nil); err != nil {
		t.Fatalf("init cluster: %v", err)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatalf("EnsureServiceDelegates: %v", err)
	}
	if _, err := BuildDeployConfigs(dir); err != nil {
		t.Fatalf("BuildDeployConfigs: %v", err)
	}
}
