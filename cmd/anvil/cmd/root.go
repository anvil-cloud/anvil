package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Build-time variables injected via ldflags.
// Example: go build -ldflags "-X github.com/anvil-cloud/anvil/cmd/anvil/cmd.version=0.1.0"
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var rootCmd = &cobra.Command{
	Use:   "anvil",
	Short: "Anvil – opinionated cloud infrastructure, zero configuration",
	Long: `Anvil wraps cloud resources into secure-by-default components
so you can ship infrastructure without the boilerplate.

  anvil deploy      Deploy your infrastructure
  anvil preview     Preview changes without applying
  anvil destroy     Tear down a deployment
  anvil bootstrap   Set up state storage in your cloud account`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return err
	}
	return nil
}

func init() {
	rootCmd.AddCommand(versionCmd)
	rootCmd.CompletionOptions.DisableDefaultCmd = true
}
