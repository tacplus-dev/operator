package bootstrap

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// `upgrade` takes the operator's releases from its tacplus-dev home: the
// GitHub releases of tacplus-dev/operator.
func TestUpgradeReadsReleasesFromTacplusDevOnGitHub(t *testing.T) {
	if upgradeAPIBase != "https://api.github.com/repos/tacplus-dev/operator" {
		t.Errorf("upgradeAPIBase = %q", upgradeAPIBase)
	}
	if got, want := releaseAssetURL("v0.1.0", "schematic-operator-linux-arm64"),
		"https://github.com/tacplus-dev/operator/releases/download/v0.1.0/schematic-operator-linux-arm64"; got != want {
		t.Errorf("releaseAssetURL() = %q, want %q", got, want)
	}

	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[{"tag_name":"v0.2.0"},{"tag_name":"v0.1.1"}]`))
	}))
	defer srv.Close()
	prev := upgradeAPIBase
	upgradeAPIBase = srv.URL
	defer func() { upgradeAPIBase = prev }()

	tags, err := fetchReleases("")
	if err != nil {
		t.Fatalf("fetchReleases: %v", err)
	}
	if want := []string{"v0.2.0", "v0.1.1"}; !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
	if gotPath != "/releases" {
		t.Errorf("requested %q, want /releases", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q without a token, want none (the repository is public)", gotAuth)
	}

	if _, err := fetchReleases("ghp-not-a-real-token"); err != nil {
		t.Fatalf("fetchReleases with token: %v", err)
	}
	if gotAuth != "Bearer ghp-not-a-real-token" {
		t.Errorf("Authorization = %q, want the token as Bearer", gotAuth)
	}
}
