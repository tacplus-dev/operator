package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// llmCredentialDeployDir writes a minimal operator config dir whose crypto
// carries an llm_credential routed to the given services.
func llmCredentialDeployDir(t *testing.T, services []string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "schematic.localtest.me",
		SpiffePrefix: "local.dev",
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			SentinelNodes: 1,
			GatewayNodes:  1,
			EventbusNodes: 1,
			AppServices:   map[string]int32{"llmproxy": 1},
		},
		LlmCredential:         "sk-ant-test-credential",
		LlmCredentialServices: services,
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
	return dir
}

// The generated chassis takes a service's typed Config from kernel's
// ConfigState, which kernel seeds from Config.configs on load. A credential
// that only reaches the service's own bootstrap YAML is therefore never seen
// by the service (llmproxy ran with an empty key and Anthropic answered 401
// "x-api-key header is required"). The holder entry must carry it.
func TestKernelBootstrapSeedsLlmCredentialIntoServicesHolder(t *testing.T) {
	RegisterAppServices([]string{"llmproxy"})
	t.Cleanup(func() { RegisterAppServices(nil) })
	dir := llmCredentialDeployDir(t, []string{"llmproxy"})

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
	got := kernelCfg.GetConfigs().GetLlmproxyConfig().GetLlmCredential()
	if got != "sk-ant-test-credential" {
		t.Fatalf("kernel services holder llmproxy_config.llm_credential = %q, want the routed credential", got)
	}
}

// A service listed in llm_credential_services that the build has no holder
// entry for is a config mistake, and must fail generation loudly rather than
// silently route the credential nowhere.
func TestKernelBootstrapRefusesLlmCredentialForUnknownService(t *testing.T) {
	RegisterAppServices([]string{"llmproxy"})
	t.Cleanup(func() { RegisterAppServices(nil) })
	dir := llmCredentialDeployDir(t, []string{"nosuchservice"})

	_, err := BuildDeployConfigs(dir)
	if err == nil || !strings.Contains(err.Error(), "nosuchservice") {
		t.Fatalf("BuildDeployConfigs error = %v, want a refusal naming nosuchservice", err)
	}
}
