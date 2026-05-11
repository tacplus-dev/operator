package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	protoKey "gitlab.com/secondstarcavalier/schematic/platform/operator/tacplus/types/libs/key"
)

// ResolveConfigDirSmart determines the bootstrap directory using smart detection:
// 1. If -c flag or $CAVALIER_OPERATOR_DIR is set → use that
// 2. If ./creds/ or ./services/ exists → cwd is the bootstrap dir
// 3. If ./bootstrap/creds/ or ./bootstrap/services/ exists → use ./bootstrap
// 4. Default to ./bootstrap
func ResolveConfigDirSmart() string {
	if env := os.Getenv("CAVALIER_OPERATOR_DIR"); env != "" {
		return env
	}
	for _, marker := range []string{"creds", "services"} {
		if _, err := os.Stat(marker); err == nil {
			return "."
		}
		if _, err := os.Stat(filepath.Join(OperatorBootstrapDir, marker)); err == nil {
			return OperatorBootstrapDir
		}
	}
	return OperatorBootstrapDir
}

// LoadPrivateKey reads a Key proto from a YAML or JSON file in a cred pair directory.
func LoadPrivateKey(credPairDir string) (*protoKey.Key, error) {
	var keyPath string
	for _, ext := range []string{".yaml", ".json"} {
		p := filepath.Join(credPairDir, "key"+ext)
		if _, err := os.Stat(p); err == nil {
			keyPath = p
			break
		}
	}
	if keyPath == "" {
		return nil, fmt.Errorf("no key file found in %s", credPairDir)
	}

	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}

	return protoKey.KeyFromBytes(data)
}
