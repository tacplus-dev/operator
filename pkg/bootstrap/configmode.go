package bootstrap

import (
	"io/fs"
	"os"
	"path/filepath"
)

// The config directory holds keys, PKI and tokens, so the operator makes it,
// and every directory in it, owner-only, and writes every file in it
// owner-only, the public ones (ca.pem, an API key's creds.yaml) included:
// nothing reads them off this disk but the operator's user (#29).
const (
	configDirMode  os.FileMode = 0700
	configFileMode os.FileMode = 0600
)

// writeConfigFile writes data to path 0600. os.WriteFile keeps the mode of a
// file that already exists, so a file an earlier version wrote 0644 is
// tightened here too.
func writeConfigFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, configFileMode); err != nil {
		return err
	}
	return os.Chmod(path, configFileMode)
}

// tightenConfigTree makes every directory under root (root included) 0700 and
// every file 0600, for a tree written with modes of its own: the OPA policy
// bundle's filesystem.Directory carries the 0755/0644 the pod sees, which is
// not what the config directory on disk gets.
func tightenConfigTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, configDirMode)
		}
		return os.Chmod(path, configFileMode)
	})
}
