package bootstrap

import (
	"fmt"
	"os"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"gopkg.in/yaml.v3"

	"gitlab.com/seabeam/schematic/platform/services/operator/model/grants"
	protoEntitlements "gitlab.com/seabeam/schematic/platform/services/operator/model/types/generated/entitlements"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// ApplyTopologyPreset sets topology fields from a named preset.
// Presets: "small" (default), "medium", "large".
func ApplyTopologyPreset(cfg *protoOperator.Config, preset string) {
	if cfg.Topology == nil {
		cfg.Topology = &protoOperator.Topology{}
	}
	t := cfg.Topology
	switch strings.ToLower(preset) {
	case "large":
		t.KernelNodes = 9
		t.EventbusNodes = 5
		t.OauthNodes = 5
		t.OpaNodes = 5
		t.StratumNodes = 5
	case "medium":
		t.KernelNodes = 5
		t.EventbusNodes = 3
		t.OauthNodes = 3
		t.OpaNodes = 3
		t.StratumNodes = 3
	default: // small — matches proto field annotation defaults
		t.KernelNodes = 3
		t.EventbusNodes = 3
		t.OauthNodes = 3
		t.OpaNodes = 3
		t.StratumNodes = 3
	}
}

// entitlementGrantsField is the field on TokenEntitlements that carries grants.
// Named once so the shape assumption is checked against the descriptor rather
// than assumed.
const entitlementGrantsField = "grants"

// allServiceGrants returns the profile entry that grants everything the fleet
// exposes: the package-wide wildcard for every package in the GENERATED grant
// catalog, under TokenEntitlements' actual grant field.
//
// It used to reflect over TokenEntitlements looking for repeated-ENUM fields,
// one per service, and take each enum's value 99 (<SVC>_RPCS_ANY). That shape
// is gone: the message carries `repeated string grants = 1`. The loop matched
// nothing, returned an empty map, and `add-profile --all` wrote a profile with
// zero grants and reported success. Fail loud if the field shape moves again.
func allServiceGrants() map[string][]string {
	var ep protoEntitlements.TokenEntitlements
	fd := ep.ProtoReflect().Descriptor().Fields().ByName(entitlementGrantsField)
	if fd == nil || fd.Kind() != protoreflect.StringKind || !fd.IsList() {
		panic(fmt.Sprintf(
			"TokenEntitlements.%s is not a repeated string; regenerate the model or update allServiceGrants",
			entitlementGrantsField))
	}

	var tokens []string
	for _, token := range grants.Tokens() {
		// Package wildcards only: a capability wildcard (<PKG>_READ_ANY etc.) is
		// a strict subset of its package's ANY, and every exact token is too.
		if g, ok := grants.Lookup(token); ok && g.Capability == "ANY" {
			tokens = append(tokens, token)
		}
	}
	return map[string][]string{entitlementGrantsField: tokens}
}

// AddProfile adds or updates an entitlement profile in services/kernel.yaml.
// Idempotent: if a profile with the same ID exists, its grants are replaced.
func AddProfile(cfgDir, id string, grants map[string][]string) error {
	kernelPath := fmt.Sprintf("%s/services/kernel.yaml", cfgDir)

	data, err := os.ReadFile(kernelPath)
	if err != nil {
		return fmt.Errorf("reading kernel config: %w", err)
	}

	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parsing kernel config: %w", err)
	}

	entry := map[string]any{"id": id}
	for svc, rpcs := range grants {
		rpcList := make([]any, len(rpcs))
		for i, r := range rpcs {
			rpcList[i] = r
		}
		entry[svc] = rpcList
	}

	var profiles []any
	if existing, ok := cfg["entitlement_profiles"]; ok {
		if list, ok := existing.([]any); ok {
			profiles = list
		}
	}

	found := false
	for i, item := range profiles {
		if m, ok := item.(map[string]any); ok {
			if m["id"] == id {
				profiles[i] = entry
				found = true
				break
			}
		}
	}
	if !found {
		profiles = append(profiles, entry)
	}

	cfg["entitlement_profiles"] = profiles

	outData, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling kernel config: %w", err)
	}

	return os.WriteFile(kernelPath, outData, 0644)
}

// parseGrantFlag parses a "service=RPC1,RPC2" string into service name and RPC list.
func parseGrantFlag(s string) (string, []string, error) {
	parts := strings.SplitN(s, "=", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", nil, fmt.Errorf("invalid --grant %q: expected service=RPC1,RPC2", s)
	}
	rpcs := strings.Split(parts[1], ",")
	for i := range rpcs {
		rpcs[i] = strings.TrimSpace(rpcs[i])
	}
	return parts[0], rpcs, nil
}
