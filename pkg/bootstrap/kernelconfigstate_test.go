package bootstrap

import (
	"os"
	"testing"
	"time"

	"go.uber.org/zap"

	grpcconfig_pb "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/grpcconfig"
)

// Before the operator's kernel identity is up there is no ConfigState to read:
// the provider renders exactly what operator.yaml lists, as it did before the
// operator read kernel's records.
func TestKernelConfigStateBeforeKernelYieldsNoRecordTenants(t *testing.T) {
	k := &kernelConfigState{}
	if got := k.Proto(); got != nil {
		t.Fatalf("Proto() before the state client exists = %v, want nil", got)
	}
	sc, err := NewServiceConfigProvider(recordTenantsTestDir(t), k.Proto)()
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Tenants) != 0 || sc.RefusedTenants != nil {
		t.Fatalf("Tenants = %v, RefusedTenants = %v; want none", sc.Tenants, sc.RefusedTenants)
	}
}

// On a fresh cluster kernel does not exist until the reconciler deploys it, so
// starting the kernel read must return at once and leave the ServiceManager
// bring-up retrying in the background.
func TestStartKernelConfigStateDoesNotBlockOnKernel(t *testing.T) {
	dir := recordTenantsTestDir(t)
	t.Setenv(grpcconfig_pb.BootstrapConfigEnv, "")

	started := make(chan *kernelConfigState)
	go func() { started <- startKernelConfigState(dir, zap.NewNop()) }()
	var k *kernelConfigState
	select {
	case k = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("startKernelConfigState blocked")
	}

	// The bootstrap config is written first; once it is, the background
	// goroutine is in sm.New(), which cannot reach a kernel here.
	deadline := time.Now().Add(10 * time.Second)
	for os.Getenv(grpcconfig_pb.BootstrapConfigEnv) == "" {
		if time.Now().After(deadline) {
			t.Fatal("operator bootstrap config was never written")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := k.Proto(); got != nil {
		t.Fatalf("Proto() with no kernel reachable = %v, want nil", got)
	}
}
