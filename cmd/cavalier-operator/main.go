package main

import (
	"os"

	"gitlab.com/seabeam/schematic/platform/services/operator/pkg/bootstrap"
)

func main() {
	if err := bootstrap.Execute(); err != nil {
		os.Exit(1)
	}
}
