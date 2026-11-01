// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"gopkg.in/yaml.v3"

	protoMailbox "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/mailbox"
	protoOauth "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/oauth"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
	protoOperatorV1 "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operatorv1"
	"gitlab.com/seabeam/schematic/platform/services/operator/pkg/helm"
)

// The uni-config (#17) is the one document that defines a deployment:
// operatorv1.UniConfig (proto/services/uniconfig.proto). The deployment's
// pipeline assembles it in memory and applies it as one namespaced Secret; the
// operator reads only that.
//
// The rest of the operator works from a config directory (operator.yaml,
// crypto.yaml, services/, tenants/), and writes into it as it generates
// (configs/, the kernel delegates), so the operator expands the uni-config into
// a directory of its own, private to the process, and works from there. That
// directory is the operator's working state, not a second source: it is
// rebuilt from the uni-config at every start.
const (
	// UniConfigSecretName is the namespaced Secret that carries the uni-config.
	UniConfigSecretName = "uni-config"
	// UniConfigSecretKey is that Secret's single key: the serialized UniConfig.
	UniConfigSecretKey = "uni-config"
	// ServiceSecretsFile holds, in an expanded uni-config, the services' own
	// Secrets messages (secrets.oauth, secrets.mailbox): the values the chart
	// renders into each service's Secret.
	ServiceSecretsFile = "service-secrets.yaml"
)

// serviceNameRe is what a services or tenants key may be: it names a file.
var serviceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ParseUniConfig parses a serialized uni-config (YAML, JSON or binary proto)
// and refuses one that is not a whole, well-formed deployment: an unknown or
// retired field (by its path), a missing required secret (by its name), and
// secret material in the committed part.
func ParseUniConfig(data []byte) (*protoOperatorV1.UniConfig, error) {
	uc, err := decodeUniConfig(data)
	if err != nil {
		return nil, err
	}
	if err := checkUniConfig(uc); err != nil {
		return nil, err
	}
	if err := protoOperatorV1.ValidateUniConfig(uc); err != nil {
		return nil, fmt.Errorf("uni-config: %w", err)
	}
	return uc, nil
}

// decodeUniConfig parses data strictly: the generated parser drops what it does
// not know, so the field names are checked against the message first.
func decodeUniConfig(data []byte) (*protoOperatorV1.UniConfig, error) {
	md := (&protoOperatorV1.UniConfig{}).ProtoReflect().Descriptor()
	var tree any
	if err := yaml.Unmarshal(data, &tree); err == nil {
		if doc, ok := tree.(map[string]any); ok {
			if unknown := unknownFields(md, doc, ""); len(unknown) > 0 {
				return nil, fmt.Errorf("uni-config: unknown field(s): %s", strings.Join(unknown, ", "))
			}
			uc, err := protoOperatorV1.UniConfigFromBytes(data)
			if err != nil {
				return nil, fmt.Errorf("uni-config: %w", err)
			}
			return uc, nil
		}
	}
	uc := &protoOperatorV1.UniConfig{}
	if err := proto.Unmarshal(data, uc); err != nil {
		return nil, fmt.Errorf("uni-config: neither YAML, JSON nor a binary UniConfig: %w", err)
	}
	if unknown := unknownBinaryFields(uc.ProtoReflect(), ""); len(unknown) > 0 {
		return nil, fmt.Errorf("uni-config: unknown field(s): %s", strings.Join(unknown, ", "))
	}
	return uc, nil
}

// unknownFields returns the path of every key in node that md does not
// declare, by its proto or its JSON name. A retired (reserved) name is unknown
// too, and says so.
func unknownFields(md protoreflect.MessageDescriptor, node any, at string) []string {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for _, k := range sortedKeys(m) {
		p := joinFieldPath(at, k)
		fd := md.Fields().ByName(protoreflect.Name(k))
		if fd == nil {
			fd = md.Fields().ByJSONName(k)
		}
		if fd == nil {
			if md.ReservedNames().Has(protoreflect.Name(k)) {
				p += " (retired)"
			}
			out = append(out, p)
			continue
		}
		v := m[k]
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() == nil {
				continue
			}
			if entries, ok := v.(map[string]any); ok {
				for _, ek := range sortedKeys(entries) {
					out = append(out, unknownFields(fd.MapValue().Message(), entries[ek], p+"."+ek)...)
				}
			}
		case fd.Message() == nil:
		case fd.IsList():
			if items, ok := v.([]any); ok {
				for i, item := range items {
					out = append(out, unknownFields(fd.Message(), item, fmt.Sprintf("%s[%d]", p, i))...)
				}
			}
		default:
			out = append(out, unknownFields(fd.Message(), v, p)...)
		}
	}
	return out
}

// unknownBinaryFields is unknownFields for a binary message: the field numbers
// it carried that its descriptor does not declare.
func unknownBinaryFields(m protoreflect.Message, at string) []string {
	var out []string
	if len(m.GetUnknown()) > 0 {
		out = append(out, joinFieldPath(at, "<unknown field number>"))
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		p := joinFieldPath(at, string(fd.Name()))
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
					out = append(out, unknownBinaryFields(mv.Message(), p+"."+k.String())...)
					return true
				})
			}
		case fd.Message() == nil:
		case fd.IsList():
			for i := 0; i < v.List().Len(); i++ {
				out = append(out, unknownBinaryFields(v.List().Get(i).Message(), fmt.Sprintf("%s[%d]", p, i))...)
			}
		default:
			out = append(out, unknownBinaryFields(v.Message(), p)...)
		}
		return true
	})
	return out
}

func joinFieldPath(at, name string) string {
	if at == "" {
		return name
	}
	return at + "." + name
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// checkUniConfig refuses a uni-config with a required part missing, secret
// material outside secrets, a secret a configured feature needs left empty, or
// a services or tenants key that cannot name a file.
func checkUniConfig(uc *protoOperatorV1.UniConfig) error {
	op := uc.GetOperator()
	if op == nil {
		return fmt.Errorf("uni-config: operator is required")
	}
	// The committed part carries no secret: each of these is a protected
	// variable's, under secrets.
	if op.GetPki() != nil {
		return fmt.Errorf("uni-config: operator.pki is secret material: it belongs under secrets.pki")
	}
	if op.GetLlmCredential() != "" {
		return fmt.Errorf("uni-config: operator.llm_credential is secret material: it belongs under secrets.llm_credential")
	}
	if op.GetGitlabRegistryToken() != "" {
		return fmt.Errorf("uni-config: operator.gitlab_registry_token is secret material: it belongs under secrets.gitlab_registry_token")
	}

	s := uc.GetSecrets()
	if s == nil {
		return fmt.Errorf("uni-config: secrets is required")
	}
	if s.GetPki() == nil {
		return fmt.Errorf("uni-config: secrets.pki is required")
	}
	if len(s.GetPki().GetRootCa().GetChain()) == 0 {
		return fmt.Errorf("uni-config: secrets.pki.root_ca is required")
	}

	for _, name := range sortedKeys(uc.GetServices()) {
		if !serviceNameRe.MatchString(name) {
			return fmt.Errorf("uni-config: services key %q is not a service name", name)
		}
	}
	for _, name := range sortedKeys(uc.GetTenants()) {
		if !serviceNameRe.MatchString(name) {
			return fmt.Errorf("uni-config: tenants key %q is not a tenant name", name)
		}
	}
	for _, p := range sortedKeys(uc.GetOpaPolicies()) {
		if _, err := opaPolicyPath(p); err != nil {
			return err
		}
	}

	// A secret is required when the config that uses it is set.
	if text, ok := uc.GetServices()["oauth"]; ok {
		if oc, err := protoOauth.ConfigFromBytes([]byte(text)); err == nil {
			for _, p := range oc.GetProviders() {
				if p.GetApple() != nil && s.GetOauth().GetAppleSigninKeyP8() == "" {
					return fmt.Errorf("uni-config: secrets.oauth.apple_signin_key_p8 is required: oauth provider %q is a Sign in with Apple provider", p.GetName())
				}
			}
		}
	}
	if text, ok := uc.GetServices()["mailbox"]; ok {
		if mc, err := protoMailbox.ConfigFromBytes([]byte(text)); err == nil && mc.GetGmail() != nil && s.GetMailbox().GetGmailServiceAccountJson() == "" {
			return fmt.Errorf("uni-config: secrets.mailbox.gmail_service_account_json is required: mailbox's gmail is set")
		}
	}
	return nil
}

// opaPolicyPath returns where an opa_policies key goes under services/opa/,
// refusing one that would leave that directory.
func opaPolicyPath(key string) (string, error) {
	clean := path.Clean(key)
	if key == "" || path.IsAbs(key) || clean != key || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("uni-config: opa_policies key %q is not a relative path under services/opa/", key)
	}
	return filepath.FromSlash(clean), nil
}

// AssembleUniConfig reads a config directory, as a deployment commits it plus
// its crypto.yaml and service-secrets.yaml, into a uni-config. deploy.list, a
// local development filter, is not part of it.
func AssembleUniConfig(dir string) (*protoOperatorV1.UniConfig, error) {
	cfg, err := LoadConfig(dir)
	if err != nil {
		return nil, err
	}
	op := proto.Clone(cfg).(*protoOperator.Config)
	op.Pki = nil
	op.LlmCredential = ""
	op.GitlabRegistryToken = ""

	secrets := &protoOperatorV1.Secrets{
		Pki:                 cfg.GetPki(),
		LlmCredential:       cfg.GetLlmCredential(),
		GitlabRegistryToken: cfg.GetGitlabRegistryToken(),
	}
	serviceSecrets, err := LoadServiceSecrets(dir)
	if err != nil {
		return nil, err
	}
	secrets.Oauth = serviceSecrets.GetOauth()
	secrets.Mailbox = serviceSecrets.GetMailbox()

	uc := &protoOperatorV1.UniConfig{
		Operator:    op,
		Services:    map[string]string{},
		Tenants:     map[string]*protoOauth.OAuthTenantConfig{},
		OpaPolicies: map[string]string{},
		Secrets:     secrets,
	}

	servicesDir := filepath.Join(dir, "services")
	entries, err := os.ReadDir(servicesDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading services/: %w", err)
	}
	for _, e := range entries {
		name, isYAML := strings.CutSuffix(e.Name(), ".yaml")
		if e.IsDir() || !isYAML {
			continue
		}
		data, err := os.ReadFile(filepath.Join(servicesDir, e.Name()))
		if err != nil {
			return nil, err
		}
		uc.Services[name] = string(data)
	}

	opaDir := OPAPolicyDir(dir)
	err = filepath.WalkDir(opaDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// macOS metadata, never policy: opa hands every file under its
		// directory to the rego parser, and an AppleDouble ._main.rego took
		// opa0 down on 2026-09-04.
		if strings.HasPrefix(d.Name(), "._") || d.Name() == ".DS_Store" {
			return nil
		}
		rel, err := filepath.Rel(opaDir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		uc.OpaPolicies[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading services/opa/: %w", err)
	}

	tenantDirs, err := os.ReadDir(filepath.Join(dir, "tenants"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading tenants/: %w", err)
	}
	tenantMD := (&protoOauth.OAuthTenantConfig{}).ProtoReflect().Descriptor()
	for _, e := range tenantDirs {
		if !e.IsDir() {
			continue
		}
		rel := filepath.Join("tenants", e.Name(), "oauth.yaml")
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var tree any
		if err := yaml.Unmarshal(data, &tree); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", rel, err)
		}
		if unknown := unknownFields(tenantMD, tree, ""); len(unknown) > 0 {
			return nil, fmt.Errorf("%s: unknown field(s): %s", rel, strings.Join(unknown, ", "))
		}
		tc, err := protoOauth.OAuthTenantConfigFromBytes(data)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", rel, err)
		}
		uc.Tenants[e.Name()] = tc
	}
	return uc, nil
}

// ExpandUniConfig writes a parsed uni-config into dir as the config directory
// the rest of the operator reads. dir must not exist yet or be empty: two
// deployments' files must never mix. The secret files are readable by their
// owner only.
func ExpandUniConfig(uc *protoOperatorV1.UniConfig, dir string) error {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("expanding the uni-config: %s is not empty", dir)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("expanding the uni-config: %w", err)
	}

	op := proto.Clone(uc.GetOperator()).(*protoOperator.Config)
	s := uc.GetSecrets()
	if err := SaveSettings(dir, op); err != nil {
		return err
	}
	crypto := &protoOperator.Config{
		Pki:                   s.GetPki(),
		LlmCredential:         s.GetLlmCredential(),
		LlmCredentialServices: op.GetLlmCredentialServices(),
		GitlabRegistryToken:   s.GetGitlabRegistryToken(),
	}
	if err := writeConfigYAML(filepath.Join(dir, OperatorCryptoFile), crypto, 0o600); err != nil {
		return err
	}
	if len(op.GetImageDigests()) > 0 {
		if err := writeConfigYAML(filepath.Join(dir, OperatorImagesFile), &protoOperator.Config{ImageDigests: op.GetImageDigests()}, 0o644); err != nil {
			return err
		}
	}

	serviceSecrets := &protoOperatorV1.Secrets{Oauth: s.GetOauth(), Mailbox: s.GetMailbox()}
	data, err := protoOperatorV1.SecretsToYAML(serviceSecrets)
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", ServiceSecretsFile, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ServiceSecretsFile), data, 0o600); err != nil {
		return err
	}

	for name, text := range uc.GetServices() {
		if !serviceNameRe.MatchString(name) {
			return fmt.Errorf("uni-config: services key %q is not a service name", name)
		}
		if err := writeFileAll(filepath.Join(dir, "services", name+".yaml"), []byte(text)); err != nil {
			return err
		}
	}
	for key, text := range uc.GetOpaPolicies() {
		rel, err := opaPolicyPath(key)
		if err != nil {
			return err
		}
		if err := writeFileAll(filepath.Join(OPAPolicyDir(dir), rel), []byte(text)); err != nil {
			return err
		}
	}
	for name, tc := range uc.GetTenants() {
		if !serviceNameRe.MatchString(name) {
			return fmt.Errorf("uni-config: tenants key %q is not a tenant name", name)
		}
		data, err := protoOauth.OAuthTenantConfigToYAML(tc)
		if err != nil {
			return fmt.Errorf("marshaling tenants/%s/oauth.yaml: %w", name, err)
		}
		if err := writeFileAll(filepath.Join(TenantConfigDir(dir, name), "oauth.yaml"), data); err != nil {
			return err
		}
	}
	return nil
}

func writeConfigYAML(p string, cfg *protoOperator.Config, mode os.FileMode) error {
	data, err := protoOperator.ConfigToYAML(cfg)
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", filepath.Base(p), err)
	}
	return os.WriteFile(p, data, mode)
}

func writeFileAll(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// LoadServiceSecrets reads the services' own Secrets messages from an expanded
// uni-config. A directory without the file (one not expanded from a
// uni-config) has none, and their Secrets render with empty keys.
func LoadServiceSecrets(dir string) (*protoOperatorV1.Secrets, error) {
	data, err := os.ReadFile(filepath.Join(dir, ServiceSecretsFile))
	if os.IsNotExist(err) {
		return &protoOperatorV1.Secrets{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", ServiceSecretsFile, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 || strings.TrimSpace(string(data)) == "{}" {
		return &protoOperatorV1.Secrets{}, nil
	}
	s, err := protoOperatorV1.SecretsFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", ServiceSecretsFile, err)
	}
	return &protoOperatorV1.Secrets{Oauth: s.GetOauth(), Mailbox: s.GetMailbox()}, nil
}

// ValidateUniConfig is what the pipeline's diff stage runs before apply: the
// document parses (ParseUniConfig), and the deployment it defines builds the
// way generate builds it, each service's config checked against that
// service's Config.
func ValidateUniConfig(data []byte) error {
	uc, err := ParseUniConfig(data)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "uni-config-validate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, "config")
	if err := ExpandUniConfig(uc, dir); err != nil {
		return err
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		return err
	}
	if err := ValidateConfig(cfg); err != nil {
		return fmt.Errorf("uni-config: operator: %w", err)
	}
	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		return fmt.Errorf("uni-config: %w", err)
	}
	if _, err := BuildDeployConfigs(dir); err != nil {
		return fmt.Errorf("uni-config: %w", err)
	}
	return nil
}

// readUniConfigSource reads a uni-config from a file, or from stdin for "-",
// so the pipeline can hand it over without writing it anywhere.
func readUniConfigSource(src string, stdin io.Reader) ([]byte, error) {
	if src == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(src)
}

// newGenerator returns the chart generator for a config directory: the
// registry's tenant-owned and pinned services, and the service secrets an
// expanded uni-config holds. An empty dir (a bundle) gives no service secrets.
func newGenerator(dir string, resolver helm.TagResolver) (*helm.Generator, error) {
	tenantOwned, err := TenantOwnedServices()
	if err != nil {
		return nil, fmt.Errorf("reading tenant-owned services from registry: %w", err)
	}
	pinnedTenant, err := PinnedTenantServices()
	if err != nil {
		return nil, fmt.Errorf("reading pinned-tenant services from registry: %w", err)
	}
	g := helm.New(CurrentSeries(), CurrentMajorMinor(), resolver, tenantOwned, pinnedTenant)
	if dir == "" {
		return g, nil
	}
	secrets, err := LoadServiceSecrets(dir)
	if err != nil {
		return nil, err
	}
	g.SetSecrets(secrets)
	return g, nil
}
