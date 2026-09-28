package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	protoOauth "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/oauth"
	protoFilesystem "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/filesystem"
	protoOpa "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/opalib"
)

// Minimum accepted by the generated OPA policy validators.
const stablePolicyCreated int64 = 1735689600

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
		Created:    stablePolicyCreated,
	}
}

// DefaultOPABundle returns a filesystem.Directory containing a default
// entitlements policy seeded with trusted_issuer rules for each idpIssuer and
// (issuer, client_id) → tenant namespace rules for each tenantIdp.
func DefaultOPABundle(idpIssuers []string, tenantIdps []TenantIdp) *protoFilesystem.Directory {
	var rules strings.Builder
	for _, iss := range idpIssuers {
		fmt.Fprintf(&rules, "trusted_issuer if {\n\tinput.claims.origin_iss == %q\n}\n", iss)
	}
	abacRego := fmt.Sprintf(defaultIdpRegoTemplate, rules.String())
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
				// idp/claims.rego — maps incoming IDP token claims to schematic roles.
				// Seeded with trusted_issuer rules for each configured IDP issuer URL.
				Name:       "idp",
				Permission: perm755,
				Files: []*protoFilesystem.File{
					{Name: "claims.rego", Permission: perm644, Content: []byte(abacRego)},
				},
			},
			{
				// grants/static.rego — static role grants not derived from IDP claims.
				// Empty by default; add rules here for service accounts or fixed roles.
				Name:       "grants",
				Permission: perm755,
				Files: []*protoFilesystem.File{
					{Name: "static.rego", Permission: perm644, Content: []byte(defaultGrantsRego)},
				},
			},
			{
				// tenants/namespace.rego — (IDP issuer, client_id) → tenant
				// authorization namespace. Kernel stamps the minted user token
				// with what this emits and refuses the mint when it emits
				// nothing, so an unlisted issuer cannot obtain a token.
				Name:       "tenants",
				Permission: perm755,
				Files: []*protoFilesystem.File{
					{Name: "namespace.rego", Permission: perm644, Content: []byte(tenantNamespaceRego(tenantIdps))},
				},
			},
		},
	}
}

// TenantIdp binds one tenant-owned IDP provider to the tenant's authorization
// namespace. Per oauth.OAuthProvider, the (issuer_url, client_id) pair
// uniquely identifies the tenant — no claim matching is involved.
type TenantIdp struct {
	IssuerURL       string
	ClientID        string
	TenantNamespace string
}

// TenantConfigDir returns the directory holding one tenant's config slices
// within a config dir.
func TenantConfigDir(cfgDir, tenant string) string {
	return filepath.Join(cfgDir, "tenants", tenant)
}

// LoadTenantIdps reads tenants/<name>/oauth.yaml — an oauth.OAuthTenantConfig —
// for every tenant named in operator.yaml, and returns the bindings its enabled
// providers declare. A tenant with no file contributes nothing: its users then
// resolve to no namespace, and kernel refuses to mint for them. That is the
// intended fail-closed behaviour, not a gap to paper over.
func LoadTenantIdps(cfgDir string) ([]TenantIdp, error) {
	cfg, err := LoadConfig(cfgDir)
	if err != nil {
		return nil, err
	}

	// Keyed by (issuer_url, client_id) — the pair the proto says identifies the
	// tenant. Two tenants claiming the same pair is a misconfiguration that
	// would render a policy OPA rejects at eval time, so reject it here.
	seen := map[[2]string]string{}
	var out []TenantIdp
	for _, tenant := range cfg.GetTenants() {
		path := filepath.Join(TenantConfigDir(cfgDir, tenant), "oauth.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		tc, err := protoOauth.OAuthTenantConfigFromBytes(data)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		ns := tenantNamespacePrefix + tenant
		for _, p := range tc.GetProviders() {
			if !p.GetEnable() || p.GetIssuerUrl() == "" || p.GetClientId() == "" {
				continue
			}
			key := [2]string{p.GetIssuerUrl(), p.GetClientId()}
			if prev, ok := seen[key]; ok {
				if prev != ns {
					return nil, fmt.Errorf("IDP (%s, %s) is claimed by both %s and %s",
						key[0], key[1], prev, ns)
				}
				continue
			}
			seen[key] = ns
			out = append(out, TenantIdp{IssuerURL: key[0], ClientID: key[1], TenantNamespace: ns})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].IssuerURL != out[j].IssuerURL {
			return out[i].IssuerURL < out[j].IssuerURL
		}
		return out[i].ClientID < out[j].ClientID
	})
	return out, nil
}

// tenantNamespaceRego renders the entitlements.tenants policy: the
// (issuer, client_id) → namespace table the eval rule reads.
//
// Two rule shapes are emitted per binding. The exact one matches issuer AND
// audience. The issuer-only one is emitted only when that issuer names exactly
// one tenant, because the claim set kernel forwards to OPA does not always
// carry the audience (oauth's BrokerCliAuth re-auth path forwards sub, email,
// origin_iss and groups only). An issuer shared by two tenants therefore gets
// the exact rule alone, and a request that cannot prove its audience resolves
// to no namespace rather than to an arbitrary one of them.
func tenantNamespaceRego(idps []TenantIdp) string {
	var b strings.Builder
	b.WriteString(tenantNamespaceRegoHeader)

	for _, idp := range idps {
		fmt.Fprintf(&b, "\nnamespace := %q if {\n\tinput.claims.origin_iss == %q\n\taud_matches(%q)\n}\n",
			idp.TenantNamespace, idp.IssuerURL, idp.ClientID)
	}

	byIssuer := map[string]map[string]bool{}
	for _, idp := range idps {
		if byIssuer[idp.IssuerURL] == nil {
			byIssuer[idp.IssuerURL] = map[string]bool{}
		}
		byIssuer[idp.IssuerURL][idp.TenantNamespace] = true
	}
	for _, idp := range idps {
		if len(byIssuer[idp.IssuerURL]) != 1 {
			continue
		}
		delete(byIssuer, idp.IssuerURL)
		fmt.Fprintf(&b, "\nnamespace := %q if {\n\tinput.claims.origin_iss == %q\n}\n",
			idp.TenantNamespace, idp.IssuerURL)
	}

	b.WriteString(tenantNamespaceRegoFooter)
	return b.String()
}

// WriteDefaultOPAPolicy writes the default OPA entitlements policy to disk.
func WriteDefaultOPAPolicy(destDir string, idpIssuers []string, tenantIdps []TenantIdp) error {
	return WriteOPABundle(DefaultOPABundle(idpIssuers, tenantIdps), destDir)
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
		Created:    stablePolicyCreated,
	}, nil
}

// LoadOptionalOPAPolicy loads an OPA policy directory when it exists.
func LoadOptionalOPAPolicy(regoDir, entryPoint string) (*protoOpa.OpaPolicy, error) {
	if _, err := os.Stat(regoDir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("OPA policy directory %q: %w", regoDir, err)
	}
	return LoadOPAPolicy(regoDir, entryPoint)
}

const defaultMainRego = `# Entry point for the entitlements policy.
# Combines roles from idp/ (IDP claim-derived) and grants/ (static), then
# maps the resulting role set to service entitlements in the schematic token.
package entitlements

import data.entitlements.idp
import data.entitlements.grants
import data.entitlements.tenants

default roles := set()
roles := merged if {
	merged := idp.roles | grants.roles
	count(merged) > 0
}

default eval := {"token_entitlements": {}}

# namespace is the tenant subtree the caller's IDP binds them to, resolved by
# tenants/namespace.rego from the (issuer, client_id) pair. Kernel stamps the
# minted user token with it and REFUSES the mint when it is absent or empty --
# there is no fallback to the cluster root. An issuer this policy cannot name a
# tenant for therefore cannot log in, which is the point.

# The grants below name only services the platform actually ships. An earlier
# version of this generator granted MCP_ANY, ANTHROPIC_ANY, GITHUB_ANY,
# GITLAB_ANY and OPENAI_ANY -- all application services that have since left the
# platform -- which meant the ` + "`user`" + ` role, whose entire grant set was
# OPENAI_ANY + ANTHROPIC_ANY, resolved to nothing usable at all. That was fixed
# by hand in the generated output but never here, so regenerating reintroduced
# the broken set. Keep this list and the shipped service set in step.

# admin — full access, including kernel's administrative surface.
eval := {"namespace": tenants.namespace, "token_entitlements": {
	"grants": [
		"SENTINEL_ANY",
		"ECHO_ANY",
		"TASKS_ANY",
		"QUERY_ANY",
		"KERNEL_ANY",
	],
}} if {
	roles["admin"]
}

# prod / user — everything the shipped webapp needs EXCEPT kernel.
#
# Both seeded groups land here. The distinction that matters is kernel: it is
# the cluster's administrative surface and its own token-minting authority, so a
# non-admin does not get it. QUERY_ANY sits with the data-plane grants rather
# than with kernel: the record plane is the record of truth that every query
# enters through, and its protection is egress redaction plus server-pinned
# predicates, not RPC-level denial. A record type added to the plane needs no new
# grant -- QUERY_ANY already covers the RPCs the trait generates over it -- so
# this list scales with services, never with application types.
#
# QUERY_ANY, not STRATUM_ANY: since BIG GOAL 3 item 16 the record query plane is
# a TRAIT (testapp/proto/vservices/query.proto), so its grants are keyed on the
# contract rather than on the service that hosts it. STRATUM_* is no longer in
# the generated catalog at all, and the test below is what would have caught it.
eval := {"namespace": tenants.namespace, "token_entitlements": {
	"grants": [
		"SENTINEL_ANY",
		"ECHO_ANY",
		"TASKS_ANY",
		"QUERY_ANY",
	],
}} if {
	some role in {"prod", "user"}
	roles[role]
	not roles["admin"]
}
`

// defaultIdpRegoTemplate is the generated IDP claims → roles policy.
// %s is replaced with trusted_issuer rules derived from configured IDP issuers.
const defaultIdpRegoTemplate = `# IDP claims → schematic roles.
# Validates the incoming external IDP token and maps its claim attributes
# (groups, sub) to the internal role set used by the entitlements policy.
# trusted_issuer rules below are seeded from services/oauth.yaml providers.
package entitlements.idp

default roles := set()

%s

roles := user_roles if {
	trusted_issuer
	groups := input.claims.groups
	is_array(groups)
	user_roles := {g | some g in groups; g != ""}
	count(user_roles) > 0
}

# A token with no groups claim gets NO roles -- the ` + "`default roles := set()`" + `
# above is the answer. An earlier version of this generator mapped the missing
# claim to {"admin"}, which made the absence of an assertion the strongest
# assertion an IDP could make: any trusted issuer that simply did not emit
# groups handed the caller kernel's administrative surface. Roles are granted by
# a claim that is present, never by one that is absent.
`

const defaultGrantsRego = `# Static role grants — not derived from IDP claims.
# Add rules here for service accounts, fixed role assignments, or
# any access that should be granted independently of IDP group membership.
package entitlements.grants

default roles := set()
`

// tenantNamespaceRegoHeader/Footer bracket the generated (issuer, client_id) →
// namespace rules.
const tenantNamespaceRegoHeader = `# IDP (issuer, client_id) → tenant authorization namespace.
# Generated from tenants/<name>/oauth.yaml (oauth.OAuthTenantConfig.providers);
# the (issuer_url, client_id) pair uniquely identifies the tenant, so no claim
# matching is involved.
#
# ` + "`default namespace := \"\"`" + ` is the answer for an issuer that is not
# listed. Kernel refuses to mint a user token whose namespace the policy could
# not name -- it does NOT fall back to the cluster root -- so an unknown issuer
# authenticates against nothing.
package entitlements.tenants

default namespace := ""
`

const tenantNamespaceRegoFooter = `
# aud is either a string or an array of strings in the JWT spec, and the oauth
# broker forwards the IDP's ` + "`aud`" + ` claim as ` + "`origin_aud`" + `.
aud_matches(client_id) if {
	is_string(input.claims.origin_aud)
	input.claims.origin_aud == client_id
}

aud_matches(client_id) if {
	is_array(input.claims.origin_aud)
	some a in input.claims.origin_aud
	a == client_id
}
`
