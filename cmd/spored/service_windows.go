//go:build windows

package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceName = "spored"

// runService is invoked from main() when spored is launched by the Windows
// Service Control Manager. It satisfies svc.Handler.
type sporedService struct{}

func (s *sporedService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	// Run the lifecycle daemon in the background; it blocks in waitForShutdown
	// until requestStop() (below) is called.
	done := make(chan struct{})
	go func() {
		runDaemon()
		close(done)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case <-done:
			// Daemon exited on its own (e.g. self-terminate/stop path).
			changes <- svc.Status{State: svc.StopPending}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				requestStop() // unblock runDaemon → graceful cleanup
				// Give cleanup a moment to run (agent.Cleanup uses a 10s budget).
				select {
				case <-done:
				case <-time.After(20 * time.Second):
				}
				return false, 0
			}
		}
	}
}

// runWindowsService runs spored under the SCM. Called from main() when
// svc.IsWindowsService() is true.
func runWindowsService() error {
	return svc.Run(serviceName, &sporedService{})
}

// isWindowsService reports whether the process was started by the SCM.
func isWindowsService() bool {
	is, err := svc.IsWindowsService()
	return err == nil && is
}

// newServiceCmd adds `spored service install|uninstall|start|stop`, used by the
// PowerShell installer so it doesn't have to shell out to sc.exe with the exact
// recovery flags. Windows-only.
func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage the spored Windows service",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "install <exe-path>",
			Short: "Install spored as an auto-start Windows service",
			Args:  cobra.ExactArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return installService(args[0]) },
		},
		&cobra.Command{
			Use:   "uninstall",
			Short: "Remove the spored Windows service",
			Args:  cobra.NoArgs,
			RunE:  func(_ *cobra.Command, _ []string) error { return uninstallService() },
		},
		&cobra.Command{
			Use:   "start",
			Short: "Start the spored Windows service",
			Args:  cobra.NoArgs,
			RunE:  func(_ *cobra.Command, _ []string) error { return startService() },
		},
	)
	return cmd
}

func installService(exePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", serviceName)
	}
	s, err := m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName:  "Spawn Agent (spored)",
		Description:  "Instance self-monitoring for spawn-managed EC2 instances (TTL/idle/completion).",
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	})
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	// Auto-restart on failure (match the systemd Restart=on-failure behavior).
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, 86400); err != nil {
		// Non-fatal: the service still works without recovery actions.
		fmt.Printf("warning: could not set recovery actions: %v\n", err)
	}
	fmt.Printf("installed service %s (%s)\n", serviceName, exePath)
	return nil
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %s not installed: %w", serviceName, err)
	}
	defer s.Close()
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	fmt.Printf("uninstalled service %s\n", serviceName)
	return nil
}

func startService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	return s.Start()
}
