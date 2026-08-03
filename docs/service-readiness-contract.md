# The service readiness contract

`spawn service` runs a long-lived HTTP service on an EC2 instance and forwards a
local port to it. This page is the spec for the one thing the service and spawn
share: a single JSON line on stdout.

Origin: spawn#409.

## Why a contract at all

The launcher has to know **which port to forward to**, and it cannot know it in
advance:

- A fixed port is wrong. Something else on the box may already hold it, and the
  service is the only party that finds out.
- Polling a guessed port is worse. "Is 8080 open yet?" cannot distinguish *my
  service is starting* from *someone else's service is already there*, and the
  answer arrives late either way.

So the service picks its own port — `:0`, let the OS assign — and **announces**
what it got. Nothing polls, nothing guesses, and the forward goes to the port the
service actually bound.

That single line is the whole interface. spawn does not know what the service
does, and the service does not know it was spawned. No workload is named anywhere
in spawn, and nothing in this contract is specific to one.

## The line

When the service begins serving, it prints exactly one newline-terminated JSON
object to **stdout**:

```json
{"event":"ready","addr":"127.0.0.1:54321","provenance":{"sourceHash":"9f3c…","commit":"abc1234"}}
```

| Field | Required | Meaning |
|-------|----------|---------|
| `event` | yes | Must be `"ready"`. The discriminator that separates this line from any other output. |
| `addr` | yes | The address the service **actually bound**, after `:0` resolution. `host:port`. |
| `token` | no | An access credential the service requires. Carried into the printed URL. |
| `provenance` | no | Opaque build identity, logged as "what exactly got spawned". Any shape. |

Unknown fields are ignored, not rejected — the contract is expected to grow
additively (`token` arrived after the first consumer shipped), and a reader that
failed on a field it didn't know would break on every such addition.

### Implementing it

The service takes a listen address on the command line. By default spawn appends:

```
--addr 127.0.0.1:0
```

Override the spelling with `--addr-args`, or pass an empty string to append
nothing at all (for a service that reads its address from a config file).

Bind the **loopback**, not `0.0.0.0`. The tunnel is the only intended path in; a
service on `0.0.0.0` is reachable by anything that can route to the instance,
which for an unauthenticated service is the whole story.

In Go, the shape is:

```go
ln, err := net.Listen("tcp", *addr) // *addr is "127.0.0.1:0"
if err != nil {
	log.Fatal(err)
}
// Announce the resolved address — NOT the requested one. ln.Addr() has the real
// port; *addr still says ":0".
json.NewEncoder(os.Stdout).Encode(map[string]any{
	"event": "ready",
	"addr":  ln.Addr().String(),
	"provenance": map[string]string{"commit": commit},
})
os.Stdout.Sync() // don't let the line sit in a buffer while the launcher waits
log.Fatal(http.Serve(ln, handler))
```

Three details that matter:

- **Announce after binding, not before.** The point of the line is the resolved
  port. A line printed before `Listen` either carries `:0` or a guess.
- **Flush.** A buffered line the launcher never sees is indistinguishable from a
  service that never started, and costs a whole boot timeout to find out.
- **stdout, one line.** Log to stderr. spawn scans stdout line by line and skips
  what doesn't parse, so ordinary log lines are harmless — but a readiness line
  split across writes is not a line.

Everything else on stdout is ignored, and stderr is passed through to the user's
terminal.

## What spawn does with it

1. Runs the command over an SSH session, so the child's stdout is a pipe here
   rather than a logfile on the box.
2. Scans stdout for a line that parses as a `ready` event.
3. **Verifies** the announced address (see below).
4. Opens `ssh -L <local>:<announced>` and prints the local URL, with the `token`
   appended when the service supplied one.
5. Holds until you interrupt it or the instance's lifetime ends, then terminates
   the instance.

## Verification, and why it is not a TCP dial

A readiness line is **forgeable**. `init()` runs before `main`, so a workload can
always print a well-formed line of its own choosing *first* — "first match wins"
is defeated by construction, and a launcher that trusts the first line forwards to
whatever port the workload named. So each candidate address is verified before it
is trusted, and candidates are tried in order: the real line wins.

Verification is an **HTTP request through the forward**, not a TCP connect,
because `ssh -L` binds and accepts on the local end *whether or not anything is
listening on the far side*. A successful connect to a forward proves only that ssh
is running. A forged address therefore passes a TCP dial and fails a request.

Any HTTP response counts, including `401` and `404`: the question is whether a
server is there, not whether the caller is authorized or the path exists.
Rejecting non-200s would reject every authenticated service as forged.

## Lifetime

Closing an SSH session does **not** reliably kill what it started — without a TTY
there is no SIGHUP — so a service that outlived its launcher would keep serving,
and keep billing, until the TTL. The remote command is therefore wrapped: it runs
the service in the background and blocks on stdin, and when the session drops
(stdin EOF) it sends `SIGTERM`, then `SIGKILL` if the service ignores it.

That is a request, not a guarantee. A network partition can strand the remote
process. **The instance's TTL is the guarantee** — which is why `spawn service`
launches through the same mandatory-TTL guard as every other launch path, and why
a run with no `--ttl` gets a 1h idle timeout rather than running unbounded.

## Failure diagnosis

A service that never becomes ready is indistinguishable from a slow one, so the
boot timeout (`--boot-timeout`, default 2m) is the only detector for that class.
When it fires — or when the service exits first — the error carries the evidence:
the command's stderr, plus the stdout lines that didn't match. Without those there
is nothing left to debug with.

The two causes are reported distinctly, because the remedies differ: a service
that *exited* has a bug or a bad argument, while one that *hung* may just need a
longer `--boot-timeout`.

## Testing without EC2

The contract is exercised end to end at $0. `pkg/service` runs a service as a
local child, reads its readiness line, forwards a loopback port to it, and drives
it through the forward — including the forgery cases above. The Substrate emulator
is control-plane only (instances never boot, no SSH), so this loopback path, not
the emulator, is what keeps the first real spawn from being the first test.
