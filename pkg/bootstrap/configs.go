package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// 0600, like every service config below: a kernel bootstrap config carries
	// the node's TLS private key and its signing CA key.
	for i, bsYAML := range dc.BootstrapYAMLs {
		name := dc.BootstrapNames[i]
		if err := os.WriteFile(filepath.Join(configsDir, name+".yaml"), bsYAML, 0600); err != nil {
			return fmt.Errorf("writing %s.yaml: %w", name, err)
		}
	}

	t := dc.Cfg.Topology
	serviceFiles := map[string][]byte{
		"sentinel": dc.SentinelConfig,
		"gateway":  dc.GatewayConfig,
		"eventbus": dc.EventbusConfig,
	}
	if len(dc.OpaConfig) > 0 && t.OpaNodes > 0 {
		serviceFiles["opa"] = dc.OpaConfig
	}
	if len(dc.EchoConfig) > 0 && t.EchoNodes > 0 && dc.Cfg.GetOptionalServices().GetEcho() {
		serviceFiles["echo"] = dc.EchoConfig
	}
	if len(dc.MessagingConfig) > 0 && t.MessagingNodes > 0 && dc.Cfg.GetOptionalServices().GetMessaging() {
		serviceFiles["messaging"] = dc.MessagingConfig
	}
	if len(dc.MailboxConfig) > 0 && t.MailboxNodes > 0 && dc.Cfg.GetOptionalServices().GetMailbox() {
		serviceFiles["mailbox"] = dc.MailboxConfig
	}
	if len(dc.PersonalauthConfig) > 0 && t.PersonalauthNodes > 0 && dc.Cfg.GetOptionalServices().GetPersonalauth() {
		serviceFiles["personalauth"] = dc.PersonalauthConfig
	}
	if len(dc.StratumConfig) > 0 && t.StratumNodes > 0 {
		serviceFiles["stratum"] = dc.StratumConfig
	}
	if len(dc.KmakerConfig) > 0 && t.KmakerNodes > 0 {
		serviceFiles["kmaker"] = dc.KmakerConfig
	}

	opt := dc.Cfg.GetOptionalServices()
	if len(dc.OauthConfig) > 0 && opt.GetOauth() {
		serviceFiles["oauth"] = dc.OauthConfig
	}
	if len(dc.SamlConfig) > 0 && opt.GetSaml() {
		serviceFiles["saml"] = dc.SamlConfig
	}
	// The application's own services. The in-memory bundle path (BundleFromConfigs)
	// has always included these; this on-disk path did not, so `build` produced a
	// portable bundle with NO config for any app service — and `apply --bundle`
	// then started them with an empty config, which is a startup crash
	// ("data is null or empty"), not a degraded mode. It went unnoticed because
	// the one-shot `apply` builds its bundle in memory and was the only path
	// exercised after app services were introduced.
	for name, data := range dc.AppConfigs {
		if len(data) > 0 {
			serviceFiles[name] = data
		}
	}

	for svc, data := range serviceFiles {
		if err := os.WriteFile(filepath.Join(configsDir, svc+".yaml"), data, 0600); err != nil {
			return fmt.Errorf("writing %s.yaml: %w", svc, err)
		}
	}
	if len(dc.LetsencryptServiceConfig) > 0 {
		if err := os.WriteFile(filepath.Join(configsDir, "letsencrypt-service.yaml"), dc.LetsencryptServiceConfig, 0600); err != nil {
			return fmt.Errorf("writing letsencrypt-service.yaml: %w", err)
		}
	}

	// Retire configs for services the topology no longer deploys.
	//
	// This function only ever wrote, and PackBundle re-ingests every *.yaml it
	// finds here, so a service removed from the topology kept being shipped
	// forever with the config it had on the day it was removed. anthropic-proxy
	// was retired with the rest of proto-app and its config was still being
	// packed into every bundle weeks later.
	//
	// Only *.yaml is considered, so hand-kept files with other suffixes (e.g. a
	// .bak) survive; ca.pem and the bootstrapN configs are written above and are
	// therefore in the keep set.
	keep := map[string]bool{"ca.pem": true}
	for name := range serviceFiles {
		keep[name+".yaml"] = true
	}
	for _, name := range dc.BootstrapNames {
		keep[name+".yaml"] = true
	}
	if len(dc.LetsencryptServiceConfig) > 0 {
		keep["letsencrypt-service.yaml"] = true
	}
	entries, err := os.ReadDir(configsDir)
	if err != nil {
		return fmt.Errorf("reading configs directory: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".yaml") || keep[name] {
			continue
		}
		if err := os.Remove(filepath.Join(configsDir, name)); err != nil {
			return fmt.Errorf("removing stale config %s: %w", name, err)
		}
		fmt.Printf("removed stale service config: configs/%s\n", name)
	}

	return nil
}
