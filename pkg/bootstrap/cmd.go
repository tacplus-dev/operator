package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var rootCmd = &cobra.Command{
	Use:   "cavalier-operator",
	Short: "Cavalier cluster operator — deploy, manage, and operate a cavalier cluster",
	Long: `cavalier-operator is the cavalier cluster lifecycle manager.

When run without subcommands it starts in the mode declared by operator.yaml
(default: IN_CLUSTER — continuously reconciles the Kubernetes cluster state).

Modes:
  in-cluster      Run inside Kubernetes, reconcile Helm releases continuously.
                  gateway replicas are auto-set to the number of schedulable nodes.
  generate-only   Generate Helm charts to output_dir and exit. Does not talk to k8s.

Bootstrap workflow:
  cavalier-operator init ca                Initialize root CA → crypto.yaml
  cavalier-operator init cluster           Initialize cluster PKI + operator.yaml
  cavalier-operator init keycloak          Enable Keycloak IDP (dev/POC)
  cavalier-operator init-opa               Generate OPA entitlements policy
  cavalier-operator init config <name>     Apply named template from cavalier/config repo
  cavalier-operator export config <name>   Pack config dir back into a template file
  cavalier-operator build                  Build self-contained deployment bundle
  cavalier-operator bundle expand <file>   Unpack bundle to directory
  cavalier-operator bundle pack <dir>      Pack directory back to bundle
  cavalier-operator generate               Generate Helm charts to disk
  cavalier-operator apply                  Apply charts to Kubernetes (one-shot)
  cavalier-operator apply --bundle <file>  Apply from pre-built bundle

Management:
  cavalier-operator set-image-tag <tag>    Update container image tag
  cavalier-operator set-mode <mode>        Switch between in-cluster and generate-only
  cavalier-operator set-web-root <path>    Set gateway web root host path
  cavalier-operator unset-web-root         Remove web root (use embedded webapp)
  cavalier-operator add-api-key <name>     Create an API key credential pair
  cavalier-operator add-profile <id>       Create or update an entitlement profile
  cavalier-operator set-deploy-list <file> Set the deploy list filter
  cavalier-operator unset-deploy-list      Remove deploy list (deploy all)
  cavalier-operator merge <svc> <file>     Deep-merge YAML into a service config
  cavalier-operator gen-backend-tokens     Generate OPA backend token policy`,
	RunE: runOperator,
}

var configDir string

func init() {
	rootCmd.PersistentFlags().StringVarP(&configDir, "dir", "c", "", "config directory (default: ./bootstrap or $CAVALIER_OPERATOR_DIR)")

	rootCmd.AddCommand(initCmd)
	initCmd.AddCommand(initCACmd)
	initCmd.AddCommand(initClusterCmd)
	initCmd.AddCommand(initKeycloakCmd)
	initCmd.AddCommand(initConfigCmd)

	rootCmd.AddCommand(generateCmd)
	rootCmd.AddCommand(buildCmd)
	rootCmd.AddCommand(applyCmd)
	rootCmd.AddCommand(bundleCmd)
	bundleCmd.AddCommand(bundleExpandCmd)
	bundleCmd.AddCommand(bundlePackCmd)
	rootCmd.AddCommand(setVersionCmd)
	rootCmd.AddCommand(setModeCmd)
	rootCmd.AddCommand(initOPACmd)
	rootCmd.AddCommand(setWebRootCmd)
	rootCmd.AddCommand(unsetWebRootCmd)
	rootCmd.AddCommand(addApiKeyCmd)
	rootCmd.AddCommand(addProfileCmd)
	rootCmd.AddCommand(setDeployListCmd)
	rootCmd.AddCommand(unsetDeployListCmd)
	rootCmd.AddCommand(mergeCmd)
	rootCmd.AddCommand(genBackendTokensCmd)
	rootCmd.AddCommand(addCredsCmd)
	rootCmd.AddCommand(exportCmd)

	addProfileCmd.Flags().StringArrayVar(&profileGrants, "grant", nil, "per-service RPC grant as service=RPC1,RPC2 (repeatable)")
	addProfileCmd.Flags().BoolVar(&profileAll, "all", false, "grant _ANY on all well-known services")

	addApiKeyCmd.Flags().StringArrayVar(&apiKeyProfiles, "profile", []string{"admin"}, "entitlement profile to assign (repeatable)")
	addApiKeyCmd.Flags().StringArrayVar(&apiKeyClaims, "claim", nil, "custom claim as key=value (repeatable)")
}

// Execute runs the root cobra command.
func Execute() error {
	return rootCmd.Execute()
}

func resolveConfigDir() string {
	if configDir != "" {
		return configDir
	}
	return resolveConfigDirAuto()
}

// --- init-opa ---

var initOPACmd = &cobra.Command{
	Use:   "init-opa",
	Short: "Generate OPA entitlements policy from IDP config",
	Long: `Generates the OPA entitlements policy directory (services/opa/) by reading
the configured IDP providers from services/oauth.yaml.

Each provider's issuer URL becomes a trusted_issuer rule in the ABAC policy.

Edit services/opa/abac/attributes.rego after generation to customize
which users get which profiles.

Example:
  cavalier-operator init-opa`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return InitOPA(resolveConfigDir())
	},
}

// --- set-web-root ---

var setWebRootCmd = &cobra.Command{
	Use:   "set-web-root [path]",
	Short: "Set persistent web-root for gateway (read-only mount)",
	Long: `Sets a host path to mount into the gateway container as /webapp (read-only).
The setting persists across 'cavalier-operator generate' rebuilds.

Example:
  cavalier-operator set-web-root /Users/jody/webapp/dist`,
	Args: cobra.ExactArgs(1),
	RunE: runSetWebRoot,
}

func runSetWebRoot(cmd *cobra.Command, args []string) error {
	dir := resolveConfigDir()
	path := args[0]

	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("resolving absolute path: %w", err)
		}
		path = abs
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		return err
	}
	cfg.WebRoot = path
	if err := SaveSettings(dir, cfg); err != nil {
		return err
	}

	fmt.Printf("Web root set: %s (ro)\n", path)
	fmt.Printf("Run 'cavalier-operator generate' to rebuild.\n")
	return nil
}

// --- unset-web-root ---

var unsetWebRootCmd = &cobra.Command{
	Use:   "unset-web-root",
	Short: "Remove web-root setting (revert to embedded webapp)",
	Long:  `Removes the persistent web-root setting. The gateway will use its embedded webapp on next rebuild.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := resolveConfigDir()
		cfg, err := LoadConfig(dir)
		if err != nil {
			return err
		}
		cfg.WebRoot = ""
		if err := SaveSettings(dir, cfg); err != nil {
			return err
		}
		fmt.Printf("Web root removed. Gateway will use embedded webapp.\n")
		fmt.Printf("Run 'cavalier-operator generate' to rebuild.\n")
		return nil
	},
}

// --- add-api-key ---

var addApiKeyCmd = &cobra.Command{
	Use:   "add-api-key <name>",
	Short: "Create an API key credential pair",
	Long: `Generates an EC P-256 credential pair for machine-to-machine API key
authentication with the OAuth service.

Creates:
  creds/<name>/creds.yaml   public JWK (registered in oauth config)
  creds/<name>/key.yaml     private key (used by the machine client)

Also adds an api_key entry to services/oauth.yaml and registers a
kernel delegate so the key holder can exchange tokens.

Example:
  cavalier-operator add-api-key ci-bot --profile admin
  cavalier-operator add-api-key monitoring --profile reader`,
	Args: cobra.ExactArgs(1),
	RunE: runAddApiKey,
}

func runAddApiKey(cmd *cobra.Command, args []string) error {
	dir := resolveConfigDir()
	name := args[0]

	var claims map[string]string
	if len(apiKeyClaims) > 0 {
		claims = make(map[string]string, len(apiKeyClaims))
		for _, c := range apiKeyClaims {
			k, v, ok := strings.Cut(c, "=")
			if !ok {
				return fmt.Errorf("invalid --claim %q: expected key=value", c)
			}
			claims[k] = v
		}
	}

	if err := AddApiKey(dir, name, apiKeyProfiles, claims); err != nil {
		return err
	}

	fmt.Printf("API key created: %s\n", name)
	fmt.Printf("  creds/%s/creds.yaml  (public JWK)\n", name)
	fmt.Printf("  creds/%s/key.yaml    (private key — keep secret)\n", name)
	fmt.Printf("  profiles: %s\n", strings.Join(apiKeyProfiles, ", "))
	if len(claims) > 0 {
		fmt.Printf("  claims:\n")
		for k, v := range claims {
			fmt.Printf("    %s=%s\n", k, v)
		}
	}
	fmt.Printf("\nRun 'cavalier-operator generate' to rebuild.\n")
	return nil
}

// --- add-profile ---

var (
	profileGrants []string
	profileAll    bool
)

var addProfileCmd = &cobra.Command{
	Use:   "add-profile <id>",
	Short: "Create or update an entitlement profile",
	Long: `Creates or updates an entitlement profile in services/kernel.yaml.

Profiles map OPA policy output to typed per-service RPC entitlements.
Use --all for full access to all well-known services, or --grant for
fine-grained control.

Idempotent: re-running with the same ID replaces existing grants.

Examples:
  cavalier-operator add-profile admin --all
  cavalier-operator add-profile reader --grant sentinel=SENTINEL_SERVICE_RPCS_GET_DASHBOARD
  cavalier-operator add-profile custom --all --grant mcp=MCP_ASSIST_SERVICE_RPCS_CHAT`,
	Args: cobra.ExactArgs(1),
	RunE: runAddProfile,
}

func runAddProfile(cmd *cobra.Command, args []string) error {
	dir := resolveConfigDir()
	id := args[0]

	grants := make(map[string][]string)
	if profileAll {
		for svc, rpcs := range allServiceGrants() {
			grants[svc] = rpcs
		}
	}

	for _, g := range profileGrants {
		svc, rpcs, err := parseGrantFlag(g)
		if err != nil {
			return err
		}
		grants[svc] = rpcs
	}

	if len(grants) == 0 {
		return fmt.Errorf("no grants specified — use --all or --grant service=RPC1,RPC2")
	}

	if err := AddProfile(dir, id, grants); err != nil {
		return err
	}

	fmt.Printf("Profile %q added to services/kernel.yaml\n", id)
	for svc, rpcs := range grants {
		fmt.Printf("  %s: %s\n", svc, strings.Join(rpcs, ", "))
	}
	fmt.Printf("\nRun 'cavalier-operator generate' to rebuild.\n")
	return nil
}

// --- set-deploy-list ---

var setDeployListCmd = &cobra.Command{
	Use:   "set-deploy-list <path>",
	Short: "Set the deploy list (filter which services are deployed)",
	Long: `Sets the deploy list from a file containing one project path per line.
Only services whose name (last path segment) appears in the file will
be included in Helm output.

Example:
  cavalier-operator set-deploy-list /path/to/actually_deploy`,
	Args: cobra.ExactArgs(1),
	RunE: runSetDeployList,
}

func runSetDeployList(cmd *cobra.Command, args []string) error {
	dir := resolveConfigDir()
	srcPath := args[0]

	data, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", srcPath, err)
	}

	dstPath := filepath.Join(dir, "deploy.list")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	if err := os.WriteFile(dstPath, data, 0644); err != nil {
		return fmt.Errorf("writing deploy.list: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var names []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "/")
		names = append(names, parts[len(parts)-1])
	}

	fmt.Printf("Deploy list set: %d services\n", len(names))
	for _, n := range names {
		fmt.Printf("  %s\n", n)
	}
	fmt.Printf("\nRun 'cavalier-operator generate' to rebuild.\n")
	return nil
}

// --- unset-deploy-list ---

var unsetDeployListCmd = &cobra.Command{
	Use:   "unset-deploy-list",
	Short: "Remove deploy list (deploy all services from the topology)",
	Long:  `Removes the deploy list filter. All services with topology > 0 will be deployed.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := resolveConfigDir()
		path := filepath.Join(dir, "deploy.list")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing deploy.list: %w", err)
		}
		fmt.Printf("Deploy list removed. All topology services will be deployed.\n")
		fmt.Printf("Run 'cavalier-operator generate' to rebuild.\n")
		return nil
	},
}

// --- merge ---

var mergeCmd = &cobra.Command{
	Use:   "merge <service> <file>",
	Short: "Deep-merge a YAML file into a service config",
	Long: `Deep-merge a YAML file into services/<service>.yaml.

The source file's values are merged into the existing config using
deep merge semantics: maps are merged recursively, scalars and lists
from the source file override existing values.

Examples:
  cavalier-operator merge oauth static/oauth.yaml
  cavalier-operator merge kernel overrides/kernel.yaml`,
	Args: cobra.ExactArgs(2),
	RunE: runMerge,
}

func runMerge(cmd *cobra.Command, args []string) error {
	service := args[0]
	srcPath := args[1]

	dir := resolveConfigDir()
	dstPath := filepath.Join(dir, "services", service+".yaml")

	if _, err := os.Stat(dstPath); os.IsNotExist(err) {
		return fmt.Errorf("no services/%s.yaml found in %s — run 'cavalier-operator init cluster' first", service, dir)
	}

	srcData, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", srcPath, err)
	}
	var srcMap map[string]any
	if err := yaml.Unmarshal(srcData, &srcMap); err != nil {
		return fmt.Errorf("parsing %s: %w", srcPath, err)
	}

	dstData, err := os.ReadFile(dstPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", dstPath, err)
	}
	var dstMap map[string]any
	if err := yaml.Unmarshal(dstData, &dstMap); err != nil {
		return fmt.Errorf("parsing %s: %w", dstPath, err)
	}

	deepMerge(dstMap, srcMap)

	outData, err := yaml.Marshal(dstMap)
	if err != nil {
		return fmt.Errorf("marshaling merged config: %w", err)
	}
	if err := os.WriteFile(dstPath, outData, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", dstPath, err)
	}

	fmt.Printf("Merged %s into %s\n", srcPath, dstPath)
	return nil
}

// --- add-creds ---

var addCredsCmd = &cobra.Command{
	Use:   "add-creds <name>",
	Short: "Generate credential pair for a service and add kernel delegate",
	Long: `Generates an EC credential pair for a service and registers it as a
kernel delegate with authorized access to all services ("any").

Idempotent: skips if creds/<name>/ already exists.

Example:
  cavalier-operator add-creds letsencrypt`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := resolveConfigDir()
		name := args[0]
		credsDir := filepath.Join(dir, "creds")
		kernelPath := filepath.Join(dir, "services", "kernel.yaml")
		if err := os.MkdirAll(credsDir, 0755); err != nil {
			return fmt.Errorf("creating creds directory: %w", err)
		}
		if err := writeCredPairAndDelegate(credsDir, kernelPath, name, "any"); err != nil {
			return err
		}
		fmt.Printf("Run 'cavalier-operator generate' to rebuild.\n")
		return nil
	},
}

// deepMerge recursively merges src into dst. Maps are merged; all other types
// (scalars, lists) from src override dst.
func deepMerge(dst, src map[string]any) {
	for k, srcVal := range src {
		dstVal, exists := dst[k]
		if !exists {
			dst[k] = srcVal
			continue
		}

		srcMap, srcIsMap := srcVal.(map[string]any)
		dstMap, dstIsMap := dstVal.(map[string]any)
		if srcIsMap && dstIsMap {
			deepMerge(dstMap, srcMap)
		} else {
			dst[k] = srcVal
		}
	}
}
