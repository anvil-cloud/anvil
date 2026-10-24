package cmd

import (
	"fmt"
	"regexp"

	"github.com/spf13/cobra"
)

var stageSetEnvironment string

var stageCmd = &cobra.Command{
	Use:   "stage",
	Short: "Manage stages",
	Long:  `View or set the active stage for deploy and preview commands.`,
}

var stageSetCmd = &cobra.Command{
	Use:   "set [name]",
	Short: "Set the active stage",
	Long:  `Set the default stage used by deploy and preview. Destroy always requires --stage explicitly.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runStageSet,
}

var stageGetCmd = &cobra.Command{
	Use:   "get",
	Short: "Show the active stage",
	RunE:  runStageGet,
}

func init() {
	stageSetCmd.Flags().StringVar(&stageSetEnvironment, "environment", "", "Environment type: prod or nonprod (default nonprod)")
	stageCmd.AddCommand(stageSetCmd)
	stageCmd.AddCommand(stageGetCmd)
	rootCmd.AddCommand(stageCmd)
}

var validStageName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)

func runStageSet(cmd *cobra.Command, args []string) error {
	name := args[0]

	if !validStageName.MatchString(name) {
		return fmt.Errorf("Invalid stage name \"%s\".\n  Use alphanumeric characters and hyphens only.", name)
	}

	config, err := loadAnvilConfig()
	if err != nil {
		config = &anvilConfig{
			Stages: make(map[string]*stageConfig),
		}
	}

	config.Active = name

	// If this stage already exists, update its environment if a new one is provided
	sc, exists := config.Stages[name]

	// Resolve environment: flag → existing → default
	env := stageSetEnvironment
	if env == "" && exists && sc.Environment != "" {
		env = sc.Environment
	}
	if env == "" {
		env = defaultEnvironment
	}

	if env != "prod" && env != "nonprod" {
		return fmt.Errorf("Invalid environment %q. Must be \"prod\" or \"nonprod\".", env)
	}

	// Update or create the stage config
	if exists {
		sc.Environment = env
	} else {
		if config.Stages == nil {
			config.Stages = make(map[string]*stageConfig)
		}
		config.Stages[name] = &stageConfig{
			Environment: env,
		}
	}

	err = writeAnvilConfig(*config)
	if err != nil {
		return fmt.Errorf("failed to write anvil.yaml: %w", err)
	}

	if isTTY() {
		fmt.Printf("  %s Stage set to: %s (%s)\n", green("✔"), bold(name), env)
	} else {
		fmt.Printf("  Stage set to: %s (%s)\n", name, env)
	}

	return nil
}

func runStageGet(cmd *cobra.Command, args []string) error {
	config, err := loadAnvilConfig()
	if err != nil || config.Active == "" {
		fmt.Println("  No active stage set. Using default: dev")
		fmt.Println("  Run `anvil stage set <n>` to set one.")
		return nil
	}

	sc, ok := config.Stages[config.Active]
	if isTTY() {
		if ok && sc.Environment != "" {
			fmt.Printf("  Active stage: %s (%s)\n", bold(config.Active), sc.Environment)
		} else {
			fmt.Printf("  Active stage: %s\n", bold(config.Active))
		}
	} else {
		if ok && sc.Environment != "" {
			fmt.Printf("  Active stage: %s (%s)\n", config.Active, sc.Environment)
		} else {
			fmt.Printf("  Active stage: %s\n", config.Active)
		}
	}

	return nil
}
