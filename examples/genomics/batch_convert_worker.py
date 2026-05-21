#!/usr/bin/env python3
"""
Batch conversion worker: Converts BAMs to BAMS3 in parallel

Each worker instance picks BAMs from the manifest and converts them.
Uses round-robin distribution based on instance index.
"""

import os
import sys
import json
import subprocess
import time
from pathlib import Path

sys.path.insert(0, '/opt/bams3')


def load_conversion_manifest():
    """Load the manifest of BAMs to convert."""
    manifest_file = '/data/conversion_manifest.json'

    if not Path(manifest_file).exists():
        print("Error: Conversion manifest not found")
        sys.exit(1)

    with open(manifest_file) as f:
        return json.load(f)


def get_my_assignments(manifest, instance_index, total_instances):
    """Determine which BAMs this instance should convert."""
    all_bams = manifest['bams']

    # Round-robin assignment
    my_bams = []
    for i, bam in enumerate(all_bams):
        if i % total_instances == instance_index:
            my_bams.append(bam)

    return my_bams


def convert_bam_to_bams3(bam_s3_uri, output_s3_prefix, chunk_size_mb, threads):
    """Convert a single BAM to BAMS3."""
    sample_name = Path(bam_s3_uri).stem

    print(f"Converting {sample_name}...")
    start_time = time.time()

    # Download BAM
    local_bam = f"/data/{sample_name}.bam"
    print(f"  Downloading {bam_s3_uri}...")
    subprocess.run([
        'aws', 's3', 'cp',
        bam_s3_uri,
        local_bam,
        '--quiet'
    ], check=True)

    download_time = time.time() - start_time
    file_size_gb = Path(local_bam).stat().st_size / (1024**3)

    # Convert to BAMS3
    local_bams3 = f"/data/{sample_name}.bams3"
    conversion_start = time.time()

    print(f"  Converting to BAMS3 ({file_size_gb:.2f} GB)...")
    subprocess.run([
        'python3', '/opt/bams3/bams3_converter.py',
        local_bam,
        local_bams3,
        '--chunk-size', str(chunk_size_mb * 1_000_000),
        '--compression', os.environ.get('COMPRESSION', 'lz4'),
        '--parallel', str(threads)
    ], check=True)

    conversion_time = time.time() - conversion_start

    # Upload BAMS3
    upload_start = time.time()
    output_uri = f"{output_s3_prefix}/{sample_name}.bams3"

    print(f"  Uploading to {output_uri}...")
    subprocess.run([
        'aws', 's3', 'sync',
        local_bams3,
        output_uri,
        '--quiet'
    ], check=True)

    upload_time = time.time() - upload_start
    total_time = time.time() - start_time

    # Calculate BAMS3 size
    bams3_size_gb = sum(
        f.stat().st_size
        for f in Path(local_bams3).rglob('*')
        if f.is_file()
    ) / (1024**3)

    compression_ratio = file_size_gb / bams3_size_gb if bams3_size_gb > 0 else 0

    # Cleanup
    subprocess.run(['rm', '-rf', local_bam, local_bams3])

    return {
        'sample': sample_name,
        'input_uri': bam_s3_uri,
        'output_uri': output_uri,
        'input_size_gb': file_size_gb,
        'output_size_gb': bams3_size_gb,
        'compression_ratio': compression_ratio,
        'download_time_sec': download_time,
        'conversion_time_sec': conversion_time,
        'upload_time_sec': upload_time,
        'total_time_sec': total_time,
        'throughput_mbps': (file_size_gb * 8192) / total_time if total_time > 0 else 0
    }


def main():
    # Get instance info from peer discovery
    with open('/etc/spawn/pipeline-peers.json') as f:
        peers = json.load(f)

    instance_index = peers['instance_index']
    total_instances = len(peers['stage_peers'])

    print(f"=== Worker {instance_index}/{total_instances} Starting ===")

    # Load manifest
    manifest = load_conversion_manifest()
    total_bams = len(manifest['bams'])

    print(f"Total BAMs in manifest: {total_bams}")

    # Get my assignments
    my_bams = get_my_assignments(manifest, instance_index, total_instances)

    print(f"My assignments: {len(my_bams)} BAMs")

    if not my_bams:
        print("No BAMs assigned to this worker")
        return

    # Get configuration
    chunk_size_mb = int(os.environ.get('CHUNK_SIZE_MB', '5'))
    threads = int(os.environ.get('THREADS', '8'))
    output_bucket = manifest.get('output_bucket')
    output_prefix = manifest.get('output_prefix')

    output_s3_prefix = f"s3://{output_bucket}/{output_prefix}"

    # Convert each assigned BAM
    results = []
    for i, bam in enumerate(my_bams):
        print(f"\n--- Converting {i+1}/{len(my_bams)}: {bam['name']} ---")

        try:
            result = convert_bam_to_bams3(
                bam['s3_uri'],
                output_s3_prefix,
                chunk_size_mb,
                threads
            )
            result['status'] = 'success'
            results.append(result)

            print(f"✓ Completed: {result['sample']}")
            print(f"  Size: {result['input_size_gb']:.2f} GB → {result['output_size_gb']:.2f} GB")
            print(f"  Compression: {result['compression_ratio']:.2f}x")
            print(f"  Time: {result['total_time_sec']:.1f}s ({result['throughput_mbps']:.1f} Mbps)")

        except Exception as e:
            print(f"✗ Failed: {bam['name']}: {e}")
            results.append({
                'sample': bam['name'],
                'input_uri': bam['s3_uri'],
                'status': 'failed',
                'error': str(e)
            })

    # Write conversion log
    log_file = '/data/conversion_log.json'
    with open(log_file, 'w') as f:
        json.dump({
            'worker_id': instance_index,
            'total_assigned': len(my_bams),
            'successful': sum(1 for r in results if r['status'] == 'success'),
            'failed': sum(1 for r in results if r['status'] == 'failed'),
            'conversions': results
        }, f, indent=2)

    # Print summary
    successful = sum(1 for r in results if r['status'] == 'success')
    total_time = sum(r.get('total_time_sec', 0) for r in results if r['status'] == 'success')
    total_input_gb = sum(r.get('input_size_gb', 0) for r in results if r['status'] == 'success')

    print(f"\n=== Worker {instance_index} Summary ===")
    print(f"Successful: {successful}/{len(my_bams)}")
    print(f"Total data: {total_input_gb:.2f} GB")
    print(f"Total time: {total_time:.1f}s")
    if successful > 0:
        print(f"Avg throughput: {(total_input_gb * 8192) / total_time:.1f} Mbps")
    print("===========================")


if __name__ == '__main__':
    main()
