//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/spore-host/spawn/pkg/agent"
)

// reloadReqCh carries the SCM ParamChange control (service_windows.go) to the
// daemon's monitor loop (#1278), the Windows counterpart of SIGHUP.
var reloadReqCh = make(chan struct{}, 1)

// requestReload queues a reload request without blocking the SCM handler; a
// request already pending covers this one.
func requestReload() {
	select {
	case reloadReqCh <- struct{}{}:
	default:
	}
}

func notifyReload() <-chan struct{} { return reloadReqCh }

func newDaemonReloader() daemonReloader {
	return daemonReloader{
		markerPath: agent.ReloadMarkerPath(),
		daemonPID:  serviceProcessID,
		signal: func(_ context.Context, _ int) error {
			return controlService(svc.ParamChange)
		},
		sleep:    sleepFor,
		poll:     reloadPollInterval,
		attempts: reloadPollAttempts,
	}
}

// serviceProcessID is the running spored service's process id, 0 when it is
// stopped or not installed.
func serviceProcessID(_ context.Context) (int, error) {
	m, err := mgr.Connect()
	if err != nil {
		return 0, fmt.Errorf("connect to service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return 0, nil
		}
		return 0, fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return 0, fmt.Errorf("query service: %w", err)
	}
	if st.State != svc.Running {
		return 0, nil
	}
	return int(st.ProcessId), nil
}

func controlService(c svc.Cmd) error {
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
	_, err = s.Control(c)
	return err
}
