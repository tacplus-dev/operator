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

func TestOperatorRepoUsesDeployableServiceNamespace(t *testing.T) {
	want := "secondstarcavalier/schematic/platform/services/operator/cavalier-operator"
	if got := operatorRepo(); got != want {
		t.Fatalf("operatorRepo() = %q, want %q", got, want)
	}
}
