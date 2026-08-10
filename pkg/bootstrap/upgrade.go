package bootstrap

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const (
	upgradeProjectID   = "81967216"
	upgradePackageName = "schematic-operator"
	upgradeAPIBase     = "https://gitlab.com/api/v4"
)

// Version is the current binary version, set at build time via -ldflags.
var Version = "dev"

var upgradeVersion string

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade schematic-operator to the latest patch release",
	Long: `Download and replace the running schematic-operator binary.

By default, upgrades to the latest patch release on the current MAJOR.MINOR
version line (e.g. if running v1.2.1, installs the latest v1.2.x).
Patch releases contain bug fixes only and are safe to apply without risk of
destabilising existing deployments.

If a newer MAJOR.MINOR release line is available, a notice is printed to
stderr with the command to upgrade — run it when you are ready.

Version selection:
  (default)         Latest patch for the current MAJOR.MINOR
  --version 1.2     Latest patch for major.minor 1.2
  --version 1.2.3   Exact release v1.2.3

Authentication is read from (in order):
  1. GITLAB_TOKEN or PRIVATE_TOKEN environment variable
  2. ~/.netrc entry for gitlab.com:

       machine gitlab.com
       login <username>
       password <your-pat>`,
	RunE: runUpgrade,
}

func init() {
	upgradeCmd.Flags().StringVarP(&upgradeVersion, "version", "v", "", "version to install: MAJOR.MINOR or MAJOR.MINOR.PATCH")
	rootCmd.AddCommand(upgradeCmd)
}

func runUpgrade(_ *cobra.Command, _ []string) error {
	token := gitlabToken()
	if token == "" {
		return fmt.Errorf("no GitLab token found — set GITLAB_TOKEN or add a ~/.netrc entry for gitlab.com")
	}

	binName, err := platformBinary()
	if err != nil {
		return err
	}

	releases, err := fetchReleases(token)
	if err != nil {
		return fmt.Errorf("fetching releases: %w", err)
	}
	if len(releases) == 0 {
		return fmt.Errorf("no releases found")
	}

	tag, err := resolveReleaseTag(releases, upgradeVersion)
	if err != nil {
		return fmt.Errorf("resolving version: %w", err)
	}

	// Notify if a newer MAJOR.MINOR release line exists beyond the resolved tag.
	checkNewerLine(releases, tag)

	fmt.Printf("Installing schematic-operator %s (%s)...\n", tag, binName)

	url := fmt.Sprintf("%s/projects/%s/packages/generic/%s/%s/%s",
		upgradeAPIBase, upgradeProjectID, upgradePackageName, tag, binName)

	current, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current binary: %w", err)
	}
	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		return fmt.Errorf("resolving symlinks: %w", err)
	}

	if err := downloadReplace(url, token, current); err != nil {
		return err
	}

	fmt.Printf("Successfully upgraded to %s.\n", tag)
	return nil
}

// resolveReleaseTag selects the exact release tag to download.
// version may be "" (current line's latest patch), "MAJOR.MINOR", or "MAJOR.MINOR.PATCH".
func resolveReleaseTag(releases []string, version string) (string, error) {
	if version != "" {
		bare := strings.TrimPrefix(version, "v")
		parts := strings.Split(bare, ".")
		switch len(parts) {
		case 3:
			return "v" + bare, nil
		case 2:
			return latestPatchFor(releases, bare)
		default:
			return "", fmt.Errorf("invalid version %q: use MAJOR.MINOR or MAJOR.MINOR.PATCH", version)
		}
	}

	// Default: latest patch for the running binary's MAJOR.MINOR.
	mm := currentMajorMinor()
	if mm == "" {
		// Dev build — upgrade to absolute latest.
		return releases[0], nil
	}
	return latestPatchFor(releases, mm)
}

// latestPatchFor returns the newest release whose tag starts with vMAJOR.MINOR.
func latestPatchFor(releases []string, majorMinor string) (string, error) {
	prefix := "v" + strings.TrimPrefix(majorMinor, "v") + "."
	for _, tag := range releases {
		if strings.HasPrefix(tag, prefix) {
			return tag, nil
		}
	}
	return "", fmt.Errorf("no release found matching %s.x", majorMinor)
}

// CurrentSeries returns the image series name derived from the binary version,
// e.g. Version "v1.2.12-main.3" → "1-2". Returns "1-1" if unparseable.
func CurrentSeries() string {
	mm := currentMajorMinor()
	if mm == "" {
		return "1-1"
	}
	return strings.ReplaceAll(mm, ".", "-")
}

// CurrentMajorMinor returns "MAJOR.MINOR" from the binary version, e.g. "1.2".
// Returns "" if unparseable.
func CurrentMajorMinor() string {
	return currentMajorMinor()
}

// CurrentProtoPatch returns the PROTO_PATCH component of the binary version.
// e.g. Version "v1.2.12-main.3" → 12. Returns 0 if unparseable.
func CurrentProtoPatch() int {
	_, pp, _, _ := parseVersion(Version)
	return pp
}

// CurrentBranch returns the BRANCH component of the binary version.
// e.g. Version "v1.2.12-main.3" → "main". Returns "main" if unparseable.
func CurrentBranch() string {
	branch, _, _, _ := parseVersion(Version)
	if branch == "" {
		return "main"
	}
	return branch
}

// currentMajorMinor parses Version and returns "MAJOR.MINOR", or "" if unparseable.
// Works with both old format (v1.2.27) and new format (v1.2.12-main.3).
func currentMajorMinor() string {
	bare := strings.TrimPrefix(Version, "v")
	parts := strings.Split(bare, ".")
	if len(parts) < 2 {
		return ""
	}
	if _, err := strconv.Atoi(parts[0]); err != nil {
		return ""
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// parseVersion parses a version string in the format:
//
//	v{MAJOR}.{MINOR}.{PROTO_PATCH}-{BRANCH}.{LOCAL_PATCH}
//	e.g. "v1.2.12-main.3"
//
// Returns (branch, protoPatch, localPatch, ok). ok=false for old-format versions.
func parseVersion(v string) (branch string, protoPatch, localPatch int, ok bool) {
	bare := strings.TrimPrefix(v, "v")
	// Expected: "1.2.12-main.3"
	// Split on "-" to separate "1.2.12" from "main.3"
	dashIdx := strings.Index(bare, "-")
	if dashIdx < 0 {
		return "", 0, 0, false
	}
	numPart := bare[:dashIdx]   // "1.2.12"
	branchPart := bare[dashIdx+1:] // "main.3"

	numSegs := strings.Split(numPart, ".")
	if len(numSegs) != 3 {
		return "", 0, 0, false
	}
	pp, err := strconv.Atoi(numSegs[2])
	if err != nil {
		return "", 0, 0, false
	}

	// branchPart: "main.3" — last dot-segment is the local patch
	lastDot := strings.LastIndex(branchPart, ".")
	if lastDot < 0 {
		return "", 0, 0, false
	}
	br := branchPart[:lastDot]
	lp, err := strconv.Atoi(branchPart[lastDot+1:])
	if err != nil {
		return "", 0, 0, false
	}

	return br, pp, lp, true
}

// checkNewerLine prints a stderr notice if any release has a higher MAJOR.MINOR than target.
func checkNewerLine(releases []string, target string) {
	targetMM := parseMajorMinor(target)
	if targetMM == nil {
		return
	}
	for _, tag := range releases {
		mm := parseMajorMinor(tag)
		if mm == nil {
			continue
		}
		if mm[0] > targetMM[0] || (mm[0] == targetMM[0] && mm[1] > targetMM[1]) {
			bin := filepath.Base(os.Args[0])
			fmt.Fprintf(os.Stderr,
				"notice: a newer release line v%d.%d is available — to upgrade, run: %s upgrade --version %d.%d\n",
				mm[0], mm[1], bin, mm[0], mm[1])
			return // report only the newest newer line
		}
	}
}

// parseMajorMinor parses a tag like "v1.2.3" -> [major, minor]. Returns nil if unparseable.
func parseMajorMinor(tag string) []int {
	bare := strings.TrimPrefix(tag, "v")
	parts := strings.Split(bare, ".")
	if len(parts) < 2 {
		return nil
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil
	}
	return []int{major, minor}
}

// fetchReleases returns all release tag names sorted newest-first.
func fetchReleases(token string) ([]string, error) {
	url := fmt.Sprintf("%s/projects/%s/releases?per_page=100", upgradeAPIBase, upgradeProjectID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetching releases: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var items []struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, fmt.Errorf("parsing releases: %w", err)
	}

	tags := make([]string, len(items))
	for i, item := range items {
		tags[i] = item.TagName
	}
	return tags, nil
}

// downloadReplace downloads url into a temp file then atomically replaces dest.
// On Windows (where running executables are locked) it writes dest+".new" instead.
func downloadReplace(url, token, dest string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	fmt.Printf("Downloading from %s\n", url)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading binary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("downloading binary: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	dir := filepath.Dir(dest)

	if runtime.GOOS == "windows" {
		// Windows locks running executables; write alongside and instruct the user.
		newPath := strings.TrimSuffix(dest, ".exe") + ".new.exe"
		if err := writeFile(newPath, resp.Body, 0755); err != nil {
			return err
		}
		fmt.Printf("Downloaded to %s\n", newPath)
		fmt.Printf("To complete the upgrade, run:\n  move /Y %q %q\n", newPath, dest)
		return nil
	}

	tmp, err := os.CreateTemp(dir, ".schematic-operator-upgrade-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("writing download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0755); err != nil {
		return fmt.Errorf("setting permissions: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("replacing binary: %w", err)
	}
	return nil
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return f.Close()
}

// platformBinary returns the binary filename for the current OS/arch.
func platformBinary() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "schematic-operator-darwin-arm64", nil
	case "linux/amd64":
		return "schematic-operator-linux-amd64", nil
	case "linux/arm64":
		return "schematic-operator-linux-arm64", nil
	case "windows/amd64":
		return "schematic-operator-windows-amd64.exe", nil
	case "windows/arm64":
		return "schematic-operator-windows-arm64.exe", nil
	default:
		return "", fmt.Errorf("unsupported platform %s/%s — download manually from the releases page", runtime.GOOS, runtime.GOARCH)
	}
}

// gitlabToken returns a GitLab PAT from env or ~/.netrc.
func gitlabToken() string {
	if t := os.Getenv("GITLAB_TOKEN"); t != "" {
		return t
	}
	if t := os.Getenv("PRIVATE_TOKEN"); t != "" {
		return t
	}
	return netrcPassword("gitlab.com")
}

// netrcPassword parses ~/.netrc and returns the password for the given machine.
func netrcPassword(machine string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".netrc"))
	if err != nil {
		return ""
	}
	// Tokenise by whitespace — handles both single-line and multi-line netrc.
	toks := strings.Fields(string(data))
	for i := 0; i+1 < len(toks); i++ {
		if toks[i] == "machine" && toks[i+1] == machine {
			for j := i + 2; j+1 < len(toks); j++ {
				if toks[j] == "machine" {
					break // next machine stanza
				}
				if toks[j] == "password" {
					return toks[j+1]
				}
			}
		}
	}
	return ""
}
