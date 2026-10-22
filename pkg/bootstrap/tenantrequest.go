package bootstrap

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		err = appendTenantNamespaceRules(cfgDir, tenant, idps)
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
	Short: "Append an existing tenant's oauth.yaml routing to namespace.rego",
	Long: `Brings services/opa/tenants/namespace.rego up to date with
tenants/<name>/oauth.yaml for a tenant operator.yaml already lists: each of
its providers and claim_matchers renders the rule approve-tenant would append,
and only the rules the file does not already hold are appended. Hand-written
rules are left as they are, and running it again changes nothing.

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
		fmt.Printf("Namespace rules for tenant %s are up to date.\n", args[0])
		fmt.Printf("Run 'schematic-operator generate' to rebuild.\n")
		return nil
	},
}

// SyncTenantRules appends the namespace rules tenants/<tenant>/oauth.yaml
// declares that services/opa/tenants/namespace.rego does not hold yet. tenant
// must already be listed in operator.yaml; new tenants go through
// ApproveTenant.
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
	return appendTenantNamespaceRules(cfgDir, tenant, idps)
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

// appendTenantNamespaceRules adds tenant's IdP → namespace rules to
// services/opa/tenants/namespace.rego. The file is appended to, never
// regenerated: it carries hand-written rules (Google's per-person binding) that
// tenants/<name>/oauth.yaml cannot express. An issuer the file already
// mentions is refused rather than appended, because a second issuer-only rule
// naming a different namespace makes the policy fail at eval for every caller
// of that issuer — that edit needs a person.
//
// A claim matcher's issuer is a shared cluster IDP the file is expected to
// mention already, so its claim rule is refused only when the file holds an
// issuer-only rule for that issuer: both would match the tenant's callers.
//
// A rule the file already holds byte-for-byte is skipped, so appending the
// same tenant's rules again changes nothing.
//
// A config dir without services/opa has nothing to append to: its policy is
// generated at deploy time from LoadTenantIdps, which already includes tenant.
func appendTenantNamespaceRules(cfgDir, tenant string, idps []TenantIdp) error {
	if _, err := os.Stat(OPAPolicyDir(cfgDir)); os.IsNotExist(err) {
		return nil
	}
	path := filepath.Join(OPAPolicyDir(cfgDir), "tenants", "namespace.rego")
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	ns := tenantNamespacePrefix + tenant
	var rules strings.Builder
	issuers := map[string]bool{}
	for _, idp := range idps {
		if idp.TenantNamespace != ns {
			continue
		}
		if idp.ClaimKey != "" {
			rule := claimNamespaceRule(idp)
			if strings.Contains(string(existing), rule) {
				continue
			}
			if strings.Contains(string(existing), fmt.Sprintf("\tinput.claims.origin_iss == %q\n}", idp.IssuerURL)) {
				return fmt.Errorf("issuer %s has an issuer-only rule in %s; bind %s by hand", idp.IssuerURL, path, ns)
			}
			rules.WriteString(rule)
			continue
		}
		rule := exactNamespaceRule(idp)
		if strings.Contains(string(existing), rule) {
			continue
		}
		if strings.Contains(string(existing), fmt.Sprintf("%q", idp.IssuerURL)) {
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
	if rules.Len() == 0 {
		return nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	if _, err := f.WriteString(rules.String()); err != nil {
		f.Close()
		return fmt.Errorf("appending to %s: %w", path, err)
	}
	return f.Close()
}
