package main

import (
	"os"

	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/bootstrap"
)

func main() {
	if err := bootstrap.Execute(); err != nil {
		os.Exit(1)
	}
}
