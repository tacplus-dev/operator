package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// ConfigTemplate is the single-file format `export config` writes and
// `init config` applies.
// Each named template (e.g. dev1.yaml) captures operator settings and OPA
// policy source in one file that the operator can expand into a config dir
// or pack from a config dir.
type ConfigTemplate struct {
	// Operator holds operator.yaml settings (no secrets, no FQDN, no PKI).
	// Merged into the config dir's operator.yaml using defaults semantics:
	// template values fill in missing keys; existing cluster settings win.
	Operator map[string]any `yaml:"operator,omitempty"`

	// OPA maps rego file paths (relative to services/opa/) to rego source.
	// Files are written/overwritten in services/opa/ when expanded.
	OPA map[string]string `yaml:"opa,omitempty"`
}

// --- init config ---

var initConfigCmd = &cobra.Command{
	Use:   "config <file>",
	Short: "Apply a config template file to the config dir",
	Long: `Apply a configuration template file, as 'export config' writes one.

The template is a single YAML file containing:
  operator:  settings merged into operator.yaml (template fills missing keys)
  opa:       rego files written to services/opa/ (overwrites generated files)

Run after 'schematic-operator init-opa' to apply the custom OPA policy.

Examples:
  schematic-operator init config dev1.yaml`,
	Args: cobra.ExactArgs(1),
	RunE: runInitConfig,
}

func runInitConfig(cmd *cobra.Command, args []string) error {
	path := args[0]
	dir := resolveConfigDir()

	tmpl, err := readConfigTemplate(path)
	if err != nil {
		return err
	}

	fmt.Printf("Config template: %s\n", path)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	return applyConfigTemplate(tmpl, dir)
}

// readConfigTemplate reads and parses the template file at path.
func readConfigTemplate(path string) (*ConfigTemplate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config template: %w", err)
	}

	var tmpl ConfigTemplate
	if err := yaml.Unmarshal(data, &tmpl); err != nil {
		return nil, fmt.Errorf("parsing config template %s: %w", path, err)
	}
	return &tmpl, nil
}

// applyConfigTemplate applies a ConfigTemplate to a config directory:
//   - operator.yaml: deep-merged (template fills missing keys; existing wins)
//   - services/opa/*.rego: written/overwritten
func applyConfigTemplate(tmpl *ConfigTemplate, dir string) error {
	if len(tmpl.Operator) > 0 {
		dest := filepath.Join(dir, "operator.yaml")
		dstMap := map[string]any{}
		if data, err := os.ReadFile(dest); err == nil {
			if err := yaml.Unmarshal(data, &dstMap); err != nil {
				return fmt.Errorf("parsing existing operator.yaml: %w", err)
			}
			if dstMap == nil {
				dstMap = map[string]any{}
			}
		}
		mergeDefaults(dstMap, tmpl.Operator)
		out, err := yaml.Marshal(dstMap)
		if err != nil {
			return fmt.Errorf("marshaling operator.yaml: %w", err)
		}
		if err := os.WriteFile(dest, out, 0644); err != nil {
			return fmt.Errorf("writing operator.yaml: %w", err)
		}
		fmt.Printf("  operator.yaml  merged\n")
	}

	if len(tmpl.OPA) > 0 {
		opaDir := filepath.Join(dir, "services", "opa")
		for relPath, content := range tmpl.OPA {
			dest := filepath.Join(opaDir, filepath.FromSlash(relPath))
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return fmt.Errorf("creating opa dir for %s: %w", relPath, err)
			}
			if err := os.WriteFile(dest, []byte(content), 0644); err != nil {
				return fmt.Errorf("writing opa/%s: %w", relPath, err)
			}
			fmt.Printf("  services/opa/%-35s written\n", relPath)
		}
	}

	return nil
}

// mergeDefaults recursively merges src into dst, only setting keys absent from dst.
// Existing values (cluster settings: fqdn, pki, topology) are preserved.
func mergeDefaults(dst, src map[string]any) {
	for k, srcVal := range src {
		dstVal, exists := dst[k]
		if !exists {
			dst[k] = srcVal
			continue
		}
		srcMap, srcIsMap := srcVal.(map[string]any)
		dstMap, dstIsMap := dstVal.(map[string]any)
		if srcIsMap && dstIsMap {
			mergeDefaults(dstMap, srcMap)
		}
	}
}

// --- export config ---

var exportConfigOutput string

var exportConfigCmd = &cobra.Command{
	Use:   "config <name>",
	Short: "Pack config dir into a named template file",
	Long: `Pack the current config directory's operator settings and OPA policy
into a single template YAML file, which 'init config' applies.

Captures:
  operator.yaml settings  → template operator: section (PKI and secrets stripped)
  services/opa/*.rego     → template opa: section

Example:
  schematic-operator export config dev1
  schematic-operator export config dev1 -o /path/to/config/dev1.yaml`,
	Args: cobra.ExactArgs(1),
	RunE: runExportConfig,
}

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export config dir to a portable template",
}

func init() {
	exportConfigCmd.Flags().StringVarP(&exportConfigOutput, "output", "o", "", "output file (default: <name>.yaml)")
	exportCmd.AddCommand(exportConfigCmd)
}

func runExportConfig(cmd *cobra.Command, args []string) error {
	name := args[0]
	dir := resolveConfigDir()

	outFile := exportConfigOutput
	if outFile == "" {
		outFile = name + ".yaml"
	}

	tmpl := &ConfigTemplate{}

	// Read operator.yaml and strip PKI/secrets.
	cfg, err := LoadConfig(dir)
	if err != nil {
		return err
	}
	// Clear sensitive/cluster-specific fields before exporting.
	cfg.Pki = nil
	cfg.Fqdn = ""
	cfg.Port = 0
	cfg.Topology = nil
	cfg.WebRoot = ""
	cfg.OutputDir = ""
	cfg.Patch = 0
	cfg.Dev = false
	cfg.GitlabRegistryToken = ""
	cfg.LlmCredential = ""
	cfg.TraceId = ""
	// Write to YAML and read back as a raw map so we can embed in the template.
	settingsYAML, err := protoOperator.ConfigToYAML(cfg)
	if err != nil {
		return fmt.Errorf("serializing operator settings: %w", err)
	}
	var settingsMap map[string]any
	if err := yaml.Unmarshal(settingsYAML, &settingsMap); err != nil {
		return fmt.Errorf("parsing operator settings: %w", err)
	}
	// Drop empty/nil entries.
	cleanMap(settingsMap)
	if len(settingsMap) > 0 {
		tmpl.Operator = settingsMap
	}

	// Read services/opa/ rego files.
	opaDir := filepath.Join(dir, "services", "opa")
	if fi, err := os.Stat(opaDir); err == nil && fi.IsDir() {
		opaFiles := map[string]string{}
		if err := collectRegoFiles(opaDir, "", opaFiles); err != nil {
			return fmt.Errorf("reading opa policy: %w", err)
		}
		if len(opaFiles) > 0 {
			tmpl.OPA = opaFiles
		}
	}

	data, err := yaml.Marshal(tmpl)
	if err != nil {
		return fmt.Errorf("marshaling template: %w", err)
	}

	header := fmt.Sprintf("# %s — schematic config template\n# Generated by: schematic-operator export config %s\n# Upload to schematic/config repo as %s.yaml\n\n", name, name, name)
	if err := os.WriteFile(outFile, append([]byte(header), data...), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", outFile, err)
	}

	fmt.Printf("Exported template: %s\n", outFile)
	if tmpl.Operator != nil {
		fmt.Printf("  operator settings: %d keys\n", len(tmpl.Operator))
	}
	if tmpl.OPA != nil {
		fmt.Printf("  opa rego files:    %d\n", len(tmpl.OPA))
	}
	return nil
}

// collectRegoFiles walks dir recursively and adds .rego file contents to files,
// keyed by their path relative to dir.
func collectRegoFiles(dir, rel string, files map[string]string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		relPath := name
		if rel != "" {
			relPath = rel + "/" + name
		}
		fullPath := filepath.Join(dir, name)
		if e.IsDir() {
			if err := collectRegoFiles(fullPath, relPath, files); err != nil {
				return err
			}
		} else if strings.HasSuffix(name, ".rego") {
			data, err := os.ReadFile(fullPath)
			if err != nil {
				return err
			}
			files[relPath] = string(data)
		}
	}
	return nil
}

// cleanMap removes nil and empty-map entries from m in-place.
func cleanMap(m map[string]any) {
	for k, v := range m {
		if v == nil {
			delete(m, k)
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			cleanMap(sub)
			if len(sub) == 0 {
				delete(m, k)
			}
		}
	}
}
