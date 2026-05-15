package bootstrap

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	configProjectID = "secondstarcavalier%2Fcontrolplane%2Fcavalier%2Fconfig"
	configAPIBase   = "https://gitlab.com/api/v4"
)

var initConfigTag string

var initConfigCmd = &cobra.Command{
	Use:   "config <name>",
	Short: "Apply a named config template from the cavalier/config repo",
	Long: `Download and apply a named configuration template from the cavalier/config repo.

Files from the template are extracted into the config directory:
  - operator.yaml is deep-merged (template fills in missing keys; existing values win)
  - All other files (OPA rego, service configs) are written/overwritten

Run after 'cavalier-operator init-opa' to apply the custom OPA entitlements policy.

Examples:
  cavalier-operator init config dev1
  cavalier-operator init config dev1 --tag v1.0.0-main.3`,
	Args: cobra.ExactArgs(1),
	RunE: runInitConfig,
}

func init() {
	initConfigCmd.Flags().StringVar(&initConfigTag, "tag", "", "template version tag (default: latest release)")
}

func runInitConfig(cmd *cobra.Command, args []string) error {
	name := args[0]
	dir := resolveConfigDir()

	token := gitlabToken()
	if token == "" {
		return fmt.Errorf("no GitLab token found — set GITLAB_TOKEN or add a ~/.netrc entry for gitlab.com")
	}

	tag := initConfigTag
	if tag == "" {
		releases, err := fetchConfigReleases(token)
		if err != nil {
			return fmt.Errorf("fetching config releases: %w", err)
		}
		if len(releases) == 0 {
			return fmt.Errorf("no releases found in cavalier/config repo — tag and release the repo first")
		}
		tag = releases[0]
	}

	fmt.Printf("Config template: %s @ %s\n", name, tag)

	archiveURL := fmt.Sprintf("%s/projects/%s/repository/archive.tar.gz?sha=%s&path=%s",
		configAPIBase, configProjectID, tag, name)

	req, err := http.NewRequest(http.MethodGet, archiveURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading config archive: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("downloading config archive: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	count, err := extractConfigTemplate(resp.Body, name, dir)
	if err != nil {
		return fmt.Errorf("extracting config template: %w", err)
	}

	fmt.Printf("Config template '%s' applied to %s/ (%d files)\n", name, dir, count)
	return nil
}

// fetchConfigReleases returns release tag names from the config repo, newest first.
func fetchConfigReleases(token string) ([]string, error) {
	url := fmt.Sprintf("%s/projects/%s/releases?per_page=10", configAPIBase, configProjectID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var items []struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}

	tags := make([]string, len(items))
	for i, item := range items {
		tags[i] = item.TagName
	}
	return tags, nil
}

// extractConfigTemplate reads a tar.gz archive and extracts files from the named
// config path into destDir. The archive root and config name prefix are stripped.
// operator.yaml is deep-merged (template fills missing keys); all other files
// are overwritten. Returns the number of regular files extracted.
func extractConfigTemplate(r io.Reader, name, destDir string) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("decompressing archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	// The archive root is "{repo}-{ref}-{sha}/". We detect it from the first entry
	// and build the strip prefix as "{root}/{name}/".
	var stripPrefix string
	count := 0

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("reading archive: %w", err)
		}

		// Detect archive root from the first entry.
		if stripPrefix == "" {
			parts := strings.SplitN(hdr.Name, "/", 2)
			stripPrefix = parts[0] + "/" + name + "/"
		}

		if !strings.HasPrefix(hdr.Name, stripPrefix) {
			continue
		}

		rel := strings.TrimPrefix(hdr.Name, stripPrefix)
		if rel == "" {
			continue // the template root dir itself
		}

		dest := filepath.Join(destDir, filepath.FromSlash(rel))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0755); err != nil {
				return 0, fmt.Errorf("creating directory %s: %w", dest, err)
			}

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return 0, fmt.Errorf("creating parent dir for %s: %w", dest, err)
			}
			if filepath.Base(dest) == "operator.yaml" {
				if err := mergeConfigOperatorYAML(tr, dest); err != nil {
					return 0, fmt.Errorf("merging operator.yaml: %w", err)
				}
			} else {
				mode := hdr.FileInfo().Mode()
				if mode == 0 {
					mode = 0644
				}
				if err := writeConfigFile(tr, dest, mode); err != nil {
					return 0, fmt.Errorf("writing %s: %w", rel, err)
				}
			}
			count++
		}
	}

	if count == 0 {
		return 0, fmt.Errorf("no files found for template %q in archive — check the name and tag", name)
	}
	return count, nil
}

// mergeConfigOperatorYAML deep-merges the YAML from r into the existing operator.yaml
// at dest. Template values fill in missing keys; existing values take precedence.
// If dest does not exist, the template file is written as-is.
func mergeConfigOperatorYAML(r io.Reader, dest string) error {
	srcData, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	var srcMap map[string]any
	if err := yaml.Unmarshal(srcData, &srcMap); err != nil {
		return fmt.Errorf("parsing template operator.yaml: %w", err)
	}
	if srcMap == nil {
		return nil // nothing to merge
	}

	// Load existing if present.
	dstMap := map[string]any{}
	if data, err := os.ReadFile(dest); err == nil {
		if err := yaml.Unmarshal(data, &dstMap); err != nil {
			return fmt.Errorf("parsing existing operator.yaml: %w", err)
		}
		if dstMap == nil {
			dstMap = map[string]any{}
		}
	}

	// Merge: template fills in keys absent from existing (existing wins on conflict).
	mergeDefaults(dstMap, srcMap)

	out, err := yaml.Marshal(dstMap)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, out, 0644)
}

// mergeDefaults recursively merges src into dst, but only sets keys from src
// that are not already present in dst. For map values, recurses into nested maps.
// This preserves existing cluster settings (fqdn, pki, topology) while applying
// template defaults (tls, optional_services) if not already set.
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
		// Both non-map: existing wins, no-op.
	}
}

func writeConfigFile(r io.Reader, dest string, mode os.FileMode) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
