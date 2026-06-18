#!/usr/bin/env nextflow

/*
 * Example Nextflow pipeline using the nf-spawn executor plugin.
 *
 * Each process below runs on its own ephemeral EC2 instance (sized per-process
 * via `ext.instanceType` in nextflow.config) and auto-terminates when the task
 * completes. You run `nextflow` locally; nf-spawn handles the instances.
 *
 * Config (see README.md / nextflow.config):
 *   plugins { id 'nf-spawn@0.8.0' }
 *   process { executor = 'spawn'; ext.instanceType = 't3.medium' }
 *   workDir = 's3://my-bucket/nextflow-work'
 */

params.input  = "data/*.txt"
params.outdir = "results"

// A cheap QC-style step — runs on the default instance type.
process PREPARE {
    input:
    path sample

    output:
    path "${sample.baseName}.prepared.txt"

    script:
    """
    # Real work would go here; this just demonstrates per-task execution.
    wc -l ${sample} > ${sample.baseName}.prepared.txt
    """
}

// A heavier step — give it a bigger instance via `withName: 'ANALYZE'` in config.
process ANALYZE {
    input:
    path prepared

    output:
    path "${prepared.baseName}.result.txt"

    script:
    """
    sort ${prepared} > ${prepared.baseName}.result.txt
    """
}

process COLLECT {
    publishDir params.outdir, mode: 'copy'

    input:
    path results

    output:
    path 'summary.txt'

    script:
    """
    cat ${results} > summary.txt
    """
}

workflow {
    samples = Channel.fromPath(params.input)
    PREPARE(samples) | ANALYZE | collect | COLLECT
}
