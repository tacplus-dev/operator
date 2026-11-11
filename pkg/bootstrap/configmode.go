package bootstrap

import "os"

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
