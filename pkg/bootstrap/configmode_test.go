package bootstrap

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// assertConfigModes fails for every directory under root that is not 0700 and
// every file that is not 0600: the config directory holds keys, PKI and
// tokens, so nothing in it is group- or world-readable (#29).
func assertConfigModes(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0600)
		if d.IsDir() {
			want = 0700
		}
		if got := fi.Mode().Perm(); got != want {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s: mode %#o, want %#o", rel, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

// initClusterDir runs `init ca` then `init cluster` into a config directory
// that does not exist yet, as a first install does, and returns it.
func initClusterDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "config")

	prevDir, prevFQDN, prevPort, prevType := configDir, initClusterFQDN, initClusterPort, initClusterType
	t.Cleanup(func() {
		configDir, initClusterFQDN, initClusterPort, initClusterType = prevDir, prevFQDN, prevPort, prevType
	})
	configDir = dir
	initClusterFQDN = "console.example.test"
	initClusterPort = 443
	initClusterType = "small"

	if err := runInitCA(nil, nil); err != nil {
		t.Fatalf("init ca: %v", err)
	}
	if err := runInitCluster(nil, nil); err != nil {
		t.Fatalf("init cluster: %v", err)
	}
	return dir
}

// `init ca`, `init cluster` and `generate` create the config directory 0700
// and each file in it 0600, the public ones (ca.pem, configs/ca.pem) included,
// and so does an expanded bundle.
func TestConfigDirAndFilesAreCreatedOwnerOnly(t *testing.T) {
	dir := initClusterDir(t)
	assertConfigModes(t, dir)

	if err := BuildConfigOutput(dir); err != nil {
		t.Fatalf("configs: %v", err)
	}
	assertConfigModes(t, dir)

	b, err := BuildBundle(dir)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	expanded := filepath.Join(t.TempDir(), "expanded")
	if err := ExpandBundle(b, expanded); err != nil {
		t.Fatalf("ExpandBundle: %v", err)
	}
	assertConfigModes(t, expanded)
}

// `add api key` writes creds/<name>/ 0700 and the public JWK (creds.yaml) 0600,
// like the private key beside it and the services/oauth.yaml it updates.
func TestAddApiKeyWritesOwnerOnly(t *testing.T) {
	dir := initClusterDir(t)
	if err := AddApiKey(dir, "ci", nil, nil); err != nil {
		t.Fatalf("add api key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "creds", "ci", "creds.yaml")); err != nil {
		t.Fatalf("creds.yaml: %v", err)
	}
	assertConfigModes(t, dir)
}

// `set-deploy-list` creates the config directory 0700 when it is the first
// command run, and deploy.list 0600; the backend token policy's directory and
// rego (it holds the provider tokens) are the same.
func TestDeployListAndBackendTokenPolicyAreOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	prevDir := configDir
	t.Cleanup(func() { configDir = prevDir })
	configDir = dir

	list := filepath.Join(t.TempDir(), "deploy.list")
	if err := os.WriteFile(list, []byte("kernel\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runSetDeployList(nil, []string{list}); err != nil {
		t.Fatalf("set-deploy-list: %v", err)
	}

	clearBackendTokenEnv(t)
	t.Setenv("ANTHROPIC_TOKEN", "sk-ant-test")
	if _, _, err := writeBackendTokenPolicy(dir); err != nil {
		t.Fatalf("backend token policy: %v", err)
	}
	assertConfigModes(t, dir)
}

// `init-opa` writes the policy tree through the bundle's filesystem.Directory,
// whose own modes are 0755/0644 (what the pod sees); on disk, in the config
// directory, it is 0700/0600 like everything else.
func TestDefaultOPAPolicyIsWrittenOwnerOnly(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "opa")
	if err := os.MkdirAll(dest, 0700); err != nil {
		t.Fatal(err)
	}
	if err := WriteDefaultOPAPolicy(dest, []string{"https://accounts.google.com"}, nil); err != nil {
		t.Fatalf("WriteDefaultOPAPolicy: %v", err)
	}
	var files int
	if err := filepath.WalkDir(dest, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files++
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no policy file written")
	}
	assertConfigModes(t, dest)
}

// A file an earlier version wrote 0644 is 0600 once the operator rewrites it.
func TestRewrittenConfigFileIsTightened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigFile(path, []byte("new")); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0600 {
		t.Errorf("mode %#o, want 0600", got)
	}
}
