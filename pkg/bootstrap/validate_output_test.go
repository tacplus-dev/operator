package bootstrap

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// generatedOutputFixture writes a config dir with every service in the topology
// enabled, so the generated-output gate covers the whole emitted surface rather
// than the platform minimum.
func generatedOutputFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "console.seabeam.cloud",
		SpiffePrefix: "local.dev",
		OptionalServices: &protoOperator.OptionalServices{
			Oauth: true,
			Echo:  true,
		},
		Topology: &protoOperator.Topology{
			KernelNodes:   3,
			EventbusNodes: 1,
			GatewayNodes:  1,
			OauthNodes:    1,
			OpaNodes:      1,
			StratumNodes:  1,
			KmakerNodes:   1,
			EchoNodes:     1,
			AppServices:   map[string]int32{"tasks": 1},
		},
	}
	if err := GenerateCrypto(cfg, 3, 0, cfg.Fqdn, cfg.SpiffePrefix); err != nil {
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
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestGeneratedOutputSatisfiesConsumerProtos is the generated-output gate: every
// artifact BuildDeployConfigs emits must satisfy the spec.assert_* constraints
// declared on the proto its consumer parses it as. Nothing checked this before,
// so a field becoming assert_required upstream showed up only as a
// crash-looping pod.
func TestGeneratedOutputSatisfiesConsumerProtos(t *testing.T) {
	dir := generatedOutputFixture(t)

	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatalf("generating deploy configs: %v", err)
	}

	artifacts := deployArtifacts(dc)
	if len(artifacts) == 0 {
		t.Fatal("no generated artifacts to validate — the gate would pass vacuously")
	}
	for _, a := range artifacts {
		if err := a.validate(a.data); err != nil {
			t.Errorf("generated %s does not satisfy %s: %v", a.name, a.protoName, err)
		}
	}

	// The gate must cover every kernel replica and every enabled service, not
	// just whichever ones happen to be non-nil.
	want := []string{
		"bootstrap0", "bootstrap1", "bootstrap2", "kmaker", "echo",
		"gateway", "eventbus", "oauth", "opa", "stratum",
		"tasks",
	}
	got := map[string]bool{}
	for _, a := range artifacts {
		got[a.name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("generated-output gate does not cover %q", name)
		}
	}
}

// TestGeneratedKernelConfigCarriesEveryRequiredField is the reproduction for
// the 2026-08-20 kernel crash-loop: kernel's Config declares network, logger
// and rate_limit assert_required, BuildKernelConfigProto never set them, and
// ConfigDefaults does not recurse into submessages — so every emitted kernel
// bootstrap config was missing all three and kernel died at boot with
// "field network is required". This fails against the generator as it stood
// before f939f7d and passes after it.
func TestGeneratedKernelConfigCarriesEveryRequiredField(t *testing.T) {
	dir := generatedOutputFixture(t)

	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatalf("generating deploy configs: %v", err)
	}
	if len(dc.BootstrapYAMLs) != 3 {
		t.Fatalf("BootstrapYAMLs = %d, want 3", len(dc.BootstrapYAMLs))
	}

	for i, y := range dc.BootstrapYAMLs {
		cfg, err := protoKernel.ConfigFromBytes(y)
		if err != nil {
			t.Fatalf("parsing generated kernel config %d: %v", i, err)
		}
		protoKernel.ConfigDefaults(cfg)
		if err := protoKernel.ValidateConfig(cfg); err != nil {
			t.Fatalf("generated kernel config %d is rejected by kernel's own proto contract: %v", i, err)
		}
		// The three fields the incident turned on. An empty submessage clears
		// the presence check and then fails the nested duration/enum asserts,
		// so assert on the materialised values, not just non-nil.
		if cfg.GetNetwork().GetConnectTimeoutSeconds() <= 0 {
			t.Errorf("kernel config %d: network.connect_timeout_seconds = %d, want > 0",
				i, cfg.GetNetwork().GetConnectTimeoutSeconds())
		}
		if cfg.GetRateLimit() == nil {
			t.Errorf("kernel config %d: rate_limit is nil", i)
		}
		if cfg.GetLogger() == nil {
			t.Errorf("kernel config %d: logger is nil", i)
		}
		if !strings.Contains(string(y), "network:") {
			t.Errorf("kernel config %d: emitted YAML has no network: key", i)
		}
	}
}

// TestValidateDeployConfigsNamesTheMissingField proves the gate's failure mode:
// a required field missing from generated output must produce a non-nil error
// naming the artifact, the proto, and the field — not a silent pass.
func TestValidateDeployConfigsNamesTheMissingField(t *testing.T) {
	dir := generatedOutputFixture(t)

	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatalf("generating deploy configs: %v", err)
	}

	// Re-emit kernel replica 0 with network dropped — the exact shape the
	// generator produced before f939f7d.
	cfg, err := protoKernel.ConfigFromBytes(dc.BootstrapYAMLs[0])
	if err != nil {
		t.Fatal(err)
	}
	cfg.Network = nil
	y, err := protoKernel.ConfigToYAML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dc.BootstrapYAMLs[0] = y

	err = ValidateDeployConfigs(dc)
	if err == nil {
		t.Fatal("ValidateDeployConfigs accepted a kernel config with no network — the gate is a no-op")
	}
	msg := err.Error()
	for _, want := range []string{"bootstrap0", "kernel.Config", "Network is required"} {
		if !strings.Contains(msg, want) {
			t.Errorf("gate error does not mention %q:\n%s", want, msg)
		}
	}
}

// TestEveryDeployArtifactIsClassified fails when a new artifact is added to
// DeployConfigs without either a validator or an explicit exclusion. Without
// this the gate degrades silently: the new artifact just never gets checked,
// which is the same "nobody notices the coverage shrank" shape as the incidents
// it exists to catch.
func TestEveryDeployArtifactIsClassified(t *testing.T) {
	// Artifacts that are not a serialized proto message, so there is no
	// generated validator to run against them.
	notAProtoMessage := map[string]string{
		"CACertPEM":                "PEM bytes, not a proto message",
		"LetsencryptServiceConfig": "ACME options map, no proto in the operator's model",
	}
	// Fields that carry the validated artifacts rather than being one.
	notAnArtifact := map[string]bool{
		"BootstrapNames": true,
		"GatewayOpts":    true,
		"Cfg":            true,
	}

	validated := map[string]bool{
		"BootstrapYAMLs": true, "KmakerConfig": true,
		"EchoConfig": true, "MessagingConfig": true, "MailboxConfig": true,
		"PersonalauthConfig": true,
		"AppConfigs":         true, "GatewayConfig": true,
		"EventbusConfig": true, "OauthConfig": true, "OpaConfig": true,
		"StratumConfig":           true,
		"StratumSearchConfig":     true,
		"LetsencryptConfig":       true,
		"BlobstoreConfig":         true,
		"OperatorBootstrapConfig": true,
	}

	typ := reflect.TypeOf(DeployConfigs{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		switch {
		case validated[name], notAnArtifact[name]:
		case notAProtoMessage[name] != "":
		default:
			t.Errorf("DeployConfigs.%s is not covered by the generated-output gate — "+
				"add a validator in deployArtifacts, or record why it is not a proto message",
				name)
		}
	}

	// The other direction: a field that was removed must not linger in the
	// lists above pretending to be covered.
	present := map[string]bool{}
	for i := range typ.NumField() {
		present[typ.Field(i).Name] = true
	}
	for _, list := range []map[string]bool{validated, notAnArtifact} {
		for name := range list {
			if !present[name] {
				t.Errorf("gate references DeployConfigs.%s, which no longer exists", name)
			}
		}
	}
	for name := range notAProtoMessage {
		if !present[name] {
			t.Errorf("gate references DeployConfigs.%s, which no longer exists", name)
		}
	}
}
