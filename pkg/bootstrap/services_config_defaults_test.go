package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	protoServicesConfig "gitlab.com/seabeam/schematic/platform/services/operator/model/types/generated/services_config"
	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// A deployed service's holder entry is made present and gets its generated
// defaults; a service the fleet does not carry is skipped. This replaces the
// hand-written per-service table: the generated ServicesDefaults recurses into
// every present entry for whatever services the fleet is built from.
func TestApplyServicesHolderDefaultsPopulatesDeployedEntries(t *testing.T) {
	s := &protoServicesConfig.Services{}
	if err := ApplyServicesHolderDefaults(s, []string{"gateway", "not-in-this-fleet"}); err != nil {
		t.Fatal(err)
	}
	if s.GatewayConfig == nil {
		t.Fatal("a deployed service's entry must be present so kernel does not seed it all-zero")
	}
	if s.GatewayConfig.HealthPollIntervalSeconds != 5 {
		t.Fatalf("generated defaults must reach the entry: health_poll_interval_seconds = %d, want 5", s.GatewayConfig.HealthPollIntervalSeconds)
	}
	if s.OpaConfig != nil {
		t.Fatal("a service not deployed must stay absent")
	}
}

// mailbox, messaging and weather are absent from the bundle's configs: block,
// so kernel used to re-seed their ConfigState from an all-zero holder entry and
// each died with "field ... must be >= N, got: 0" after 30s. The generated
// bootstrap must carry their proto defaults instead.
func TestKernelBootstrapDefaultsHolderEntriesAbsentFromBundle(t *testing.T) {
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "schematic.localtest.me",
		SpiffePrefix: "local.dev",
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			GatewayNodes:  1,
			EventbusNodes: 1,
		},
	}
	if err := GenerateCrypto(cfg, 1, 0, cfg.Fqdn, cfg.SpiffePrefix); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"kernel.yaml", "gateway.yaml"} {
		if err := os.WriteFile(filepath.Join(servicesDir, f), []byte("{}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dc.BootstrapYAMLs) == 0 {
		t.Fatal("no kernel bootstrap YAMLs generated")
	}
	kernelCfg, err := protoKernel.ConfigFromBytes(dc.BootstrapYAMLs[0])
	if err != nil {
		t.Fatalf("parsing kernel bootstrap YAML: %v", err)
	}
	configs := kernelCfg.GetConfigs()
	if got := configs.GetEventbusConfig().GetDefaultRetentionHours(); got != 168 {
		t.Errorf("eventbus_config.default_retention_hours = %d, want 168 (the proto default; assert_ge 1)", got)
	}
}
