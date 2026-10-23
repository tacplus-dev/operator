package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"go.uber.org/zap"

	sm "gitlab.com/seabeam/schematic/platform/services/operator/model/s2sm"
	"gitlab.com/seabeam/schematic/platform/services/operator/model/s2sm/state/services/config_state"
	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	grpcconfig_pb "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/grpcconfig"
)

// kernelConfigState is the operator's read of kernel's tenant records: the
// generated kernel.ConfigState state client (operator.proto's state import),
// once the operator's own kernel identity is up. Until then Proto returns nil,
// which recordTenants reads as no record tenants.
type kernelConfigState struct {
	client atomic.Pointer[config_state.StateClient]
}

// Proto is the records source NewServiceConfigProvider reads each pass.
func (k *kernelConfigState) Proto() *protoKernel.ConfigState {
	c := k.client.Load()
	if c == nil {
		return nil
	}
	return c.Proto()
}

// startKernelConfigState brings up the operator's own ServiceManager and hands
// its ConfigState state client to the returned kernelConfigState.
//
// It runs in the background, and it must. sm.New() blocks until the operator
// can authenticate to kernel, retrying for many minutes before giving up — and
// on a fresh cluster kernel does not exist until this very reconciler has
// deployed it. Bootstrapping synchronously would deadlock the first install.
// Until it succeeds the reconciler renders operator.yaml's tenants only.
//
// sm.New() is the same bootstrap letsencrypt and opa use. SetServing(true)
// follows it because New() leaves the health handle NOT_SERVING and only Run()
// flips it — and the operator does not call Run(): it serves no gRPC of its own
// here, it only needs the kernel identity and the state client that come with it.
func startKernelConfigState(cfgDir string, log *zap.Logger) *kernelConfigState {
	k := &kernelConfigState{}
	go func() {
		if err := writeOperatorBootstrapConfig(cfgDir); err != nil {
			log.Warn("operator kernel identity unavailable: writing bootstrap config; kernel tenant records will not be deployed",
				zap.Error(err))
			return
		}

		s, err := sm.New()
		if err != nil {
			log.Warn("operator kernel identity unavailable: kernel tenant records will not be deployed",
				zap.Error(err))
			return
		}
		s.SetServing(true)

		k.client.Store(s.StateClients().Services().ConfigState())
		log.Info("operator kernel identity established; reading kernel tenant records")
	}()
	return k
}

// writeOperatorBootstrapConfig generates the operator's own chassis bootstrap
// config and points BOOTSTRAP_CONFIG at it, which is how grpcconfig.LoadConfig
// (and therefore sm.New) finds it.
//
// The operator generates this for itself because nothing else can: it is the
// component that generates every other service's bootstrap config, from the
// PKI and topology in its own config directory. The file is written to a temp
// dir rather than cfgDir so it is never picked up by the config bundle; it
// holds trusted CAs and endpoints only, no key material.
func writeOperatorBootstrapConfig(cfgDir string) error {
	dc, err := BuildDeployConfigs(cfgDir)
	if err != nil {
		return fmt.Errorf("building deploy configs: %w", err)
	}
	if len(dc.OperatorBootstrapConfig) == 0 {
		return fmt.Errorf("no operator bootstrap config was generated")
	}

	dir, err := os.MkdirTemp("", "operator-bootstrap")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	path := filepath.Join(dir, "bootstrap.yaml")
	if err := os.WriteFile(path, dc.OperatorBootstrapConfig, 0600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return os.Setenv(grpcconfig_pb.BootstrapConfigEnv, path)
}
