// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
	protoOperatorV1 "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operatorv1"
)

// uniConfigSource is --uni-config: the uni-config the command runs from, a
// file or "-" for stdin. Set, it is the operator's only input: it is expanded
// into the config directory (--dir, which must be empty, or a private
// temporary one) before the command runs.
var uniConfigSource string

func init() {
	rootCmd.PersistentFlags().StringVar(&uniConfigSource, "uni-config", "", "run from this uni-config (a file, or - for stdin) instead of a config directory")
	rootCmd.PersistentPreRunE = expandUniConfigFlag

	rootCmd.AddCommand(validateCmd)
	rootCmd.AddCommand(uniConfigCmd)
	uniConfigCmd.AddCommand(uniConfigAssembleCmd)
	uniConfigAssembleCmd.Flags().StringVar(&assembleCryptoEnv, "crypto-from-env", "", "read crypto.yaml's content from this environment variable instead of the config dir")
	uniConfigAssembleCmd.Flags().StringArrayVar(&assembleSecretEnv, "secret-from-env", nil, "set a secrets field from an environment variable, as <field path>=<VARIABLE>, e.g. oauth.apple_signin_key_p8=FLEET_APPLE_KEY_P8 (repeatable)")
}

// expandUniConfigFlag expands --uni-config, when set, into the config
// directory every command reads.
func expandUniConfigFlag(cmd *cobra.Command, args []string) error {
	if uniConfigSource == "" {
		return nil
	}
	data, err := readUniConfigSource(uniConfigSource, cmd.InOrStdin())
	if err != nil {
		return fmt.Errorf("reading the uni-config: %w", err)
	}
	uc, err := ParseUniConfig(data)
	if err != nil {
		return err
	}
	dir := configDir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "uni-config-")
		if err != nil {
			return err
		}
		dir = tmp
	}
	if err := ExpandUniConfig(uc, dir); err != nil {
		return err
	}
	configDir = dir
	return nil
}

var validateCmd = &cobra.Command{
	Use:   "validate <uni-config|->",
	Short: "Validate a uni-config before it is applied",
	Long: `Validate a uni-config: the document the deployment's pipeline assembles and
applies as the uni-config Secret. Run by the pipeline's diff stage before apply.

It refuses an unknown or retired field (naming its path), a missing required
secret (naming it), secret material outside secrets, and a deployment that does
not build: each service's config is checked against that service's Config.
Nothing is printed of the document itself.

Examples:
  schematic-operator validate uni-config.yaml
  assemble-uni-config | schematic-operator validate -`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		data, err := readUniConfigSource(args[0], cmd.InOrStdin())
		if err != nil {
			return fmt.Errorf("reading the uni-config: %w", err)
		}
		if err := ValidateUniConfig(data); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "uni-config: valid")
		return nil
	},
}

var uniConfigCmd = &cobra.Command{
	Use:   "uni-config",
	Short: "Work with the uni-config, the one document that defines a deployment",
}

var (
	assembleCryptoEnv string
	assembleSecretEnv []string
)

var uniConfigAssembleCmd = &cobra.Command{
	Use:   "assemble",
	Short: "Assemble a uni-config from a config dir and the job's secret variables",
	Long: `Assemble the uni-config from the config directory (operator.yaml, images.yaml,
services/, tenants/ and, if present, crypto.yaml) and write it to stdout as
YAML. The secrets can come from the job's environment instead of files, so a
pipeline assembles the document in memory and pipes it on:

  schematic-operator uni-config assemble \
      --crypto-from-env FLEET_CRYPTO_YAML \
      --secret-from-env oauth.apple_signin_key_p8=FLEET_APPLE_KEY_P8 |
    schematic-operator validate -

The output holds every secret of the deployment: pipe it, never save or print it.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		uc, err := AssembleUniConfig(resolveConfigDir())
		if err != nil {
			return err
		}
		if err := assembleSecretsFromEnv(uc, assembleCryptoEnv, assembleSecretEnv, os.LookupEnv); err != nil {
			return err
		}
		data, err := protoOperatorV1.UniConfigToYAML(uc)
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(data)
		return err
	},
}

// assembleSecretsFromEnv sets uc's secrets from the named environment
// variables: crypto.yaml's content from cryptoEnv, and each
// "<field path>=<VARIABLE>" of fields into secrets. An unset variable, or a
// field path Secrets does not declare, is refused by name.
func assembleSecretsFromEnv(uc *protoOperatorV1.UniConfig, cryptoEnv string, fields []string, lookup func(string) (string, bool)) error {
	if uc.Secrets == nil {
		uc.Secrets = &protoOperatorV1.Secrets{}
	}
	if cryptoEnv != "" {
		text, ok := lookup(cryptoEnv)
		if !ok {
			return fmt.Errorf("--crypto-from-env: %s is not set", cryptoEnv)
		}
		crypto, err := protoOperator.ConfigFromBytes([]byte(text))
		if err != nil {
			return fmt.Errorf("--crypto-from-env: %s is not a crypto.yaml: %w", cryptoEnv, err)
		}
		if crypto.GetPki() != nil {
			uc.Secrets.Pki = crypto.GetPki()
		}
		if crypto.GetLlmCredential() != "" {
			uc.Secrets.LlmCredential = crypto.GetLlmCredential()
		}
		if crypto.GetGitlabRegistryToken() != "" {
			uc.Secrets.GitlabRegistryToken = crypto.GetGitlabRegistryToken()
		}
		if len(crypto.GetLlmCredentialServices()) > 0 {
			uc.Operator.LlmCredentialServices = dedupe(append(uc.Operator.LlmCredentialServices, crypto.GetLlmCredentialServices()...))
		}
	}
	if len(fields) == 0 {
		return nil
	}
	tree := map[string]any{}
	for _, f := range fields {
		fieldPath, variable, ok := strings.Cut(f, "=")
		if !ok || fieldPath == "" || variable == "" {
			return fmt.Errorf("--secret-from-env %q: want <field path>=<VARIABLE>", f)
		}
		value, set := lookup(variable)
		if !set {
			return fmt.Errorf("--secret-from-env %s: %s is not set", fieldPath, variable)
		}
		node := tree
		parts := strings.Split(fieldPath, ".")
		for _, p := range parts[:len(parts)-1] {
			next, ok := node[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				node[p] = next
			}
			node = next
		}
		node[parts[len(parts)-1]] = value
	}
	md := (&protoOperatorV1.Secrets{}).ProtoReflect().Descriptor()
	if unknown := unknownFields(md, tree, "secrets"); len(unknown) > 0 {
		return fmt.Errorf("--secret-from-env: unknown field(s): %s", strings.Join(unknown, ", "))
	}
	data, err := yaml.Marshal(tree)
	if err != nil {
		return err
	}
	set, err := protoOperatorV1.SecretsFromBytes(data)
	if err != nil {
		return fmt.Errorf("--secret-from-env: %w", err)
	}
	proto.Merge(uc.Secrets, set)
	return nil
}
