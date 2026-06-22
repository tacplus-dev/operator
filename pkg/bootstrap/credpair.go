package bootstrap

import (
	"os"
	"path/filepath"
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
