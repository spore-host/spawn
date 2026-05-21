#!/usr/bin/env nextflow

/*
 * Nextflow pipeline for spawn parameter sweep execution
 */

params.sweep_file = "sweep.yaml"
params.sweep_timeout = "2h"
params.output_dir = "results"

process launchSweep {
    output:
    env SWEEP_ID

    script:
    """
    spawn launch --params ${params.sweep_file} --detach --output-id sweep_id.txt
    export SWEEP_ID=\$(cat sweep_id.txt)
    echo "Launched sweep: \$SWEEP_ID" >&2
    """
}

process waitSweep {
    input:
    env SWEEP_ID

    output:
    env SWEEP_ID

    script:
    """
    echo "Waiting for sweep: \$SWEEP_ID" >&2
    spawn launch --params ${params.sweep_file} --detach --wait --wait-timeout ${params.sweep_timeout}
    """
}

process checkStatus {
    input:
    env SWEEP_ID

    output:
    path 'status.json'

    script:
    """
    spawn status \$SWEEP_ID --json > status.json
    """
}

process processResults {
    publishDir params.output_dir, mode: 'copy'

    input:
    path status_json

    output:
    path 'results.txt'

    script:
    """
    echo "Processing sweep results..." > results.txt
    cat ${status_json} >> results.txt
    # Add custom processing logic here
    """
}

workflow {
    sweep_ch = launchSweep()
    wait_ch = waitSweep(sweep_ch)
    status_ch = checkStatus(wait_ch)
    processResults(status_ch)
}
