package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	protoServicesConfig "gitlab.com/seabeam/schematic/platform/services/operator/model/types/generated/services_config"
	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// Every message entry of kernel's services holder must have a defaulter. A new
// service added to the holder without one would be seeded an all-zero config by
// kernel and crash-loop at boot on its own assert_ge floors, which is how
// mailbox, messaging and weather went down.
func TestServicesHolderDefaultersCoverEveryEntry(t *testing.T) {
	fields := (&protoServicesConfig.Services{}).ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.Message() == nil {
			continue
		}
		if _, ok := servicesHolderDefaulters[string(fd.Name())]; !ok {
			t.Errorf("kernel's services holder declares %s with no servicesHolderDefaulters entry", fd.Name())
		}
	}
}

// mailbox, messaging and weather are absent from the bundle's configs: block,
// so kernel used to re-seed their ConfigState from an all-zero holder entry and
// each died with "field ... must be >= N, got: 0" after 30s. The generated
// bootstrap must carry their proto defaults instead.
func TestKernelBootstrapDefaultsHolderEntriesAbsentFromBundle(t *testing.T) {
	RegisterAppServices([]string{"weather"})
	t.Cleanup(func() { RegisterAppServices(nil) })

	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "schematic.localtest.me",
		SpiffePrefix: "local.dev",
		Topology: &protoOperator.Topology{
			KernelNodes:    1,
			SentinelNodes:  1,
			GatewayNodes:   1,
			EventbusNodes:  1,
			MailboxNodes:   1,
			MessagingNodes: 1,
			AppServices:    map[string]int32{"weather": 1},
		},
		OptionalServices: &protoOperator.OptionalServices{
			Mailbox:   true,
			Messaging: true,
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
	if got := configs.GetMailboxConfig().GetSweepIntervalSeconds(); got != 300 {
		t.Errorf("mailbox_config.sweep_interval_seconds = %d, want 300 (assert_ge 10)", got)
	}
	if got := configs.GetMessagingConfig().GetMaxRecipients(); got != 64 {
		t.Errorf("messaging_config.max_recipients = %d, want 64 (assert_ge 1)", got)
	}
	if got := configs.GetWeatherConfig().GetObservationPollSeconds(); got != 3600 {
		t.Errorf("weather_config.observation_poll_seconds = %d, want 3600 (assert_ge 300)", got)
	}
}
