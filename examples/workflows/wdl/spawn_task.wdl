version 1.0

## Spawn parameter sweep task for WDL workflows

task spawn_sweep {
  input {
    File params_file
    String wait_timeout = "2h"
    Int memory_gb = 4
    Int cpu = 2
  }

  command <<<
    set -e

    echo "Launching spawn parameter sweep..."
    spawn launch \
      --params ~{params_file} \
      --detach \
      --wait \
      --wait-timeout ~{wait_timeout} \
      --output-id sweep_id.txt

    SWEEP_ID=$(cat sweep_id.txt)
    echo "Sweep completed: $SWEEP_ID"

    # Get final status
    spawn status $SWEEP_ID --json > sweep_status.json

    # Output sweep ID
    echo -n "$SWEEP_ID" > sweep_id_output.txt
  >>>

  output {
    String sweep_id = read_string("sweep_id_output.txt")
    File sweep_status = "sweep_status.json"
  }

  runtime {
    docker: "scttfrdmn/spawn:latest"
    memory: "~{memory_gb} GB"
    cpu: cpu
  }
}

task process_results {
  input {
    String sweep_id
    File sweep_status
  }

  command <<<
    set -e

    echo "Processing sweep: ~{sweep_id}"

    # Parse status
    python3 <<CODE
import json

with open('~{sweep_status}') as f:
    status = json.load(f)

print(f"Status: {status['Status']}")
print(f"Total Parameters: {status['TotalParams']}")
print(f"Launched: {status['Launched']}")
print(f"Failed: {status['Failed']}")

if status['Failed'] > 0:
    print("WARNING: Some instances failed!")
CODE

    # Add custom processing logic
    echo "Processing complete!" > results.txt
  >>>

  output {
    File results = "results.txt"
  }

  runtime {
    docker: "python:3.11"
    memory: "8 GB"
    cpu: 4
  }
}

workflow spawn_parameter_sweep {
  input {
    File sweep_config
    String timeout = "2h"
  }

  call spawn_sweep {
    input:
      params_file = sweep_config,
      wait_timeout = timeout
  }

  call process_results {
    input:
      sweep_id = spawn_sweep.sweep_id,
      sweep_status = spawn_sweep.sweep_status
  }

  output {
    String completed_sweep_id = spawn_sweep.sweep_id
    File final_results = process_results.results
  }

  meta {
    description: "Launch spawn parameter sweep and process results"
    author: "Data Team"
  }
}
