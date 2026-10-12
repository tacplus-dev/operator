package bootstrap

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"

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
		if strings.Contains(string(existing), fmt.Sprintf("%q", idp.IssuerURL)) {
			return fmt.Errorf("issuer %s already appears in %s; bind it to %s by hand", idp.IssuerURL, path, ns)
		}
		rules.WriteString(exactNamespaceRule(idp))
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
