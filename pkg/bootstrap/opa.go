package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

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

// TenantIdp binds one tenant-owned IDP provider, or one claim matcher on a
// shared cluster IDP, to the tenant's authorization namespace. Per
// oauth.OAuthProvider, the (issuer_url, client_id) pair uniquely identifies the
// tenant. Per oauth.ClaimMatcher, (issuer_url, claim_key, claim_value) does;
// ClaimKey is set only for a claim matcher, and ClientID only for a provider.
type TenantIdp struct {
	IssuerURL       string
	ClientID        string
	ClaimKey        string
	ClaimValue      string
	TenantNamespace string
}

// emailDomainClaimKey is the claim oauth mints from an id_token's email, and
// only when the IdP marks that email verified.
const emailDomainClaimKey = "email_domain"

// hdClaimKey is Google's hosted-domain claim, present for Workspace users.
const hdClaimKey = "hd"

// publicMailDomains are consumer mail domains no tenant can claim through an
// email_domain matcher: anyone can sign up for an address there, so the claim
// says nothing about which organisation the user belongs to.
var publicMailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "icloud.com": true, "me.com": true,
	"mac.com": true, "outlook.com": true, "hotmail.com": true, "live.com": true,
	"msn.com": true, "yahoo.com": true, "aol.com": true, "proton.me": true,
	"protonmail.com": true, "gmx.com": true, "yandex.com": true, "zoho.com": true,
}

// TenantConfigDir returns the directory holding one tenant's config slices
// within a config dir.
func TenantConfigDir(cfgDir, tenant string) string {
	return filepath.Join(cfgDir, "tenants", tenant)
}

// LoadTenantIdps reads tenants/<name>/oauth.yaml — an oauth.OAuthTenantConfig —
// for every tenant named in operator.yaml, and returns the bindings its enabled
// providers and its claim matchers declare. A tenant with no file contributes
// nothing: its users then resolve to no namespace, and kernel refuses to mint
// for them. That is the intended fail-closed behaviour, not a gap to paper over.
func LoadTenantIdps(cfgDir string) ([]TenantIdp, error) {
	cfg, err := LoadConfig(cfgDir)
	if err != nil {
		return nil, err
	}
	return loadTenantIdps(cfgDir, cfg.GetTenants())
}

// loadTenantIdps is LoadTenantIdps for an explicit tenant list, so a tenant
// being approved can be checked against the others before operator.yaml lists
// it.
func loadTenantIdps(cfgDir string, tenants []string) ([]TenantIdp, error) {
	// Keyed by (issuer_url, client_id) — the pair the proto says identifies the
	// tenant. Two tenants claiming the same pair is a misconfiguration that
	// would render a policy OPA rejects at eval time, so reject it here.
	seen := map[[2]string]string{}
	seenMatchers := map[[3]string]string{}
	// Keyed by (issuer, domain) across the hd and email_domain claims: a
	// Workspace user carries both for their own domain, so hd=X in one tenant
	// and email_domain=X in another would match that user to both.
	seenDomains := map[[2]string]string{}
	var out []TenantIdp
	for _, tenant := range tenants {
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
		for _, m := range tc.GetClaimMatchers() {
			// oauth mints email_domain lowercased, so any other spelling never
			// matches; a public mail domain or an empty one would let anyone in.
			if v := m.GetClaimValue(); m.GetClaimKey() == emailDomainClaimKey {
				if v == "" || v != strings.ToLower(v) ||
					strings.IndexFunc(v, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
					return nil, fmt.Errorf("%s: email_domain claim value %q must be a non-empty lowercase ASCII domain", path, v)
				}
				if publicMailDomains[v] {
					return nil, fmt.Errorf("%s: email_domain %q is a public mail domain and cannot be claimed by a tenant", path, v)
				}
			}
			if m.GetProviderIssuerUrl() == "" || m.GetClaimKey() == "" || m.GetClaimValue() == "" {
				continue
			}
			if k := m.GetClaimKey(); k == hdClaimKey || k == emailDomainClaimKey {
				dkey := [2]string{m.GetProviderIssuerUrl(), m.GetClaimValue()}
				if prev, ok := seenDomains[dkey]; ok && prev != ns {
					return nil, fmt.Errorf("domain %s on %s is claimed by both %s and %s (hd or email_domain)",
						dkey[1], dkey[0], prev, ns)
				}
				seenDomains[dkey] = ns
			}
			key := [3]string{m.GetProviderIssuerUrl(), m.GetClaimKey(), m.GetClaimValue()}
			if prev, ok := seenMatchers[key]; ok {
				if prev != ns {
					return nil, fmt.Errorf("claim matcher (%s, %s=%s) is claimed by both %s and %s",
						key[0], key[1], key[2], prev, ns)
				}
				continue
			}
			seenMatchers[key] = ns
			out = append(out, TenantIdp{IssuerURL: key[0], ClaimKey: key[1], ClaimValue: key[2], TenantNamespace: ns})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].IssuerURL != out[j].IssuerURL {
			return out[i].IssuerURL < out[j].IssuerURL
		}
		if out[i].ClientID != out[j].ClientID {
			return out[i].ClientID < out[j].ClientID
		}
		if out[i].ClaimKey != out[j].ClaimKey {
			return out[i].ClaimKey < out[j].ClaimKey
		}
		return out[i].ClaimValue < out[j].ClaimValue
	})
	return out, nil
}

// tenantNamespaceRego renders the entitlements.tenants policy: the
// (issuer, client_id) and (issuer, claim) → namespace table the eval rule reads.
//
// A claim matcher gets the claim rule alone: its issuer is a shared cluster
// IDP, so it never gets an issuer-only rule, and it counts as a tenant of that
// issuer so a provider on the same issuer loses its issuer-only rule too.
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
		if idp.ClaimKey != "" {
			b.WriteString(claimNamespaceRule(idp))
			continue
		}
		b.WriteString(exactNamespaceRule(idp))
	}

	byIssuer := map[string]map[string]bool{}
	for _, idp := range idps {
		if byIssuer[idp.IssuerURL] == nil {
			byIssuer[idp.IssuerURL] = map[string]bool{}
		}
		byIssuer[idp.IssuerURL][idp.TenantNamespace] = true
	}
	for _, idp := range idps {
		if idp.ClaimKey != "" || len(byIssuer[idp.IssuerURL]) != 1 {
			continue
		}
		delete(byIssuer, idp.IssuerURL)
		b.WriteString(issuerNamespaceRule(idp))
	}

	b.WriteString(tenantNamespaceRegoFooter)
	return b.String()
}

// exactNamespaceRule is the namespace rule matching issuer AND audience.
func exactNamespaceRule(idp TenantIdp) string {
	return fmt.Sprintf("\nnamespace := %q if {\n\tinput.claims.origin_iss == %q\n\taud_matches(%q)\n}\n",
		idp.TenantNamespace, idp.IssuerURL, idp.ClientID)
}

// claimNamespaceRule is the namespace rule matching the cluster IDP AND one
// claim value. The issuer is the configured provider issuer, which oauth
// forwards as provider_iss; origin_iss is the token's own iss and differs from
// it for a per-directory IDP (an Entra `common` provider). The oauth
// broker forwards a claim that is not a well-known JWT claim under its own
// name, so input.claims[key] is the IDP's claim (e.g. hd, tid).
func claimNamespaceRule(idp TenantIdp) string {
	return fmt.Sprintf("\nnamespace := %q if {\n\tinput.claims.provider_iss == %q\n\tinput.claims[%q] == %q\n}\n",
		idp.TenantNamespace, idp.IssuerURL, idp.ClaimKey, idp.ClaimValue)
}

// issuerNamespaceRule is the issuer-only namespace rule. It is correct only
// for an issuer that names exactly one tenant; see tenantNamespaceRego.
func issuerNamespaceRule(idp TenantIdp) string {
	return fmt.Sprintf("\nnamespace := %q if {\n\tinput.claims.origin_iss == %q\n}\n",
		idp.TenantNamespace, idp.IssuerURL)
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

# This is the PLATFORM fallback, used only when a config dir ships no
# services/opa/ of its own. It names no application package: the operator is
# built against one app's catalog, and a fallback that granted that app's
# packages (it once granted testapp's ECHO_ANY/TASKS_ANY/QUERY_ANY, none of which
# exist in sonar's catalog) hands out symbols that expand to nothing elsewhere.
# An application ships its own services/opa/main.rego with its user grants
# (sonar/backend/bootstrap/services/opa/main.rego). Without one, only admin gets
# anything, and that is kernel's administrative surface.

# admin — kernel's administrative surface.
eval := {"namespace": tenants.namespace, "token_entitlements": {
	"grants": [
		"KERNEL_ANY",
	],
}} if {
	roles["admin"]
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

// tenantNamespaceRegoHeader/Footer bracket the generated (issuer, client_id)
// and (issuer, claim) → namespace rules.
const tenantNamespaceRegoHeader = `# IDP (issuer, client_id) or (issuer, claim) → tenant authorization namespace.
# Generated from tenants/<name>/oauth.yaml: each oauth.OAuthTenantConfig
# provider's (issuer_url, client_id) pair, and each claim matcher's
# (provider_issuer_url, claim_key, claim_value) on a shared cluster IDP,
# uniquely identifies the tenant.
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
