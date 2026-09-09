## `spawn connect`

*Aliases: ssh*

Connect to a spawn-managed instance via SSH.

Automatically finds your SSH key and connects. Falls back to AWS Session Manager if SSH is unavailable.

Examples:
  # Connect by instance ID
  spawn connect i-1234567890abcdef0

  # Connect by name
  spawn connect my-instance

```
spawn connect <instance-id> [-- <command>...] [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--key` |  | string |  | SSH private key path |
| `--no-start` |  | bool |  | Do not automatically start a stopped/hibernated instance |
| `--port` |  | int | `22` | SSH port |
| `--rdp-port` |  | int | `13389` | Windows --rdp --via-ssm: local port for the SSM RDP tunnel |
| `--rdp` |  | bool |  | Windows: open a Remote Desktop (RDP) connection (decrypts the Administrator password) |
| `--session-manager` |  | bool |  | Use AWS Session Manager instead of SSH |
| `--ssh` |  | bool |  | Windows: SSH in (as Administrator, over OpenSSH) instead of opening a PowerShell-over-SSM session — same SSH path as Linux |
| `--tty` | `-t` | bool |  | Allocate a pseudo-terminal (ssh -t) for the remote command. Line-buffers remote stdout so a long-running command's progress streams live instead of appearing only when it exits (useful when watching a multi-minute run, or so output isn't lost if the instance auto-terminates mid-run). Off by default: a PTY MERGES stdout and stderr and can mangle binary/structured output, so leave it off when piping such output through connect. |
| `--user` |  | string |  | SSH username (default: the instance's spawn:local-username tag, else ec2-user) |
| `--via-ssm` |  | bool |  | Windows --rdp: tunnel RDP over an SSM port-forwarding session instead of connecting to the public IP |

