package bootstrap

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	protoGrpcConfig "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/grpcconfig"
	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	protoKmaker "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kmaker"
)

// Every artifact BuildDeployConfigs emits is a serialized proto message that
// the consuming service parses and validates at boot. Nothing checked that the
// emitted bytes still satisfy that proto's spec.assert_* constraints, so a
// field becoming assert_required upstream surfaced only as a crash-looping pod
// days later (kernel's network/logger/rate_limit, 2026-08-20).
//
// ValidateDeployConfigs closes that gap by round-tripping every emitted
// artifact through the tacplus-generated <Msg>FromBytes + Validate<Msg> pair
// for the message that artifact populates — the same generated check the
// consumer runs at boot, run at generation time instead.

// generatedArtifact names one emitted artifact and the generated validator for
// the proto message its consumer parses it as.
type generatedArtifact struct {
	name      string
	protoName string
	data      []byte
	validate  func([]byte) error
}

// Each validator mirrors the consuming service's own boot sequence — parse the
// bytes, apply the generated defaults, then run the generated validator — so it
// fails on exactly what the consumer would reject and nothing else. The
// defaults pass deliberately does not recurse into submessages, which is why a
// bare `network: {}` does not satisfy kernel's assert_required network field.

// validateKernelConfig parses emitted kernel bootstrap YAML as kernel.Config
// and runs the generated validator.
func validateKernelConfig(data []byte) error {
	m, err := protoKernel.ConfigFromBytes(data)
	if err != nil {
		return err
	}
	protoKernel.ConfigDefaults(m)
	return protoKernel.ValidateConfig(m)
}

// validateKmakerConfig parses emitted kmaker config YAML as kmaker.Config and
// runs the generated validator.
func validateKmakerConfig(data []byte) error {
	m, err := protoKmaker.ConfigFromBytes(data)
	if err != nil {
		return err
	}
	protoKmaker.ConfigDefaults(m)
	return protoKmaker.ValidateConfig(m)
}

// validateServiceBootstrap parses an emitted service bootstrap YAML as the
// chassis grpcconfig.Config the ServiceManager loads at boot and runs the
// generated validator.
func validateServiceBootstrap(data []byte) error {
	m, err := protoGrpcConfig.ConfigFromBytes(data)
	if err != nil {
		return err
	}
	protoGrpcConfig.ConfigDefaults(m)
	return protoGrpcConfig.ValidateConfig(m)
}

// deployArtifacts lists every artifact in dc alongside the generated validator
// for the proto its consumer parses it as. Artifacts that are not a proto
// message (the CA PEM, the letsencrypt ACME options file) are not listed.
func deployArtifacts(dc *DeployConfigs) []generatedArtifact {
	var out []generatedArtifact

	for i, y := range dc.BootstrapYAMLs {
		name := fmt.Sprintf("bootstrap%d", i)
		if i < len(dc.BootstrapNames) {
			name = dc.BootstrapNames[i]
		}
		out = append(out, generatedArtifact{name, "kernel.Config", y, validateKernelConfig})
	}
	if dc.KmakerConfig != nil {
		out = append(out, generatedArtifact{"kmaker", "kmaker.Config", dc.KmakerConfig, validateKmakerConfig})
	}

	// Platform service bootstrap files. Each is the chassis grpcconfig.Config
	// the ServiceManager reads at boot; the service's own Config comes from
	// kernel state at runtime, not from this file.
	svc := []struct {
		name string
		data []byte
	}{
		{"echo", dc.EchoConfig},
		{"messaging", dc.MessagingConfig},
		{"mailbox", dc.MailboxConfig},
		{"personalauth", dc.PersonalauthConfig},
		{"gateway", dc.GatewayConfig},
		{"eventbus", dc.EventbusConfig},
		{"oauth", dc.OauthConfig},
		{"opa", dc.OpaConfig},
		{"stratum", dc.StratumConfig},
		{"letsencrypt", dc.LetsencryptConfig},
		{"blobstore", dc.BlobstoreConfig},
		{"operator", dc.OperatorBootstrapConfig},
	}
	for _, s := range svc {
		if s.data == nil {
			continue
		}
		out = append(out, generatedArtifact{s.name, "grpcconfig.Config", s.data, validateServiceBootstrap})
	}

	appNames := make([]string, 0, len(dc.AppConfigs))
	for name := range dc.AppConfigs {
		appNames = append(appNames, name)
	}
	sort.Strings(appNames)
	for _, name := range appNames {
		out = append(out, generatedArtifact{name, "grpcconfig.Config", dc.AppConfigs[name], validateServiceBootstrap})
	}

	return out
}

// ValidateDeployConfigs checks every generated artifact in dc against the
// generated validator for the proto message its consumer parses it as. It
// returns a non-nil error naming each artifact, the proto, and the generated
// failure — so an assert_required field the generator stopped emitting fails
// generation instead of a pod's boot.
func ValidateDeployConfigs(dc *DeployConfigs) error {
	var failures []string
	for _, a := range deployArtifacts(dc) {
		if err := a.validate(a.data); err != nil {
			failures = append(failures, fmt.Sprintf("  %s (%s): %v", a.name, a.protoName, err))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return errors.New("generated config does not satisfy the protos its consumers enforce:\n" +
		strings.Join(failures, "\n"))
}
