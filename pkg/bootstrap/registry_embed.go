// Copyright (c) 2026 Seabeam LLC. All rights reserved.

package bootstrap

import _ "embed"

// registryJSON is the tacplus-generated service registry, copied into this
// package by `just generate` and embedded so the operator binary is
// self-contained (it runs both on the host and in-cluster, where model/ is
// not present).
//
// It is embedded here rather than exported from the generated model package
// because registry.json is ~26MB: emitting an embed for it in every service's
// model would bloat every binary in the fleet for the sake of this one caller.
//
// A stale copy is a silent, live-affecting bug: the delegate authorization
// graph (registry.go) is computed from it, so a rebuild that regenerated model/
// without refreshing this copy ships an operator that does not know about
// services added since. `go generate ./pkg/bootstrap` refreshes it, and
// TestEmbeddedRegistryMatchesGeneratedModel fails the package's tests while
// the two differ.
//
//go:generate cp ../../model/registry.json registry.json
//go:embed registry.json
var registryJSON []byte
