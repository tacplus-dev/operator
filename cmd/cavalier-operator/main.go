package main

import (
	"os"

	"github.com/tacplus-dev/operator/pkg/bootstrap"
)

func main() {
	if err := bootstrap.Execute(); err != nil {
		os.Exit(1)
	}
}
