// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	protoOauth "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/oauth"
	protoOperatorV1 "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operatorv1"
)

// uniConfigFixture is a config dir as a deployment commits it plus the secrets
// its variables hold: the kmaker fixture's operator.yaml and crypto.yaml, a
// service config, an OPA policy file and a tenant's IdP config.
func uniConfigFixture(t *testing.T) string {
	t.Helper()
	dir, cfg := kmakerRetiredFixture(t)
	cfg.Tenants = []string{"alpha"}
	cfg.GitlabRegistryToken = "glpat-registry"
	cfg.LlmCredential = "sk-shared"
	cfg.LlmCredentialServices = []string{"llmproxy"}
	cfg.Topology.AppServices = map[string]int32{"llmproxy": 1}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "images.yaml"), "image_digests:\n  kernel: sha256:"+strings.Repeat("ab", 32)+"\n")
	writeTestFile(t, filepath.Join(dir, "services", "gateway.yaml"), "enable_http3: true\n")
	writeTestFile(t, filepath.Join(dir, "services", "llmproxy.yaml"), "tool_call_timeout_ms: 30000\n")
	writeTestFile(t, filepath.Join(dir, "services", "opa", "idp", "claims.rego"), "package idp\n\ndefault admin := false\n")
	writeTestFile(t, filepath.Join(dir, "tenants", "alpha", "oauth.yaml"), "claim_matchers:\n  - provider_issuer_url: https://accounts.google.com\n    claim_key: email_domain\n    claim_value: alpha.example\n")
	return dir
}

func mustUniConfigYAML(t *testing.T, uc *protoOperatorV1.UniConfig) []byte {
	t.Helper()
	data, err := protoOperatorV1.UniConfigToYAML(uc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A config dir assembled into a uni-config, serialized, parsed and expanded
// again is the same deployment: the same operator Config, the same service
// configs, policy files and tenant configs. The secrets travel under secrets,
// never in the committed part.
func TestUniConfigRoundTripsAConfigDir(t *testing.T) {
	dir := uniConfigFixture(t)
	uc, err := AssembleUniConfig(dir)
	if err != nil {
		t.Fatalf("AssembleUniConfig: %v", err)
	}
	if op := uc.GetOperator(); op.GetPki() != nil || op.GetLlmCredential() != "" || op.GetGitlabRegistryToken() != "" {
		t.Errorf("the committed part carries secret material: pki %v, llm_credential %q, gitlab_registry_token %q", op.GetPki() != nil, op.GetLlmCredential(), op.GetGitlabRegistryToken())
	}
	if s := uc.GetSecrets(); s.GetPki().GetRootCa() == nil || s.GetLlmCredential() != "sk-shared" || s.GetGitlabRegistryToken() != "glpat-registry" {
		t.Errorf("secrets = %v, want the PKI, the LLM credential and the registry token", s)
	}

	parsed, err := ParseUniConfig(mustUniConfigYAML(t, uc))
	if err != nil {
		t.Fatalf("ParseUniConfig of an assembled uni-config: %v", err)
	}
	out := filepath.Join(t.TempDir(), "work")
	if err := ExpandUniConfig(parsed, out); err != nil {
		t.Fatalf("ExpandUniConfig: %v", err)
	}

	want, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, want) {
		t.Errorf("expanded operator Config differs\n got: %v\nwant: %v", got, want)
	}
	for _, rel := range []string{
		"services/gateway.yaml",
		"services/kernel.yaml",
		"services/opa/idp/claims.rego",
	} {
		if g, w := string(readBytes(t, filepath.Join(out, rel))), string(readBytes(t, filepath.Join(dir, rel))); g != w {
			t.Errorf("%s = %q, want %q", rel, g, w)
		}
	}
	gotTenant, err := protoOauth.OAuthTenantConfigFromBytes(readBytes(t, filepath.Join(out, "tenants", "alpha", "oauth.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	wantTenant, err := protoOauth.OAuthTenantConfigFromBytes(readBytes(t, filepath.Join(dir, "tenants", "alpha", "oauth.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(gotTenant, wantTenant) {
		t.Errorf("tenants/alpha/oauth.yaml = %v, want %v", gotTenant, wantTenant)
	}
	for _, rel := range []string{OperatorCryptoFile, ServiceSecretsFile} {
		info, err := os.Stat(filepath.Join(out, rel))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is mode %v; secret material must be readable by its owner only", rel, info.Mode().Perm())
		}
	}
}

// The binary serialization parses like the YAML one.
func TestParseUniConfigAcceptsBinary(t *testing.T) {
	uc, err := AssembleUniConfig(uniConfigFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	data, err := proto.Marshal(uc)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseUniConfig(data)
	if err != nil {
		t.Fatalf("ParseUniConfig(binary): %v", err)
	}
	if !proto.Equal(parsed, uc) {
		t.Error("the binary uni-config parsed to a different message")
	}
}

// editUniConfig assembles the fixture, applies edit to its YAML document as a
// generic tree, and returns the edited document.
func editUniConfig(t *testing.T, edit func(doc map[string]any)) []byte {
	t.Helper()
	uc, err := AssembleUniConfig(uniConfigFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(mustUniConfigYAML(t, uc), &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func child(doc map[string]any, path ...string) map[string]any {
	for _, p := range path {
		next, ok := doc[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			doc[p] = next
		}
		doc = next
	}
	return doc
}

// An unknown or retired field is refused by its path, wherever it is: the
// pipeline's diff stage must catch a typo before apply, not have it vanish.
func TestParseUniConfigRefusesUnknownFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"top level", func(d map[string]any) { d["servcies"] = map[string]any{} }, "servcies"},
		{"operator", func(d map[string]any) { child(d, "operator")["fqdnn"] = "x" }, "operator.fqdnn"},
		{"retired field", func(d map[string]any) { child(d, "operator", "topology")["kmaker_nodes"] = 1 }, "operator.topology.kmaker_nodes"},
		{"service secrets", func(d map[string]any) { child(d, "secrets", "oauth")["apple_key"] = "x" }, "secrets.oauth.apple_key"},
		{"tenant", func(d map[string]any) { child(d, "tenants", "alpha")["claim_matcher"] = "x" }, "tenants.alpha.claim_matcher"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseUniConfig(editUniConfig(t, tc.edit))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseUniConfig error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// A missing required secret is refused by name.
func TestParseUniConfigNamesMissingSecrets(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"no secrets", func(d map[string]any) { delete(d, "secrets") }, "secrets"},
		{"no pki", func(d map[string]any) { delete(child(d, "secrets"), "pki") }, "secrets.pki"},
		{"no root CA", func(d map[string]any) {
			delete(child(d, "secrets", "pki"), "root_ca")
			delete(child(d, "secrets", "pki"), "rootCa")
		}, "secrets.pki.root_ca"},
		{"apple without its key", func(d map[string]any) {
			child(d, "services")["oauth"] = "providers:\n  - name: apple\n    enable: true\n    apple:\n      team_id: TEAM\n      key_id: KEY\n"
		}, "secrets.oauth.apple_signin_key_p8"},
		{"gmail without its key", func(d map[string]any) {
			child(d, "services")["mailbox"] = "gmail:\n  mailbox: crew@example.com\n"
		}, "secrets.mailbox.gmail_service_account_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseUniConfig(editUniConfig(t, tc.edit))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseUniConfig error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// Secret material in the committed part is refused: it belongs under secrets,
// where only the deployment's protected variables put it.
func TestParseUniConfigRefusesSecretsInTheCommittedPart(t *testing.T) {
	for field, value := range map[string]any{
		"gitlab_registry_token": "glpat-x",
		"llm_credential":        "sk-x",
		"pki":                   map[string]any{},
	} {
		t.Run(field, func(t *testing.T) {
			_, err := ParseUniConfig(editUniConfig(t, func(d map[string]any) { child(d, "operator")[field] = value }))
			if err == nil || !strings.Contains(err.Error(), "operator."+field) {
				t.Fatalf("ParseUniConfig error = %v, want it to name operator.%s", err, field)
			}
		})
	}
}

// validate checks each service's config against that service's Config, as
// generate does: an unknown key names the service and the key.
func TestValidateUniConfigNamesAnUnknownServiceKey(t *testing.T) {
	data := editUniConfig(t, func(d map[string]any) { child(d, "services")["gateway"] = "enable_htp3: true\n" })
	err := ValidateUniConfig(data)
	if err == nil || !strings.Contains(err.Error(), "gateway") || !strings.Contains(err.Error(), "enable_htp3") {
		t.Fatalf("ValidateUniConfig error = %v, want it to name gateway and enable_htp3", err)
	}
	if err := ValidateUniConfig(editUniConfig(t, func(map[string]any) {})); err != nil {
		t.Fatalf("ValidateUniConfig of the assembled fixture: %v", err)
	}
}

// The uni-config is expanded into a directory of its own: one that already
// holds a deployment's files could mix two of them.
func TestExpandUniConfigRefusesANonEmptyDir(t *testing.T) {
	uc, err := AssembleUniConfig(uniConfigFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "operator.yaml"), "fqdn: other.example\n")
	if err := ExpandUniConfig(uc, dir); err == nil {
		t.Fatal("ExpandUniConfig wrote into a directory that already holds a deployment")
	}
}

// The service secrets reach the chart: generating from an expanded uni-config
// renders oauth's Secret with the key the uni-config holds.
func TestGenerateFromUniConfigRendersServiceSecrets(t *testing.T) {
	data := editUniConfig(t, func(d map[string]any) { child(d, "secrets", "oauth")["apple_signin_key_p8"] = "apple-p8" })
	uc, err := ParseUniConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "work")
	if err := ExpandUniConfig(uc, dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	g, err := newGenerator(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	charts, err := g.Generate(cfg, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	secret := string(charts.Files["templates/oauth/secrets.yaml"])
	if want := base64.StdEncoding.EncodeToString([]byte("apple-p8")); !strings.Contains(secret, want) {
		t.Errorf("oauth's Secret does not carry the uni-config's key:\n%s", secret)
	}
}

// The operator's namespace comes from its service account's mount, not a
// POD_NAMESPACE variable: the operator's pod carries no configuration env.
func TestOperatorNamespaceComesFromTheServiceAccountMount(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "from-env")
	p := filepath.Join(t.TempDir(), "namespace")
	writeTestFile(t, p, "tac-seatrials\n")
	if got := operatorNamespace(p); got != "tac-seatrials" {
		t.Errorf("operatorNamespace = %q, want tac-seatrials", got)
	}
	if got := operatorNamespace(filepath.Join(t.TempDir(), "missing")); got != "schematic-operator" {
		t.Errorf("operatorNamespace without the mount = %q, want the default schematic-operator", got)
	}
}

// A changed uni-config Secret reaches the operator's mount, and the operator
// restarts to render from it: watchFile reports the change, and nothing else.
func TestWatchFileReportsAChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "uni-config")
	writeTestFile(t, p, "one")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := watchFile(ctx, p, 10*time.Millisecond)

	select {
	case <-changed:
		t.Fatal("watchFile reported a change before the file changed")
	case <-time.After(100 * time.Millisecond):
	}
	writeTestFile(t, p, "two")
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("watchFile did not report the change")
	}
}

// assemble takes the secrets from the job's environment: crypto.yaml's content
// from one variable, each named secrets field from another. An unset variable
// or a field Secrets does not declare is refused by name.
func TestAssembleSecretsFromEnv(t *testing.T) {
	dir := uniConfigFixture(t)
	crypto := string(readBytes(t, filepath.Join(dir, OperatorCryptoFile)))
	if err := os.Remove(filepath.Join(dir, OperatorCryptoFile)); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"FLEET_CRYPTO_YAML": crypto, "FLEET_APPLE_KEY_P8": "apple-p8"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	uc, err := AssembleUniConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := assembleSecretsFromEnv(uc, "FLEET_CRYPTO_YAML", []string{"oauth.apple_signin_key_p8=FLEET_APPLE_KEY_P8"}, lookup); err != nil {
		t.Fatalf("assembleSecretsFromEnv: %v", err)
	}
	if uc.GetSecrets().GetPki().GetRootCa() == nil || uc.GetSecrets().GetOauth().GetAppleSigninKeyP8() != "apple-p8" {
		t.Errorf("secrets = %v, want the PKI from FLEET_CRYPTO_YAML and the Apple key", uc.GetSecrets())
	}
	if _, err := ParseUniConfig(mustUniConfigYAML(t, uc)); err != nil {
		t.Errorf("the assembled uni-config does not parse: %v", err)
	}

	for _, tc := range []struct {
		crypto string
		fields []string
		want   string
	}{
		{"UNSET_CRYPTO", nil, "UNSET_CRYPTO"},
		{"", []string{"oauth.apple_signin_key_p8=UNSET"}, "UNSET"},
		{"", []string{"oauth.apple_key=FLEET_APPLE_KEY_P8"}, "secrets.oauth.apple_key"},
		{"", []string{"oauth.apple_signin_key_p8"}, "<field path>=<VARIABLE>"},
	} {
		err := assembleSecretsFromEnv(&protoOperatorV1.UniConfig{Operator: uc.GetOperator()}, tc.crypto, tc.fields, lookup)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("assembleSecretsFromEnv(%q, %v) error = %v, want it to name %q", tc.crypto, tc.fields, err, tc.want)
		}
	}
}

// --uni-config expands the document into the config directory every command
// then reads, and refuses a document that does not parse before any command
// runs.
func TestUniConfigFlagExpandsIntoTheConfigDir(t *testing.T) {
	prevDir, prevSource := configDir, uniConfigSource
	t.Cleanup(func() { configDir, uniConfigSource = prevDir, prevSource })

	uc, err := AssembleUniConfig(uniConfigFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "uni-config")
	writeTestFile(t, src, string(mustUniConfigYAML(t, uc)))
	configDir, uniConfigSource = filepath.Join(t.TempDir(), "work"), src
	if err := expandUniConfigFlag(rootCmd, nil); err != nil {
		t.Fatalf("expandUniConfigFlag: %v", err)
	}
	cfg, err := LoadConfig(resolveConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GetFqdn() != uc.GetOperator().GetFqdn() || cfg.GetPki().GetRootCa() == nil {
		t.Errorf("the config dir does not hold the uni-config's deployment: %v", cfg)
	}

	writeTestFile(t, src, "operator: {}\nservcies: {}\n")
	configDir = filepath.Join(t.TempDir(), "work")
	if err := expandUniConfigFlag(rootCmd, nil); err == nil || !strings.Contains(err.Error(), "servcies") {
		t.Errorf("expandUniConfigFlag of a bad document = %v, want it to name servcies", err)
	}
}
