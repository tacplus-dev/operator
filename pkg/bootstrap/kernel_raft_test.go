// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tacplus-dev/operator/pkg/helm"
	protoKernel "github.com/tacplus-dev/operator/tacplus/types/services/kernel"
)

// The chart's kernel-raft NetworkPolicy guards the port kernel's own Config
// binds Ratis on: every path to the chart (the in-cluster provider, generate,
// a bundle) hands the chart that Config, and its port is the one each kernel
// node's bootstrap carries.
func TestChartGuardsTheRaftPortKernelsConfigBinds(t *testing.T) {
	dir, _ := kmakerRetiredFixture(t)
	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	port := helm.KernelRaftPort(dc.Kernel)
	if port == 0 {
		t.Fatal("no Raft port in kernel's Config")
	}
	for i, bs := range dc.BootstrapYAMLs {
		node, err := protoKernel.ConfigFromBytes(bs)
		if err != nil {
			t.Fatalf("%s: %v", dc.BootstrapNames[i], err)
		}
		if got := node.GetRatisListener().GetPort(); got != port {
			t.Errorf("%s binds Raft on %d, the chart guards %d", dc.BootstrapNames[i], got, port)
		}
	}

	sc, err := NewServiceConfigProvider(dir, nil)()
	if err != nil {
		t.Fatal(err)
	}
	if got := helm.KernelRaftPort(sc.Kernel); got != port || sc.Kernel == nil {
		t.Errorf("provider's kernel Config = %v (Raft port %d), want port %d", sc.Kernel != nil, got, port)
	}

	b, err := BuildBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	bk, err := BundleKernelConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := helm.KernelRaftPort(bk); bk == nil || got != port {
		t.Errorf("bundle's kernel Config = %v (Raft port %d), want port %d", bk != nil, got, port)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	charts, err := helm.New(CurrentSeries(), CurrentMajorMinor(), nil, nil, nil).WithKernelConfig(dc.Kernel).Generate(cfg, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	np := string(charts.Files["templates/kernel/networkpolicy-raft.yaml"])
	if !strings.Contains(np, fmt.Sprintf("      port: %d\n", port)) {
		t.Errorf("kernel-raft does not name port %d:\n%s", port, np)
	}
}
