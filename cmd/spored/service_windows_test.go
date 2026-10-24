//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows/svc"
)

// `spored reload` on Windows sends ParamChange through the SCM (#1278). The
// service must accept it (or the SCM rejects the control) and route it to a
// reload, not ignore it.
func TestServiceControl_ParamChangeReloads(t *testing.T) {
	if serviceAccepts&svc.AcceptParamChange == 0 {
		t.Error("the service does not accept ParamChange, so the SCM rejects `spored reload`")
	}
	if got := serviceControlAction(svc.ParamChange); got != controlReload {
		t.Errorf("ParamChange -> %v, want controlReload", got)
	}
	for _, c := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		if got := serviceControlAction(c); got != controlStop {
			t.Errorf("%v -> %v, want controlStop", c, got)
		}
	}
	if got := serviceControlAction(svc.Interrogate); got != controlInterrogate {
		t.Errorf("Interrogate -> %v, want controlInterrogate", got)
	}
}

// requestReload is what the handler's controlReload does: it must reach the
// channel runDaemon forwards to the agent.
func TestRequestReload_ReachesNotifyReload(t *testing.T) {
	ch := notifyReload()
	requestReload()
	requestReload() // coalesces, never blocks the SCM handler
	select {
	case <-ch:
	default:
		t.Fatal("requestReload did not reach notifyReload's channel")
	}
}
