package upgrade

import "testing"

func TestSelectLatestChoosesLatestCompatibleProtoPatch(t *testing.T) {
	tags := []string{
		"v1.2.181-main.1",
		"v1.2.181-main.2",
		"v1.2.184-main.1",
		"v1.2.183-demo.9",
		"v1.3.183-main.9",
	}

	got := selectLatest(tags, "1.2", 183, "main")
	want := resolvedVersion{protoPatch: 181, localPatch: 2}
	if got != want {
		t.Fatalf("selectLatest() = %+v, want %+v", got, want)
	}
}

func TestSelectLatestAllowsUnboundedProtoPatch(t *testing.T) {
	tags := []string{
		"v1.2.181-main.2",
		"v1.2.184-main.1",
	}

	got := selectLatest(tags, "1.2", 0, "main")
	want := resolvedVersion{protoPatch: 184, localPatch: 1}
	if got != want {
		t.Fatalf("selectLatest() = %+v, want %+v", got, want)
	}
}

// A LOCAL_PATCH that does not fit int32 comes from the registry, not from us:
// the tag is ignored instead of wrapping (4294967297 would wrap to 1) and
// its PROTO_PATCH outranking a valid tag's (tacplus-dev/operator#27).
func TestSelectLatestIgnoresLocalPatchBeyondInt32(t *testing.T) {
	tags := []string{
		"v1.2.182-main.4294967297",
		"v1.2.182-main.2147483648",
		"v1.2.181-main.3",
	}

	got := selectLatest(tags, "1.2", 0, "main")
	want := resolvedVersion{protoPatch: 181, localPatch: 3}
	if got != want {
		t.Fatalf("selectLatest() = %+v, want %+v", got, want)
	}
}

// Each service's image is its release's, ghcr.io/tacplus-dev/<service>, the
// operator's own included (tacplus-dev/operator#6).
func TestReposAreTacplusDevOnGhcr(t *testing.T) {
	if registryHost != "ghcr.io" {
		t.Errorf("registryHost = %q, want ghcr.io", registryHost)
	}
	if got, want := operatorRepo(), "tacplus-dev/operator"; got != want {
		t.Errorf("operatorRepo() = %q, want %q", got, want)
	}
	if got, want := serviceRepo(ServiceRef{Proj: "kernel", Svc: "kernel"}), "tacplus-dev/kernel"; got != want {
		t.Errorf("serviceRepo(kernel) = %q, want %q", got, want)
	}
}

func TestRegistryTokenURLIsGhcr(t *testing.T) {
	want := "https://ghcr.io/token?service=ghcr.io&scope=repository:tacplus-dev/kernel:pull"
	if got := registryTokenURL("tacplus-dev/kernel"); got != want {
		t.Errorf("registryTokenURL() = %q, want %q", got, want)
	}
}
