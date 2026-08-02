// Copyright (c) 2026 Second Star Cavalier LLC

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
//go:embed registry.json
var registryJSON []byte
