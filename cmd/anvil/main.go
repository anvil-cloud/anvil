package main

import (
	"os"

	"github.com/anvil-cloud/anvil/cmd/anvil/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
