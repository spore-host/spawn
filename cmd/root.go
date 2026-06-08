package cmd

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/libs/update"
)

var Version = "0.38.0"

// Global flags for i18n and accessibility
var (
	flagLang          string
	flagNoEmoji       bool
	flagAccessibility bool
	flagNoColor       bool

	// Output / display flags
	spawnOutputFormat string
	spawnVerbose      bool
)

var rootCmd = &cobra.Command{
	Use: "spawn",
	// Short and Long descriptions will be set after i18n initialization
}

var i18nInitialized = false

func Execute() {
	// Parse flags early to get --lang value before help is displayed
	_ = rootCmd.ParseFlags(os.Args[1:])
	ensureI18nInitialized()

	// Start async update check (non-blocking, respects SPORE_NO_UPDATE_CHECK)
	updateCh := update.CheckAsync("spawn", Version)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Print update notice after command completes (if available)
	select {
	case result := <-updateCh:
		if result.HasUpdate() {
			fmt.Fprintf(os.Stderr, "\n%s\n", result.Message())
		}
	default:
	}
}

func init() {
	// Set PersistentPreRunE to initialize i18n after flag parsing
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		ensureI18nInitialized()
		return nil
	}

	// Add global i18n and accessibility flags
	rootCmd.PersistentFlags().StringVar(&flagLang, "lang", "", "Language for output (en, es, fr, de, ja, pt)")
	rootCmd.PersistentFlags().BoolVar(&flagNoEmoji, "no-emoji", false, "Disable emoji in output")
	rootCmd.PersistentFlags().BoolVar(&flagAccessibility, "accessibility", false, "Enable accessibility mode (implies --no-emoji)")
	rootCmd.PersistentFlags().BoolVar(&flagNoColor, "no-color", false, "Disable colorized output")

	// Output format and verbosity
	rootCmd.PersistentFlags().StringVarP(&spawnOutputFormat, "output", "o", "table", "Output format (table, json)")
	rootCmd.PersistentFlags().BoolVarP(&spawnVerbose, "verbose", "v", false, "Enable verbose output")

	// Enable shell completion for all supported shells
	rootCmd.CompletionOptions.DisableDefaultCmd = false
	rootCmd.CompletionOptions.DisableDescriptions = false
}

func ensureI18nInitialized() {
	if i18nInitialized {
		return
	}
	initI18n()
	i18nInitialized = true
}

func initI18n() {
	// Initialize i18n with configuration from flags
	cfg := i18n.Config{
		Language:          flagLang,
		Verbose:           false,
		AccessibilityMode: flagAccessibility,
		NoEmoji:           flagNoEmoji,
	}

	if err := i18n.Init(cfg); err != nil {
		log.Printf("Warning: failed to initialize i18n: %v", err)
		// Continue with default English
	}

	// Set command descriptions after i18n is initialized
	updateCommandDescriptions()
}

// getOutputFormat returns the global output format flag value.
func getOutputFormat() string {
	return spawnOutputFormat
}

func updateCommandDescriptions() {
	// Root command
	rootCmd.Short = i18n.T("spawn.root.short")
	rootCmd.Long = i18n.T("spawn.root.long")

	// Launch command
	if cmd, _, err := rootCmd.Find([]string{"launch"}); err == nil && cmd != nil {
		cmd.Short = i18n.T("spawn.launch.short")
		cmd.Long = i18n.T("spawn.launch.long")
	}

	// Connect command
	if cmd, _, err := rootCmd.Find([]string{"connect"}); err == nil && cmd != nil {
		cmd.Short = i18n.T("spawn.connect.short")
		cmd.Long = i18n.T("spawn.connect.long")
	}

	// List command
	if cmd, _, err := rootCmd.Find([]string{"list"}); err == nil && cmd != nil {
		cmd.Short = i18n.T("spawn.list.short")
		cmd.Long = i18n.T("spawn.list.long")
	}

	// DNS command
	if cmd, _, err := rootCmd.Find([]string{"dns"}); err == nil && cmd != nil {
		cmd.Short = i18n.T("spawn.dns.short")
		cmd.Long = i18n.T("spawn.dns.long")
	}

	// Extend command
	if cmd, _, err := rootCmd.Find([]string{"extend"}); err == nil && cmd != nil {
		cmd.Short = i18n.T("spawn.extend.short")
		cmd.Long = i18n.T("spawn.extend.long")
	}

	// State command
	if cmd, _, err := rootCmd.Find([]string{"state"}); err == nil && cmd != nil {
		cmd.Short = i18n.T("spawn.state.short")
		cmd.Long = i18n.T("spawn.state.long")
	}
}
