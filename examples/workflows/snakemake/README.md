# Snakemake on spawn

> ✅ **Snakemake is a first-class workflow integration on spawn**, via
> [**snakemake-executor-plugin-spawn**](https://github.com/spore-host/snakemake-executor-plugin-spawn)
> — the Snakemake analog of
> [nf-spawn](https://github.com/spore-host/nf-spawn) (Nextflow),
> [miniwdl-spawn](https://github.com/spore-host/miniwdl-spawn) (WDL), and
> [cwl-spawn](https://github.com/spore-host/cwl-spawn) (CWL). Each Snakemake job
> runs on a purpose-sized, ephemeral EC2 instance that auto-terminates, with
> inputs/outputs handled by Snakemake's native S3 storage plugin. Verified
> end-to-end.

## First-class: run every job on its own ephemeral instance

```bash
pip install snakemake-executor-plugin-spawn

snakemake \
  --executor spawn \
  --default-storage-provider s3 \
  --default-storage-prefix s3://my-bucket/snakemake-runs \
  --spawn-region us-east-1 \
  --spawn-ttl 4h \
  --jobs 8 \
  <target>
```

Each job is auto-sized from its `threads`/`resources.mem_mb` via `truffle`,
launched with a TTL and `--on-complete terminate`, and torn down when it
finishes — no cluster to run, no shared filesystem. Requires the `spawn` and
`truffle` CLIs on `PATH` and AWS credentials. See the plugin repo for details and
a runnable `examples/Snakefile`.

## Alternative pattern: drive a spawn sweep from a Snakefile

The `Snakefile` in this directory shows a *different* pattern — using
`spawn launch --params …` from inside Snakemake rules to run and wait on a spawn
parameter **sweep**, then analyze the results. This is the CLI-first orchestration
approach (spawn as a launcher invoked by rules), useful when you want a Snakefile
to coordinate a sweep rather than dispatch each rule to its own instance. It needs
only `pip install snakemake` and the `spawn` CLI:

```bash
snakemake --cores 1 --config sweep_file=config/sweep.yaml
```

Prefer the **first-class executor** (above) for the "each job → one ephemeral
instance" model; use this pattern for sweep coordination.

## See Also

- [**snakemake-executor-plugin-spawn**](https://github.com/spore-host/snakemake-executor-plugin-spawn) — the first-class executor
- [Snakemake Documentation](https://snakemake.readthedocs.io/)
- [Workflow Integration Examples](../README.md)
