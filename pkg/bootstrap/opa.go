package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	protoFilesystem "gitlab.com/secondstarcavalier/schematic/platform/operator/tacplus/types/libs/filesystem"
	protoOpa "gitlab.com/secondstarcavalier/schematic/platform/operator/tacplus/types/common/opalib"
)

// OPAPolicyDir returns the conventional OPA policy directory within a config dir.
func OPAPolicyDir(cfgDir string) string {
	return filepath.Join(cfgDir, "services", "opa")
}

// BackendTokenPolicyDir returns the backend token OPA policy directory.
func BackendTokenPolicyDir(cfgDir string) string {
	return filepath.Join(cfgDir, "services", "opa-backend-token")
}

// ProxyPolicyDir returns the proxy OPA policy directory.
func ProxyPolicyDir(cfgDir string) string {
	return filepath.Join(cfgDir, "services", "opa-proxy-policy")
}

// BuildOPABundle reads .rego files from dir and returns a filesystem.Directory.
func BuildOPABundle(regoDir string) (*protoFilesystem.Directory, error) {
	return protoFilesystem.DirectoryFromFileSystem(regoDir)
}

// WriteOPABundle writes a filesystem.Directory policy bundle to disk.
func WriteOPABundle(dir *protoFilesystem.Directory, destPath string) error {
	return protoFilesystem.DirectoryToFileSystem(dir, destPath)
}

// BuildOpaPolicy creates an OpaPolicy proto with the given parameters.
func BuildOpaPolicy(entryPoint string, bundle *protoFilesystem.Directory) *protoOpa.OpaPolicy {
	return &protoOpa.OpaPolicy{
		EntryPoint: entryPoint,
		Bundle:     bundle,
		Created:    time.Now().Unix(),
	}
}

// DefaultOPABundle returns a filesystem.Directory containing a default
// entitlements policy seeded with trusted_issuer rules for each idpIssuer.
func DefaultOPABundle(idpIssuers []string) *protoFilesystem.Directory {
	var rules strings.Builder
	for _, iss := range idpIssuers {
		fmt.Fprintf(&rules, "trusted_issuer if {\n\tinput.claims.origin_iss == %q\n}\n", iss)
	}
	abacRego := fmt.Sprintf(defaultAbacRegoTemplate, rules.String())
	perm755 := &protoFilesystem.Permission{Owner: 7, Group: 5, Other: 5}
	perm644 := &protoFilesystem.Permission{Owner: 6, Group: 4, Other: 4}

	return &protoFilesystem.Directory{
		Name:       "opa",
		Permission: perm755,
		Files: []*protoFilesystem.File{
			{Name: "main.rego", Permission: perm644, Content: []byte(defaultMainRego)},
		},
		Directories: []*protoFilesystem.Directory{
			{
				Name:       "abac",
				Permission: perm755,
				Files: []*protoFilesystem.File{
					{Name: "attributes.rego", Permission: perm644, Content: []byte(abacRego)},
				},
			},
			{
				Name:       "rbac",
				Permission: perm755,
				Files: []*protoFilesystem.File{
					{Name: "roles.rego", Permission: perm644, Content: []byte(defaultRbacRego)},
				},
			},
		},
	}
}

// WriteDefaultOPAPolicy writes the default OPA entitlements policy to disk.
func WriteDefaultOPAPolicy(destDir string, idpIssuers []string) error {
	return WriteOPABundle(DefaultOPABundle(idpIssuers), destDir)
}

// LoadOPAPolicy reads an OPA policy directory from disk and returns an OpaPolicy proto.
func LoadOPAPolicy(regoDir, entryPoint string) (*protoOpa.OpaPolicy, error) {
	if _, err := os.Stat(regoDir); err != nil {
		return nil, fmt.Errorf("OPA policy directory %q: %w", regoDir, err)
	}
	bundle, err := BuildOPABundle(regoDir)
	if err != nil {
		return nil, fmt.Errorf("reading OPA bundle from %q: %w", regoDir, err)
	}
	return &protoOpa.OpaPolicy{
		EntryPoint: entryPoint,
		Bundle:     bundle,
		Created:    time.Now().Unix(),
	}, nil
}

const defaultMainRego = `# Entry point for the entitlements policy.
package entitlements

import data.entitlements.abac
import data.entitlements.rbac

default roles := set()
roles := merged if {
	merged := abac.roles | rbac.roles
	count(merged) > 0
}

default eval := {"token_entitlements": {}}

eval := {"token_entitlements": {
	"sentinel":     ["SENTINEL_RPCS_ANY"],
	"openai":       ["OPENAI_RPCS_ANY"],
	"anthropic":    ["ANTHROPIC_RPCS_ANY"],
	"claudesensor": ["CLAUDESENSOR_RPCS_ANY"],
}} if {
	roles["admin"]
}

eval := {"token_entitlements": {
	"openai":       ["OPENAI_RPCS_ANY"],
	"anthropic":    ["ANTHROPIC_RPCS_ANY"],
	"claudesensor": ["CLAUDESENSOR_RPCS_ANY"],
}} if {
	roles["user"]
	not roles["admin"]
}
`

const defaultAbacRegoTemplate = `# Attribute-based access control rules.
package entitlements.abac

default roles := set()

%s

roles := user_roles if {
	trusted_issuer
	groups := input.claims.groups
	is_array(groups)
	user_roles := {g | some g in groups; g != ""}
	count(user_roles) > 0
}

roles := {"admin"} if {
	trusted_issuer
	not input.claims.groups
}
`

const defaultRbacRego = `# Role-based access control rules.
package entitlements.rbac

default roles := set()
`
