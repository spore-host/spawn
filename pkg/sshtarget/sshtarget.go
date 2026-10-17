// Package sshtarget is the one place spawn starts an ssh or scp client, and the
// guard that refuses to point one at a host spawn must never connect to (#928).
//
// Why it exists: `spawn status` on a STOPPED instance built `ec2-user@` + an
// empty PublicIP and ran ssh on it. ssh resolves an empty host to the local
// machine, so the command connected to the operator's own sshd (::1), offered it
// every key in their agent, and failed with "Too many authentication failures".
// Nothing about that is specific to status: eleven call sites built a
// destination from instance.PublicIP, and every one of them did the same thing
// for an instance without one.
//
// The guard therefore sits at the exec, not at the string building. Command
// parses the argument vector the way ssh(1) and scp(1) do, finds every remote
// host in it — the destination, each scp remote operand, each -J jump host —
// and refuses to build the command when any of them is empty, loopback or
// unspecified. However a caller assembled its arguments, that is the last point
// before a connection, so it is where the check cannot be routed around.
package sshtarget

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNoHost is returned for an empty destination host: the shape a stopped or
// private-only instance's PublicIP produces.
var ErrNoHost = errors.New("no host to connect to (the instance has no public IP — is it stopped, or private-only?)")

// ErrLoopback is returned for a host that resolves to this machine.
var ErrLoopback = errors.New("refusing to connect to this machine (loopback/unspecified address)")

// CheckHost reports why host must not be connected to, or nil.
//
// Refused: the empty string; localhost and every *.localhost name (RFC 6761
// reserves the whole domain for loopback); the Debian ip6-localhost aliases;
// any loopback IP (127.0.0.0/8, ::1, including IPv4-mapped); the unspecified
// addresses 0.0.0.0 and ::, which connect to the local host; and anything
// beginning with "-", which ssh would parse as an option rather than a host.
func CheckHost(host string) error {
	h := strings.TrimSpace(host)
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if i := strings.IndexByte(h, '%'); i >= 0 { // IPv6 zone: fe80::1%en0
		h = h[:i]
	}
	if h == "" {
		return ErrNoHost
	}
	if strings.HasPrefix(h, "-") {
		return fmt.Errorf("refusing host %q: it would be read as an ssh option", host)
	}
	name := strings.ToLower(strings.TrimSuffix(h, "."))
	switch name {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return fmt.Errorf("%w: %q", ErrLoopback, host)
	}
	if strings.HasSuffix(name, ".localhost") {
		return fmt.Errorf("%w: %q", ErrLoopback, host)
	}
	if ip := net.ParseIP(h); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return fmt.Errorf("%w: %q", ErrLoopback, host)
	}
	return nil
}

// Destination returns "user@host" after checking host. user may be empty, in
// which case the bare host is returned.
func Destination(user, host string) (string, error) {
	if err := CheckHost(host); err != nil {
		return "", err
	}
	if user == "" {
		return host, nil
	}
	return user + "@" + host, nil
}

// Command builds the exec.Cmd for an ssh or scp invocation, after checking every
// remote host its arguments name. name is "ssh" or "scp" (a path to either is
// accepted); anything else is refused, so this cannot be used to launch an
// unrelated program past the guard.
func Command(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	hosts, err := RemoteHosts(name, args)
	if err != nil {
		return nil, err
	}
	for _, h := range hosts {
		if err := CheckHost(h); err != nil {
			return nil, err
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- name is restricted to ssh/scp by RemoteHosts above, and every remote host in args has been checked. exec runs no shell, so args reach the client as argv elements.
	return exec.CommandContext(ctx, name, args...), nil
}

// sshOptsWithArg are the ssh(1) flags that consume an argument.
const sshOptsWithArg = "BbcDEeFIiJLlmOoPpQRSWw"

// scpOptsWithArg are the scp(1) flags that consume an argument.
const scpOptsWithArg = "cDFiJloPSX"

// RemoteHosts returns every remote host an ssh or scp argument vector names:
// for ssh, the destination and any -J jump hosts; for scp, the host of every
// remote operand and any -J jump hosts. A host is returned even when it is
// empty — "ec2-user@" yields "" — because the empty host is exactly what the
// caller must be told about.
//
// It errors when the vector names no remote host at all (ssh with no
// destination, scp with no remote operand): spawn never means either.
func RemoteHosts(name string, args []string) ([]string, error) {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(name)), ".exe")
	var withArg string
	switch base {
	case "ssh":
		withArg = sshOptsWithArg
	case "scp":
		withArg = scpOptsWithArg
	default:
		return nil, fmt.Errorf("sshtarget: %q is not an ssh or scp client", name)
	}

	var hosts []string
	var operands []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			operands = append(operands, a)
			if base == "ssh" {
				// ssh stops parsing options at the destination; the rest is the
				// remote command, which is not ours to interpret.
				operands = append(operands, args[i+1:]...)
				break
			}
			continue
		}
		// An option cluster: -tt, -i key, -ikey, -oFoo=bar.
		for j := 1; j < len(a); j++ {
			if !strings.ContainsRune(withArg, rune(a[j])) {
				continue
			}
			val := a[j+1:]
			if val == "" {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("sshtarget: option -%c has no value", a[j])
				}
				i++
				val = args[i]
			}
			if a[j] == 'J' {
				for _, jump := range strings.Split(val, ",") {
					hosts = append(hosts, destinationHost(jump))
				}
			}
			break
		}
	}

	switch base {
	case "ssh":
		if len(operands) == 0 {
			return nil, ErrNoHost
		}
		hosts = append(hosts, destinationHost(operands[0]))
	case "scp":
		remote := 0
		for _, op := range operands {
			if h, ok := scpOperandHost(op); ok {
				hosts = append(hosts, h)
				remote++
			}
		}
		if remote == 0 {
			return nil, fmt.Errorf("sshtarget: scp has no remote operand: %w", ErrNoHost)
		}
	}
	return hosts, nil
}

// destinationHost extracts the host from an ssh destination:
// [user@]host, or ssh://[user@]host[:port].
func destinationHost(dest string) string {
	if strings.HasPrefix(strings.ToLower(dest), "ssh://") {
		if u, err := url.Parse(dest); err == nil {
			return u.Hostname()
		}
		dest = dest[len("ssh://"):]
	}
	if i := strings.LastIndexByte(dest, '@'); i >= 0 {
		dest = dest[i+1:]
	}
	return dest
}

// scpOperandHost reports the host of a remote scp operand —
// [user@]host:[path], [user@][v6]:[path] or scp://[user@]host[:port][/path] —
// and false for a local path. As in scp(1), a colon after a slash is part of a
// local path, not a host separator.
func scpOperandHost(op string) (string, bool) {
	if strings.HasPrefix(strings.ToLower(op), "scp://") {
		if u, err := url.Parse(op); err == nil {
			return u.Hostname(), true
		}
		return "", true
	}
	rest := op
	if i := strings.LastIndexByte(rest, '@'); i >= 0 && !strings.Contains(rest[:i], "/") {
		rest = rest[i+1:]
	}
	if strings.HasPrefix(rest, "[") {
		if end := strings.IndexByte(rest, ']'); end > 0 && end+1 < len(rest) && rest[end+1] == ':' {
			return rest[1:end], true
		}
	}
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return "", false
	}
	if slash := strings.IndexByte(rest, '/'); slash >= 0 && slash < colon {
		return "", false
	}
	return rest[:colon], true
}
