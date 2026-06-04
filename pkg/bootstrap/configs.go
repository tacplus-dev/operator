package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
)

// BuildConfigOutput writes all generated service configs as individual YAML files
// to <cfgDir>/configs/. These files are consumed by Helm chart generation or
// directly mounted into containers.
func BuildConfigOutput(cfgDir string) error {
	dc, err := BuildDeployConfigs(cfgDir)
	if err != nil {
		return err
	}

	configsDir := filepath.Join(cfgDir, "configs")
	if err := os.MkdirAll(configsDir, 0755); err != nil {
		return fmt.Errorf("creating configs directory: %w", err)
	}

	if err := os.WriteFile(filepath.Join(configsDir, "ca.pem"), dc.CACertPEM, 0644); err != nil {
		return fmt.Errorf("writing ca.pem: %w", err)
	}

	// seed.yaml — the one-time mint bootstrap secrets file. Written next to the
	// configs but kept distinct (it is consumed once by mint then discarded;
	// NEVER persisted to durable config/state). mode 0o600 — it holds private
	// bootstrap keys.
	if len(dc.SeedYAML) > 0 {
		if err := os.WriteFile(filepath.Join(configsDir, "seed.yaml"), dc.SeedYAML, 0600); err != nil {
			return fmt.Errorf("writing seed.yaml: %w", err)
		}
	}

	for i, bsYAML := range dc.BootstrapYAMLs {
		name := dc.BootstrapNames[i]
		if err := os.WriteFile(filepath.Join(configsDir, name+".yaml"), bsYAML, 0644); err != nil {
			return fmt.Errorf("writing %s.yaml: %w", name, err)
		}
	}

	t := dc.Cfg.Topology
	serviceFiles := map[string][]byte{
		"sentinel": dc.SentinelConfig,
		"gateway":  dc.GatewayConfig,
		"eventbus": dc.EventbusConfig,
	}
	if len(dc.MsggatewayConfig) > 0 && t.MsggatewayNodes > 0 {
		serviceFiles["msggateway"] = dc.MsggatewayConfig
	}
	if len(dc.OpaConfig) > 0 && t.OpaNodes > 0 {
		serviceFiles["opa"] = dc.OpaConfig
	}
	if len(dc.StratumConfig) > 0 && t.StratumNodes > 0 {
		serviceFiles["stratum"] = dc.StratumConfig
	}
	if len(dc.ArgusConfig) > 0 && t.ArgusNodes > 0 {
		serviceFiles["argus"] = dc.ArgusConfig
	}
	if len(dc.TimedbConfig) > 0 && t.TimedbNodes > 0 {
		serviceFiles["timedb"] = dc.TimedbConfig
	}
	if len(dc.GraphdbConfig) > 0 && t.GraphdbNodes > 0 {
		serviceFiles["graphdb"] = dc.GraphdbConfig
	}
	if len(dc.ColddbConfig) > 0 && t.ColddbNodes > 0 {
		serviceFiles["colddb"] = dc.ColddbConfig
	}

	opt := dc.Cfg.GetOptionalServices()
	if len(dc.OauthConfig) > 0 && opt.GetOauth() {
		serviceFiles["oauth"] = dc.OauthConfig
	}
	if len(dc.AnthropicConfig) > 0 && opt.GetAnthropic() {
		serviceFiles["anthropic"] = dc.AnthropicConfig
	}
	if len(dc.OpenAIConfig) > 0 && opt.GetOpenai() {
		serviceFiles["openai"] = dc.OpenAIConfig
	}
	if len(dc.SamlConfig) > 0 && opt.GetSaml() {
		serviceFiles["saml"] = dc.SamlConfig
	}
	for svc, data := range serviceFiles {
		if err := os.WriteFile(filepath.Join(configsDir, svc+".yaml"), data, 0600); err != nil {
			return fmt.Errorf("writing %s.yaml: %w", svc, err)
		}
	}

	return nil
}
