# Nextflow Integration (nf-spawn)

Nextflow is spawn's **only first-class workflow integration**, via the
[**nf-spawn**](https://github.com/spore-host/nf-spawn) executor plugin. With it,
each Nextflow process runs on its own ephemeral, purpose-sized EC2 instance that
auto-terminates when the task completes — no cluster, no AWS Batch queue to
manage.

## How it works

You run `nextflow` as usual (on your laptop or a head node). The plugin
dispatches each task with `spawn launch`, the instance runs the task script,
signals completion via `spored complete`, and terminates. Nextflow polls
`spawn status` to detect completion.

```
process → spawn launch nf-<hash> --on-complete terminate → runs task → spored complete → terminates
```

## Prerequisites

- [`spawn`](https://github.com/spore-host/spawn) installed and on `PATH`
- AWS credentials configured
- Nextflow 26.04.x (the version nf-spawn tracks)
- An S3 bucket for the work directory (required for multi-instance runs)

## Configuration

Enable the plugin and the `spawn` executor in `nextflow.config`:

```groovy
plugins {
    id 'nf-spawn@0.8.0'
}

process {
    executor = 'spawn'

    // Default instance type for all processes
    ext.instanceType = 't3.medium'
    ext.region       = 'us-east-1'
    ext.ttl          = '2h'      // safety backstop — instances self-terminate

    // Per-process overrides
    withName: 'HEAVY_STEP' {
        ext.instanceType = 'c7g.4xlarge'
        ext.spot         = true
    }
}

// S3 work directory (required for multi-instance pipelines)
workDir = 's3://my-bucket/nextflow-work'
```

See the [nf-spawn README](https://github.com/spore-host/nf-spawn#per-process-ext-options)
for the full set of per-process `ext.*` options (`spot`, `ami`, `volumes`,
`fsx`, `efs`, `packages`, `setup`, …).

## Running

```bash
# Run the example pipeline
nextflow run spawn_example.nf

# Resume after a failure (re-uses completed tasks)
nextflow run spawn_example.nf -resume
```

## See Also

- [nf-spawn](https://github.com/spore-host/nf-spawn) — the executor plugin
- [genomics-nextflow/nf-core-sarek](../../genomics-nextflow/nf-core-sarek/) — running nf-core/sarek on spawn
- [Nextflow Documentation](https://www.nextflow.io/docs/latest/)
