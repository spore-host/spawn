# Pipeline examples

Definitions for `spawn pipeline validate | graph | launch`:

| file | shows |
|---|---|
| `ml-training.json` | S3 handoff into and out of a 4-instance EFA stage |
| `fanout-fanin.json` | one stage fanned out to three, then gathered by one |
| `streaming-demo.json` | TCP streaming between stages (see `../streaming/README.md`) |
| `video-streaming.json` | a streaming chain ending in an S3 output |

## Where S3 data goes

A stage's `data_output` (`"mode": "s3"`) uploads the files its `paths` match
after the command succeeds. A downstream stage's `data_input` downloads them
into its `dest_path` before its command starts.

**A file keeps its path relative to the directory part of the pattern that
matched it.** The directory part is everything before the first path segment
containing `*`, `?` or `[`. For a pattern with none (a single file), it is the
file's directory. Absolute and relative patterns behave the same way.

| `data_output` path | file on the source instance | arrives as |
|---|---|---|
| `/data/processed/*` | `/data/processed/a.csv` | `<dest_path>/a.csv` |
| `/data/*/part.csv` | `/data/2026/part.csv` | `<dest_path>/2026/part.csv` |
| `/logs/training.log` | `/logs/training.log` | `<dest_path>/training.log` |
| `out/*.csv` (relative to the stage's working directory) | `out/a.csv` | `<dest_path>/a.csv` |

So in `ml-training.json`, `preprocess` writes `/data/processed/a.csv` and `train`
receives it as `/data/input/a.csv`. Before #944, an absolute pattern kept the
whole path, and the file arrived as `/data/input/data/processed/a.csv`.

**A stage with `instance_count` > 1 namespaces its outputs by instance index.**
Every instance runs the same command and uploads the same paths. Each one's
files are kept under its index, starting at 0, and arrive downstream under it:

| source stage | arrives as |
|---|---|
| `instance_count: 1` | `<dest_path>/<file>` |
| `instance_count: 4` | `<dest_path>/0/<file>` … `<dest_path>/3/<file>` |

So `evaluate` in `ml-training.json` reads `train`'s first model as
`/models/0/best.h5`. Before #944, the four instances overwrote each other's
`best.h5` and the last upload won. A stage's own index is in
`$SPAWN_INSTANCE_INDEX`.

**`data_input.pattern`** selects files from the source stage. It is matched
against the same relative path, for a multi-instance source the part after
`<index>/`, so `"chunk-1-*"` or `"*.csv"`. It is never absolute: `validate`
rejects an absolute pattern, which could never match.

**Collisions fail the stage instead of overwriting.** This covers two paths of
one stage that map to the same name, and two `source_stages` that both deliver
a file to the same local path. Give such outputs distinct names, as the three
`process-chunk-N` stages in `fanout-fanin.json` do.

### S3 layout

Under the pipeline's `s3_bucket`, each run (#943) has its own prefix:

```
<s3_prefix>/runs/<run-id>/
  config/<pipeline_id>.json                      the definition the stages read
  stages/<stage>/output/<file>                   instance_count: 1
  stages/<stage>/output/<instance-index>/<file>  instance_count > 1
  stages/<stage>/<instance-index>/COMPLETE       written after the outputs upload
  stages/<stage>/<instance-index>/FAILED
```

`spawn pipeline collect <pipeline-id>` downloads the run's `stages/` tree.
