#!/usr/bin/env python3
"""
Stage 3: VCF merge and finalization

Receives VCF chunks from parallel workers, merges in genomic order,
produces final compressed VCF with index.
"""

import os
import sys
import json
import subprocess
import time
import zmq
from pathlib import Path
from collections import defaultdict


def merge_vcf_chunks(chunk_files, output_vcf):
    """Merge VCF chunks in genomic coordinate order."""
    print(f"Merging {len(chunk_files)} VCF chunks...")

    # Sort chunks by chromosome and position
    def sort_key(f):
        # Parse filename: chr_start_end.vcf
        parts = f.stem.split('_')
        chrom = parts[0]
        start = int(parts[1])
        # Chromosome sort order
        if chrom.startswith('chr'):
            chrom = chrom[3:]
        try:
            chrom_num = int(chrom)
        except ValueError:
            chrom_num = 100 if chrom == 'X' else 101 if chrom == 'Y' else 102
        return (chrom_num, start)

    sorted_chunks = sorted(chunk_files, key=sort_key)

    # Use bcftools concat for efficient merging
    subprocess.run([
        'bcftools', 'concat',
        *[str(f) for f in sorted_chunks],
        '-Oz',  # Compress output
        '-o', output_vcf
    ], check=True)

    print(f"Merged VCF written to {output_vcf}")


def index_vcf(vcf_file):
    """Create tabix index for VCF."""
    print(f"Indexing {vcf_file}...")
    subprocess.run([
        'tabix', '-p', 'vcf', vcf_file
    ], check=True)
    print(f"Index created: {vcf_file}.tbi")


def main():
    print("=== VCF Merger Starting ===")

    # Get configuration
    output_vcf = os.environ.get('OUTPUT_VCF', '/data/results/merged.vcf.gz')
    compress = os.environ.get('COMPRESS', 'true').lower() == 'true'

    # Ensure output directory exists
    output_dir = Path(output_vcf).parent
    output_dir.mkdir(parents=True, exist_ok=True)

    # Get upstream worker IPs from peer discovery
    with open('/etc/spawn/pipeline-peers.json') as f:
        peers = json.load(f)

    upstream_stages = peers.get('upstream_stages', {})
    if not upstream_stages:
        print("Error: No upstream stage found")
        sys.exit(1)

    # Get worker instances (all except instance 0 which is distributor)
    upstream_stage = list(upstream_stages.keys())[0]
    worker_peers = [p for p in upstream_stages[upstream_stage] if p.get('index', 0) > 0]
    num_workers = len(worker_peers)

    print(f"Expecting VCF chunks from {num_workers} workers")

    # Create ZeroMQ PULL socket (receive from all workers)
    context = zmq.Context()
    socket = context.socket(zmq.PULL)
    socket.bind("tcp://*:7501")

    # Receive VCF chunks
    chunk_dir = Path('/tmp/vcf_chunks')
    chunk_dir.mkdir(exist_ok=True)

    chunks_by_region = defaultdict(list)
    chunks_received = 0
    workers_finished = 0

    print("Waiting for VCF chunks...")
    start_time = time.time()

    while workers_finished < num_workers:
        # Receive chunk
        message = socket.recv_json()

        if message.get('done'):
            workers_finished += 1
            print(f"Worker finished ({workers_finished}/{num_workers})")
            continue

        # Decode VCF data
        vcf_data = bytes.fromhex(message['vcf_data'])
        chrom = message['chromosome']
        start = message['start']
        end = message['end']

        # Write chunk to file
        chunk_file = chunk_dir / f"{chrom}_{start}_{end}.vcf.gz"
        with open(chunk_file, 'wb') as f:
            f.write(vcf_data)

        chunks_by_region[chrom].append(chunk_file)
        chunks_received += 1

        if chunks_received % 10 == 0:
            elapsed = time.time() - start_time
            rate = chunks_received / elapsed
            print(f"Received {chunks_received} chunks ({rate:.1f} chunks/sec)")

    elapsed = time.time() - start_time
    print(f"Received all {chunks_received} chunks in {elapsed:.1f}s")

    socket.close()
    context.term()

    # Merge all chunks
    all_chunks = []
    for chrom, chunks in sorted(chunks_by_region.items()):
        all_chunks.extend(chunks)

    merge_vcf_chunks(all_chunks, output_vcf)

    # Create index
    if compress:
        index_vcf(output_vcf)

    # Generate statistics
    stats_file = output_dir / 'merge_stats.json'
    stats = {
        'total_chunks': chunks_received,
        'chromosomes': list(chunks_by_region.keys()),
        'output_vcf': str(output_vcf),
        'output_size_mb': Path(output_vcf).stat().st_size / (1024**2),
        'processing_time_sec': elapsed
    }

    with open(stats_file, 'w') as f:
        json.dump(stats, f, indent=2)

    print("\n=== VCF Merge Summary ===")
    print(f"Chunks merged: {chunks_received}")
    print(f"Chromosomes: {len(chunks_by_region)}")
    print(f"Output: {output_vcf}")
    print(f"Size: {stats['output_size_mb']:.2f} MB")
    print(f"Time: {elapsed:.1f}s")
    print("=========================")


if __name__ == '__main__':
    main()
