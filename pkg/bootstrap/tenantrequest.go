package bootstrap

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"

	protoTenantConfig "gitlab.com/seabeam/schematic/platform/services/operator/model/types/generated/tenant_config"
	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	protoOauth "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/oauth"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// TenantRequestsDir holds tenant requests awaiting approval, one
// <name>.yaml per requested tenant. A request is an oauth.OAuthTenantConfig —
// the IdP(s) the new tenant signs in with, or `providers: []` — and is exactly
// what approval installs as tenants/<name>/oauth.yaml. Filing one changes
// nothing; only approve-tenant does.
const TenantRequestsDir = "tenant-requests"

// peopleTenant is kernel's reserved tenant for personal principals. It is not
// created from operator config and cannot be requested.
const peopleTenant = "people"

// TenantRequestPath returns the path of the request for tenant within a config dir.
func TenantRequestPath(cfgDir, tenant string) string {
	return filepath.Join(cfgDir, TenantRequestsDir, tenant+".yaml")
}

var approveTenantCmd = &cobra.Command{
	Use:   "approve-tenant <name>",
	Short: "Approve a tenant request and add the tenant to the cluster",
	Long: `Approves tenant-requests/<name>.yaml (an oauth.OAuthTenantConfig) and makes
<name> a tenant of this cluster:

  operator.yaml                          <name> appended to tenants
  tenants/<name>/oauth.yaml              the request, moved into place
  services/opa/tenants/namespace.rego    the tenant's IdP → /tenants/<name> rules
                                         appended (only when services/opa exists)
  services/opa/grants/static.rego        role "user" for each email_domain claim
                                         matcher appended (likewise)
  services/kernel.yaml                   one delegate per tenant-owned service for
                                         <name>; every other delegate untouched

The next deploy renders the tenant's tenant-owned services.

Example:
  schematic-operator approve-tenant acme`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := ApproveTenant(resolveConfigDir(), args[0]); err != nil {
			return err
		}
		fmt.Printf("Tenant %s approved.\n", args[0])
		fmt.Printf("Run 'schematic-operator generate' to rebuild.\n")
		return nil
	},
}

// ApproveTenant turns the pending request for tenant into a tenant of the
// cluster. Everything is checked before anything is written: the name, that it
// is not already a tenant, that the request parses, and that none of its IdPs
// is already bound to another tenant.
func ApproveTenant(cfgDir, tenant string) error {
	cfg, err := LoadConfig(cfgDir)
	if err != nil {
		return err
	}
	if err := validateNewTenant(cfg, tenant); err != nil {
		return err
	}
	withTenant := proto.Clone(cfg).(*protoOperator.Config)
	withTenant.Tenants = append(withTenant.Tenants, tenant)
	kernelPath := filepath.Join(cfgDir, "services", "kernel.yaml")
	kernelYAML, err := withTenantDelegates(kernelPath, withTenant, tenant)
	if err != nil {
		return err
	}

	reqPath := TenantRequestPath(cfgDir, tenant)
	if _, err := os.Stat(reqPath); err != nil {
		return fmt.Errorf("no request for tenant %q: %w", tenant, err)
	}
	oauthPath := filepath.Join(TenantConfigDir(cfgDir, tenant), "oauth.yaml")
	if _, err := os.Stat(oauthPath); err == nil {
		return fmt.Errorf("%s already exists; tenant %q is not new", oauthPath, tenant)
	}

	// Move the request into place, then validate it exactly as every later
	// load will — loadTenantIdps parses it and refuses an (issuer, client_id)
	// another tenant already claims. A rejected request goes back where it was.
	if err := os.MkdirAll(TenantConfigDir(cfgDir, tenant), 0755); err != nil {
		return fmt.Errorf("creating tenant directory: %w", err)
	}
	if err := os.Rename(reqPath, oauthPath); err != nil {
		return fmt.Errorf("installing tenant request: %w", err)
	}
	tenants := append(append([]string{}, cfg.GetTenants()...), tenant)
	idps, err := loadTenantIdps(cfgDir, tenants)
	if err == nil {
		err = syncTenantNamespaceRules(cfgDir, tenant, idps)
	}
	if err == nil {
		err = syncTenantRoleRules(cfgDir, tenant, idps)
	}
	if err != nil {
		if rbErr := os.Rename(oauthPath, reqPath); rbErr != nil {
			return fmt.Errorf("%w (and restoring the request failed: %v)", err, rbErr)
		}
		return err
	}

	if err := appendTenantToSettings(cfgDir, tenant); err != nil {
		return err
	}
	return os.WriteFile(kernelPath, kernelYAML, 0644)
}

var syncTenantRulesCmd = &cobra.Command{
	Use:   "sync-tenant-rules <name>",
	Short: "Sync an existing tenant's oauth.yaml routing into namespace.rego and static.rego",
	Long: `Brings services/opa/tenants/namespace.rego up to date with
tenants/<name>/oauth.yaml for a tenant operator.yaml already lists: each of
its providers and claim_matchers renders the rule approve-tenant would append,
and only the rules the file does not already hold are appended. A generated
rule for the tenant that its oauth.yaml no longer renders (say, a claim
matcher whose provider issuer changed) is removed. services/opa/grants/static.rego
is synced the same way with the role "user" rule each email_domain claim
matcher renders, and gains the role "person" rule (an Apple, Google or
Microsoft login no tenant routes) if it lacks it. Hand-written rules are left as they are, and running it again
changes nothing.

Every tenant's oauth.yaml is checked first, exactly as deploy checks it; a
public mail domain, or an IdP or domain another tenant claims, is refused and
the file is left unchanged.

Example:
  schematic-operator sync-tenant-rules acme`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := SyncTenantRules(resolveConfigDir(), args[0]); err != nil {
			return err
		}
		fmt.Printf("Namespace and role rules for tenant %s are up to date.\n", args[0])
		fmt.Printf("Run 'schematic-operator generate' to rebuild.\n")
		return nil
	},
}

// SyncTenantRules makes tenant's generated rules in
// services/opa/tenants/namespace.rego and services/opa/grants/static.rego the
// ones tenants/<tenant>/oauth.yaml declares: missing rules are appended, stale
// ones removed. tenant must already be listed in operator.yaml; new tenants go
// through ApproveTenant.
func SyncTenantRules(cfgDir, tenant string) error {
	cfg, err := LoadConfig(cfgDir)
	if err != nil {
		return err
	}
	if !slices.Contains(cfg.GetTenants(), tenant) {
		return fmt.Errorf("%q is not a tenant of this cluster", tenant)
	}
	idps, err := loadTenantIdps(cfgDir, cfg.GetTenants())
	if err != nil {
		return err
	}
	if err := syncTenantNamespaceRules(cfgDir, tenant, idps); err != nil {
		return err
	}
	return syncTenantRoleRules(cfgDir, tenant, idps)
}

// ApproveTenantRecords approves every tenant kernel's ConfigState records as
// approved that cfgDir does not list yet, and returns their names. A tenant is
// approved when it is ACTIVE and its config entry exists: kernel's
// TenantAdmin wrote both, as the entitled person who thumbed the request. Its
// oauth slice is filed as tenant-requests/<name>.yaml — the request a person
// would file — and ApproveTenant does the rest, so a record and the CLI make
// the same tenant. A tenant already listed is skipped, so replaying the same
// state changes nothing. A refused tenant leaves the config dir as it was and
// does not stop the others; the refusals are returned together.
func ApproveTenantRecords(cfgDir string, state *protoKernel.ConfigState) ([]string, error) {
	cfg, err := LoadConfig(cfgDir)
	if err != nil {
		return nil, err
	}
	listed := make(map[string]bool, len(cfg.GetTenants()))
	for _, t := range cfg.GetTenants() {
		listed[t] = true
	}
	configs := make(map[string]*protoTenantConfig.TenantConfig, len(state.GetTenantConfigs()))
	for _, e := range state.GetTenantConfigs() {
		configs[e.GetTenantName()] = e.GetConfig()
	}

	var approved []string
	var errs []error
	for _, t := range state.GetTenants() {
		tenant := t.GetName()
		tc, ok := configs[tenant]
		if t.GetStatus() != protoKernel.TenantStatus_TENANT_STATUS_ACTIVE || !ok || listed[tenant] {
			continue
		}
		reqPath := TenantRequestPath(cfgDir, tenant)
		if _, err := os.Stat(reqPath); err == nil {
			errs = append(errs, fmt.Errorf("tenant %q: %s is already pending; a person decides which request wins", tenant, reqPath))
			continue
		}
		oauth := tc.GetOauth()
		if oauth == nil {
			oauth = &protoOauth.OAuthTenantConfig{}
		}
		request, err := protoOauth.OAuthTenantConfigToYAML(oauth)
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %q: marshaling request: %w", tenant, err))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(reqPath), 0755); err != nil {
			errs = append(errs, fmt.Errorf("tenant %q: creating %s: %w", tenant, TenantRequestsDir, err))
			continue
		}
		if err := os.WriteFile(reqPath, request, 0644); err != nil {
			errs = append(errs, fmt.Errorf("tenant %q: filing request: %w", tenant, err))
			continue
		}
		if err := ApproveTenant(cfgDir, tenant); err != nil {
			// ApproveTenant puts a refused request back where it was filed;
			// take it away again so the next pass starts clean.
			if rmErr := os.Remove(reqPath); rmErr != nil && !os.IsNotExist(rmErr) {
				err = fmt.Errorf("%w (and removing the request failed: %v)", err, rmErr)
			}
			errs = append(errs, fmt.Errorf("tenant %q: %w", tenant, err))
			continue
		}
		approved = append(approved, tenant)
	}
	return approved, errors.Join(errs...)
}

// withTenantDelegates returns kernel.yaml with a delegate added for each of
// tenant's tenant-owned service instances in cfg — the delegate generate's
// EnsureServiceDelegates writes for it — and nothing else changed. generate
// also reconciles every other delegate to the registry, which is not approval's
// business. A delegate kernel.yaml already holds for the tenant is kept as is.
func withTenantDelegates(kernelPath string, cfg *protoOperator.Config, tenant string) ([]byte, error) {
	kernelCfg, err := LoadKernelSourceConfig(kernelPath)
	if err != nil {
		return nil, err
	}
	RegisterAppServices(AppServiceNames(cfg))
	sd, err := loadServiceDeps()
	if err != nil {
		return nil, fmt.Errorf("loading service dependencies: %w", err)
	}
	instances, err := DeployedServiceInstances(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolving deployed service instances: %w", err)
	}
	existing := make(map[delegateKey]bool, len(kernelCfg.Delegates))
	for _, d := range kernelCfg.Delegates {
		existing[delegateKey{d.Name, d.TenantNamespace}] = true
	}
	for _, inst := range instances {
		if inst.Tenant != tenant || existing[delegateKey{inst.Service, inst.TenantNamespace()}] {
			continue
		}
		kernelCfg.Delegates = append(kernelCfg.Delegates, newServiceDelegate(cfg, inst, sd.authorizedServicesFor(inst.Service)))
	}
	data, err := yaml.Marshal(kernelCfg)
	if err != nil {
		return nil, fmt.Errorf("marshaling kernel config: %w", err)
	}
	return data, nil
}

// appendTenantToSettings adds tenant to operator.yaml's tenants list by
// editing the YAML node tree, not by re-marshaling the Config the way
// SaveSettings does: operator.yaml is a hand-maintained file whose comments
// record why each setting is what it is, and a round-trip through the proto
// would drop every one of them.
func appendTenantToSettings(cfgDir, tenant string) error {
	path := filepath.Join(cfgDir, OperatorSettingsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", OperatorSettingsFile, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", OperatorSettingsFile, err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s is not a YAML mapping", OperatorSettingsFile)
	}
	root := doc.Content[0]
	entry := &yaml.Node{Kind: yaml.ScalarNode, Value: tenant}
	var tenants *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "tenants" {
			tenants = root.Content[i+1]
		}
	}
	switch {
	case tenants == nil:
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "tenants"},
			&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{entry}})
	case tenants.Kind == yaml.SequenceNode:
		tenants.Content = append(tenants.Content, entry)
	default:
		return fmt.Errorf("%s: tenants is not a list", OperatorSettingsFile)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("marshaling %s: %w", OperatorSettingsFile, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("marshaling %s: %w", OperatorSettingsFile, err)
	}
	return os.WriteFile(path, buf.Bytes(), 0644)
}

// validateNewTenant checks that tenant can be added to cfg. The name becomes
// part of Kubernetes object names ("<svc>-<tenant>", and "<svc>-<tenant><i>"
// for the per-replica Deployments), so each of those must be a valid name too:
// one that is not would fail the whole chart's apply, not just this tenant.
func validateNewTenant(cfg *protoOperator.Config, tenant string) error {
	if errs := validation.IsDNS1123Label(tenant); len(errs) > 0 {
		return fmt.Errorf("tenant name %q: %s", tenant, strings.Join(errs, "; "))
	}
	if tenant == peopleTenant {
		return fmt.Errorf("tenant name %q is reserved by kernel", tenant)
	}
	for _, t := range cfg.GetTenants() {
		if t == tenant {
			return fmt.Errorf("%q is already a tenant", tenant)
		}
	}

	withTenant := proto.Clone(cfg).(*protoOperator.Config)
	withTenant.Tenants = append(withTenant.Tenants, tenant)
	instances, err := DeployedServiceInstances(withTenant)
	if err != nil {
		return fmt.Errorf("resolving deployed service instances: %w", err)
	}
	for _, inst := range instances {
		if inst.Tenant != tenant {
			continue
		}
		name := fmt.Sprintf("%s%d", inst.InstanceName(), withTenant.GetTopology().GetAppServices()[inst.Service]-1)
		if errs := validation.IsDNS1035Label(name); len(errs) > 0 {
			return fmt.Errorf("tenant name %q makes %s's object name %q invalid: %s", tenant, inst.Service, name, strings.Join(errs, "; "))
		}
	}
	return nil
}

// syncTenantNamespaceRules makes tenant's IdP → namespace rules in
// services/opa/tenants/namespace.rego the ones its oauth.yaml renders. The file
// is never regenerated: it carries hand-written rules (Google's per-person
// binding) that tenants/<name>/oauth.yaml cannot express. A rule of a rendered
// shape (exactNamespaceRule, issuerNamespaceRule, claimNamespaceRule) naming
// tenant's namespace that tenant no longer renders is removed — a matcher
// whose provider issuer changed leaves no stale rule behind — and every other
// rule stays byte-for-byte. Missing rules are appended. An issuer the file already
// mentions is refused rather than appended, because a second issuer-only rule
// naming a different namespace makes the policy fail at eval for every caller
// of that issuer — that edit needs a person.
//
// A claim matcher's issuer is a shared cluster IDP the file is expected to
// mention already, so its claim rule is refused only when the file holds an
// issuer-only rule for that issuer: both would match the tenant's callers.
//
// A rule the file already holds byte-for-byte is skipped, so syncing the
// same tenant's rules again changes nothing.
//
// A config dir without services/opa has nothing to sync: its policy is
// generated at deploy time from LoadTenantIdps, which already includes tenant.
func syncTenantNamespaceRules(cfgDir, tenant string, idps []TenantIdp) error {
	if _, err := os.Stat(OPAPolicyDir(cfgDir)); os.IsNotExist(err) {
		return nil
	}
	path := filepath.Join(OPAPolicyDir(cfgDir), "tenants", "namespace.rego")
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	ns := tenantNamespacePrefix + tenant
	rendered := map[string]bool{}
	for _, idp := range idps {
		if idp.TenantNamespace != ns {
			continue
		}
		if idp.ClaimKey != "" {
			rendered[claimNamespaceRule(idp)] = true
			continue
		}
		rendered[exactNamespaceRule(idp)] = true
		rendered[issuerNamespaceRule(idp)] = true
	}
	kept := renderedRulePattern(ns).ReplaceAllStringFunc(string(existing), func(rule string) string {
		if rendered[rule] {
			return rule
		}
		return ""
	})

	var rules strings.Builder
	issuers := map[string]bool{}
	for _, idp := range idps {
		if idp.TenantNamespace != ns {
			continue
		}
		if idp.ClaimKey != "" {
			rule := claimNamespaceRule(idp)
			if strings.Contains(kept, rule) {
				continue
			}
			if strings.Contains(kept, fmt.Sprintf("\tinput.claims.origin_iss == %q\n}", idp.IssuerURL)) {
				return fmt.Errorf("issuer %s has an issuer-only rule in %s; bind %s by hand", idp.IssuerURL, path, ns)
			}
			rules.WriteString(rule)
			continue
		}
		rule := exactNamespaceRule(idp)
		if strings.Contains(kept, rule) {
			continue
		}
		if strings.Contains(kept, fmt.Sprintf("%q", idp.IssuerURL)) {
			return fmt.Errorf("issuer %s already appears in %s; bind it to %s by hand", idp.IssuerURL, path, ns)
		}
		rules.WriteString(rule)
		issuers[idp.IssuerURL] = true
	}
	for _, idp := range idps {
		if idp.TenantNamespace != ns || !issuers[idp.IssuerURL] {
			continue
		}
		delete(issuers, idp.IssuerURL)
		rules.WriteString(issuerNamespaceRule(idp))
	}
	updated := kept + rules.String()
	if updated == string(existing) {
		return nil
	}
	if err := os.WriteFile(path, []byte(updated), 0); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// renderedRulePattern matches a namespace rule naming ns in any of the shapes
// exactNamespaceRule, issuerNamespaceRule and claimNamespaceRule render.
func renderedRulePattern(ns string) *regexp.Regexp {
	const quoted = `"(?:[^"\\\n]|\\.)*"`
	return regexp.MustCompile(`\nnamespace := ` + regexp.QuoteMeta(fmt.Sprintf("%q", ns)) + ` if \{\n(?:` +
		`\tinput\.claims\.origin_iss == ` + quoted + `\n(?:\taud_matches\(` + quoted + `\)\n)?` +
		`|\tinput\.claims\.provider_iss == ` + quoted + `\n\tinput\.claims\[` + quoted + `\] == ` + quoted + `\n` +
		`)\}\n`)
}

// syncTenantRoleRules makes tenant's generated role rules in
// services/opa/grants/static.rego the ones its oauth.yaml renders: a person an
// email_domain claim matcher routes into the tenant holds role "user". oauth
// mints email_domain only from an email the IdP verified, so the approved
// matcher is the whole grant. admin and owner are never granted here.
//
// The rules live in grants, not idp/claims.rego. roles is a complete rule in
// both packages, and claims.rego's hand-written per-person rules
// (roles := {"admin"}) match people a domain also matches; two complete rules
// with different values fail at eval. main.rego unions idp.roles with
// grants.roles, so such a person holds {"admin", "user"} and admin's eval
// wins (user's requires not roles["admin"]). Every rendered rule grants the
// same {"user"}, so two of them matching one caller cannot conflict either.
//
// The tenant-independent personRoleRule is appended too when the file lacks
// it, so a static.rego written before that rule existed gains it on the next
// approve-tenant or sync-tenant-rules.
//
// As with namespace.rego, the file is never regenerated: a rendered rule for
// tenant its oauth.yaml no longer renders is removed, every other rule stays
// byte-for-byte, missing rules are appended, and a second run changes nothing.
func syncTenantRoleRules(cfgDir, tenant string, idps []TenantIdp) error {
	if _, err := os.Stat(OPAPolicyDir(cfgDir)); os.IsNotExist(err) {
		return nil
	}
	path := filepath.Join(OPAPolicyDir(cfgDir), "grants", "static.rego")
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	ns := tenantNamespacePrefix + tenant
	var rendered []string
	for _, idp := range idps {
		if idp.TenantNamespace == ns && idp.ClaimKey == emailDomainClaimKey {
			rendered = append(rendered, userRoleRule(idp))
		}
	}
	kept := renderedRoleRulePattern(ns).ReplaceAllStringFunc(string(existing), func(rule string) string {
		if slices.Contains(rendered, rule) {
			return rule
		}
		return ""
	})
	updated := kept
	for _, rule := range rendered {
		if !strings.Contains(kept, rule) {
			updated += rule
		}
	}
	if !strings.Contains(updated, personRoleRule) {
		updated += personRoleRule
	}
	if updated == string(existing) {
		return nil
	}
	if err := os.WriteFile(path, []byte(updated), 0); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// userRoleRule is the role rule for an email_domain claim matcher. The comment
// names the tenant's namespace, so sync can tell the tenant's rules apart.
func userRoleRule(idp TenantIdp) string {
	return fmt.Sprintf("\n# Generated: a verified %s address routed into %s is a user.\nroles := {\"user\"} if {\n\tinput.claims.provider_iss == %q\n\tinput.claims[%q] == %q\n}\n",
		idp.ClaimValue, idp.TenantNamespace, idp.IssuerURL, idp.ClaimKey, idp.ClaimValue)
}

// renderedRoleRulePattern matches a role rule userRoleRule renders for ns.
func renderedRoleRulePattern(ns string) *regexp.Regexp {
	const quoted = `"(?:[^"\\\n]|\\.)*"`
	return regexp.MustCompile(`\n# Generated: a verified \S+ address routed into ` + regexp.QuoteMeta(ns) + ` is a user\.\n` +
		`roles := \{"user"\} if \{\n\tinput\.claims\.provider_iss == ` + quoted + `\n\tinput\.claims\["email_domain"\] == ` + quoted + `\n\}\n`)
}
