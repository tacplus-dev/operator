package bootstrap

import (
	"fmt"
	"os"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"gopkg.in/yaml.v3"

	protoEntitlements "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/types/generated/entitlements"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/types/services/operator"
)

// ApplyTopologyPreset sets topology fields from a named preset.
// Presets: "small" (default), "medium", "large".
func ApplyTopologyPreset(cfg *protoOperator.OperatorConfig, preset string) {
	if cfg.Topology == nil {
		cfg.Topology = &protoOperator.Topology{}
	}
	t := cfg.Topology
	switch strings.ToLower(preset) {
	case "large":
		t.KernelNodes = 9
		t.SentinelNodes = 5
		t.EventbusNodes = 5
		t.OauthNodes = 5
		t.OpaNodes = 5
		t.TimedbNodes = 5
		t.GraphdbNodes = 5
		t.ArgusNodes = 5
		t.StratumNodes = 5
		t.SamlNodes = 5
	case "medium":
		t.KernelNodes = 5
		t.SentinelNodes = 3
		t.EventbusNodes = 3
		t.OauthNodes = 3
		t.OpaNodes = 3
		t.TimedbNodes = 3
		t.GraphdbNodes = 3
		t.ArgusNodes = 3
		t.StratumNodes = 3
		t.SamlNodes = 3
	default: // small — matches proto field annotation defaults
		t.KernelNodes = 3
		t.SentinelNodes = 3
		t.EventbusNodes = 3
		t.OauthNodes = 3
		t.OpaNodes = 3
		t.TimedbNodes = 3
		t.GraphdbNodes = 3
		t.ArgusNodes = 3
		t.StratumNodes = 3
		t.SamlNodes = 3
	}
}

// allServiceGrants returns the _ANY grant for every service field on
// TokenEntitlements, derived via proto reflection.
func allServiceGrants() map[string][]string {
	var ep protoEntitlements.TokenEntitlements
	md := ep.ProtoReflect().Descriptor()
	grants := make(map[string][]string)
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		if fd.Kind() != protoreflect.EnumKind || !fd.IsList() {
			continue
		}
		enumDesc := fd.Enum()
		for j := 0; j < enumDesc.Values().Len(); j++ {
			ev := enumDesc.Values().Get(j)
			if ev.Number() == 99 {
				grants[string(fd.Name())] = []string{string(ev.Name())}
				break
			}
		}
	}
	return grants
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
