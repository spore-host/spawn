//go:build windows

package main

import (
	"log"

	"github.com/spf13/cobra"
)

// runAsServiceIfManaged runs spored under the Windows Service Control Manager
// when the SCM launched the process, and reports true so main() returns. When
// run interactively (or for a CLI subcommand) it returns false and normal CLI
// dispatch proceeds.
func runAsServiceIfManaged() bool {
	if !isWindowsService() {
		return false
	}
	if err := runWindowsService(); err != nil {
		log.Printf("windows service error: %v", err)
	}
	return true
}

// registerPlatformCommands adds the Windows `service` management subcommand.
func registerPlatformCommands(root *cobra.Command) {
	root.AddCommand(newServiceCmd())
}
