package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// TestBuildKernelConfigSeedsAppServiceConfigFromFile: an app service's
// services/<name>.yaml is that service's typed Config and must reach kernel's
// services holder, or the service only ever sees defaults (llmproxy's D22
// federation ids, for one, live nowhere else).
func TestBuildKernelConfigSeedsAppServiceConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace: "workloads",
		Topology:  &protoOperator.Topology{AppServices: map[string]int32{"llmproxy": 1}},
	}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("delegates: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	llmproxyYAML := []byte(`anthropic_federation_rule_id: fdrl_test
anthropic_organization_id: org_test
anthropic_service_account_id: svac_test
anthropic_workspace_id: wrkspc_test
`)
	if err := os.WriteFile(filepath.Join(servicesDir, "llmproxy.yaml"), llmproxyYAML, 0644); err != nil {
		t.Fatal(err)
	}

	kcfg, err := BuildKernelConfigProto(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := kcfg.GetConfigs().GetLlmproxyConfig()
	if got == nil {
		t.Fatalf("services/llmproxy.yaml did not reach the holder: Configs = %#v", kcfg.GetConfigs())
	}
	if got.GetAnthropicFederationRuleId() != "fdrl_test" || got.GetAnthropicOrganizationId() != "org_test" ||
		got.GetAnthropicServiceAccountId() != "svac_test" || got.GetAnthropicWorkspaceId() != "wrkspc_test" {
		t.Fatalf("llmproxy config = %+v, want the four ids from services/llmproxy.yaml", got)
	}
}

// An app service with no services/<name>.yaml is left to the holder
// defaulters; the file is optional.
func TestBuildKernelConfigAppServiceFileIsOptional(t *testing.T) {
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace: "workloads",
		Topology:  &protoOperator.Topology{AppServices: map[string]int32{"llmproxy": 1}},
	}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("delegates: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildKernelConfigProto(dir); err != nil {
		t.Fatal(err)
	}
}
