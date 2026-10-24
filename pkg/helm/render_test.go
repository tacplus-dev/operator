// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package helm

import (
	"testing"

	"helm.sh/helm/v3/pkg/action"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/releaseutil"
	k8syaml "sigs.k8s.io/yaml"
)

// renderedManifests renders the generated chart with helm (client-only dry
// run) and keys each manifest by Kind/name, so tests assert on what a cluster
// would actually receive rather than on template text.
func renderedManifests(t *testing.T, charts *Charts) map[string]string {
	t.Helper()
	buffered := make([]*helmloader.BufferedFile, 0, len(charts.Files))
	for name, data := range charts.Files {
		buffered = append(buffered, &helmloader.BufferedFile{Name: name, Data: data})
	}
	ch, err := helmloader.LoadFiles(buffered)
	if err != nil {
		t.Fatalf("loading chart: %v", err)
	}
	install := action.NewInstall(new(action.Configuration))
	install.ReleaseName = "schematic"
	install.Namespace = "schematic"
	install.DryRun = true
	install.ClientOnly = true
	rel, err := install.Run(ch, map[string]interface{}{})
	if err != nil {
		t.Fatalf("helm dry-run render: %v", err)
	}
	out := map[string]string{}
	for _, doc := range releaseutil.SplitManifests(rel.Manifest) {
		var head struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := k8syaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatalf("decoding rendered manifest: %v\n%s", err, doc)
		}
		out[head.Kind+"/"+head.Metadata.Name] = doc
	}
	return out
}
