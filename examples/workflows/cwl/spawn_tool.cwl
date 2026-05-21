cwlVersion: v1.2
class: CommandLineTool
doc: "Launch spawn parameter sweep and wait for completion"

baseCommand: [spawn, launch]

requirements:
  DockerRequirement:
    dockerPull: scttfrdmn/spawn:latest

inputs:
  params_file:
    type: File
    doc: "Parameter sweep YAML/JSON file"
    inputBinding:
      prefix: --params

  detach:
    type: boolean
    default: true
    inputBinding:
      prefix: --detach

  wait:
    type: boolean
    default: true
    inputBinding:
      prefix: --wait

  wait_timeout:
    type: string
    default: "2h"
    inputBinding:
      prefix: --wait-timeout

  output_id_file:
    type: string
    default: "sweep_id.txt"
    inputBinding:
      prefix: --output-id

outputs:
  sweep_id:
    type: File
    doc: "File containing the sweep ID"
    outputBinding:
      glob: $(inputs.output_id_file)

stdout: spawn_output.txt
stderr: spawn_errors.txt
