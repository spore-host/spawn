//go:build !windows

package main

import "github.com/spf13/cobra"

// runAsServiceIfManaged is a no-op on non-Windows platforms (there's no SCM).
func runAsServiceIfManaged() bool { return false }

// registerPlatformCommands adds no platform-specific subcommands on Unix.
func registerPlatformCommands(_ *cobra.Command) {}
