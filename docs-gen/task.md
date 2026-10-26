## `spawn task`

Work with a single launched instance as a "task".

  spawn task run --spec &lt;file&gt;             launch a task from a TaskSpec (stage, run, completion record)
  spawn task status &lt;task-id&gt;              read a task's durable completion record from S3
  spawn task diagnose &lt;name|instance-id&gt;   one-screen summary + likely cause

```
spawn task
```

### `spawn task diagnose`

Summarize an instance's state, cost, and likely failure cause

```
spawn task diagnose <name|instance-id>
```

### `spawn task run`

Run a task described by a TaskSpec JSON file (the shared workflow-adapter
contract, spawn#386).

Sizing picks the cheapest type that fits, and on a price TIE the smallest that
fits — which matters on a family like hpc7g where every size costs the same
because you rent the socket rather than the cores, so a 16-vCPU request returns
hpc7g.4xlarge and not the 64-vCPU hpc7g.16xlarge (spawn#610).

To choose the type yourself, set resources.instance_type in the spec: that pins
it exactly and skips candidate search and price ranking altogether (cpu,
memory_gib, architecture and families are then ignored). That is the way to sweep
several sizes of one family deliberately, since cpu/memory can't distinguish
equal-spec sizes. resources.families narrows sizing to an allow-list without
pinning a single type.

Once the type is chosen, spawn launches an ephemeral instance that stages inputs
from S3, runs the command, stages outputs back, and writes a durable completion
record to
s3://spawn-results-&lt;account&gt;-&lt;region&gt;/tasks/&lt;task_id&gt;/completion.json — the
signal workflow adapters poll. The instance self-terminates on completion (TTL +
on_complete).

If spec.container is set, the command runs inside that image (Docker is installed
on demand; a private-ECR image is pulled with an ecr:ReadOnly grant, GPUs passed
with --gpus all). Otherwise it runs on the host. A container task is sized to at
least 2 GiB of memory even when resources.memory_gib is lower or unset, because
the Docker install is OOM-killed on smaller boxes; a pinned
resources.instance_type is honoured as given, with a warning if it is below that.
If Docker cannot be installed or its daemon does not start, the record reports
retry_class instance_health (retryable), not app_error.

A container gets Docker's default 64 MB /dev/shm, which many-rank MPI (Open MPI's
shared-memory transport), PyTorch DataLoader workers and Ray outgrow: an Open MPI
run with 64 ranks on one box dies with SIGBUS in its first collective. Set
resources.shm_gib to raise it (passed as docker run --shm-size=&lt;n&gt;g). It must be
a positive number of GiB, needs a container, and may not exceed the instance's
memory: memory_gib with headroom (or the 2 GiB container floor) for a sized spec,
the type's memory for a pinned instance_type. /dev/shm is RAM-backed, so size
memory_gib to cover it as well as the workload.

What gets bind-mounted, and as whom: spawn pre-creates the parent directory of
every inputs[].destination and outputs[].source — plus any EFS/FSx/volume mount
point from placement — as the instance user, and bind-mounts exactly those paths
into the container. The container then runs as that same uid:gid rather than the
image's declared USER (spawn#555), so the staged directories are writable
whatever the image happens to declare; every staged input is also chown'd to that
uid so it can be deleted as well as read (spawn#565). Practical upshot: any
absolute path works, the image's USER is irrelevant, and you do not need to keep
staged paths flat in /tmp. A manifest path ending in "/" is a directory and is
staged recursively in both directions (spawn#564). A directory input that copies
no objects (its prefix is empty or missing, often because the task that writes
it failed) fails stage-in as staging_error; set allow_empty: true on that input
if an empty directory is expected (spawn#1207).

A container's /tmp is its own tmpfs (docker run --tmpfs), RAM-backed and capped
at half of instance memory like the host's AL2023 /tmp, so scratch written there
uses memory, not the root disk; staged directories under /tmp are mounted on top
of it. The exception is a manifest path directly in /tmp (such as /tmp/run.sh):
then the host's /tmp is bind-mounted instead, as before (spawn#1270). command.log
says which.

A workflow engine that starts its own task containers (Cromwell's local
backend, miniwdl, Nextflow with docker.enabled) can run as a container task with
container_docker_socket: true. It bind-mounts the host's /var/run/docker.sock
into the container and adds the socket's group; --user is unchanged. This is
ROOT-EQUIVALENT ON THE INSTANCE: anything that can reach dockerd can start a
privileged container that mounts the host's /, so set it only for an image you
trust. It needs container, and is refused for a pooled task, where one task
could control the others on the shared worker. The containers the engine starts
are siblings on the host, so paths it hands them must be host paths: use the
staged or placement-mounted directories, not the engine container's own /tmp.
The provenance record notes the socket, and such a run is never proven
regenerable, because the engine's images are not recorded (spawn#1231).

Staging space is NOT resources.disk_gib unless you stage onto the root
filesystem. disk_gib sizes the root EBS volume, which is where an ordinary path
like /work lives — so that is the knob for a large staged input. /tmp is
different: on AL2023 it is a tmpfs, RAM-backed and capped near half of instance
memory by default, so staging a 40 GiB index into /tmp fails on a 32 GiB box no
matter how large disk_gib is. Stage big inputs to a non-/tmp path and size
disk_gib, or keep /tmp and size memory_gib for the staging footprint — the
latter tends to pick the instance FAMILY rather than just its size, so the two
are not interchangeable on cost.

The launch AMI is auto-selected from the sized instance type: a GPU family (g5,
g6, p4/p5, …) gets the AL2023 NVIDIA DLAMI so --gpus all lands on a host with a
driver (spawn#601), and everything else gets the standard AL2023 for the type's
architecture. Pin a specific AMI with --ami (or placement.ami in the spec); an
explicit AMI always wins over auto-selection.

A task launches into the region's default VPC unless the spec names a subnet
with placement.subnet_id, as --subnet-id does for spawn launch. In a region with
no default VPC, usual for Control Tower and Landing Zone Accelerator accounts, a
spec without one fails before launching and lists subnets to use (spawn#1267).

Re-running the same task_id is safe with --wait: the results of the previous
attempt are cleared at launch, and every run stamps a run_id into its completion
record, so --wait ignores any record that isn't from the run it just launched
instead of reporting an earlier attempt's verdict (#608).

--dry-run sizes and prints the plan without launching.

```
spawn task run --spec <file> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--ami` |  | string |  | Pin a specific AMI (overrides auto-selection and placement.ami); empty = auto-select from the sized instance type (GPU DLAMI for GPU families) |
| `--dry-run` |  | bool |  | Size and preview the task without launching |
| `--poll-interval` |  | duration | `15s` | How often to poll for completion when --wait is set |
| `--region` |  | string |  | Region to size against (default: the configured AWS region) |
| `--spec` |  | string |  | Path to a TaskSpec JSON file (required) |
| `--wait` |  | bool |  | Block until the task's completion record appears, then exit with its exit code; a task whose state is not completed exits 1 even if its command exited 0 (e.g. a declared output failed to upload) |

### `spawn task status`

Read the completion record a 'spawn task run' task wrote to
s3://spawn-results-&lt;account&gt;-&lt;region&gt;/tasks/&lt;task-id&gt;/completion.json.

If the record isn't there, status asks EC2 about the task's instance: a live
instance means the task is still running; a terminated one with no record is
reported as terminated, never as running, with the outcome of spored's
terminal flush (the spawn:flush-status tag) while EC2 still lists the
instance, about an hour after it ends. A record is followed by a Flush: line.
With --check-complete, exit codes mirror 'spawn status': 0=completed,
1=failed (including terminated without a record), 2=running, 3=error
(including no record and no instance found).

```
spawn task status <task-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--check-complete` |  | bool |  | Exit 0=completed, 1=failed, 2=running, 3=error instead of printing |
| `--region` |  | string |  | Region the task ran in (default: the configured AWS region) |
| `--results-prefix` |  | string |  | s3://bucket/prefix the task's records were written under; required if the spec set results_prefix (default: spawn's own spawn-results-&lt;account&gt;-&lt;region&gt;/tasks) |

