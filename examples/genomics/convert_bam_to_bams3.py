#!/usr/bin/env python3
"""
Stage 1: Convert BAM → BAMS3 format

Reads BAM from S3, converts to BAMS3 chunked format, uploads to S3.
Uses BAMS3 converter from aws-direct-s3 project.
"""

import os
import sys
import json
import subprocess
import time
from pathlib import Path

# Add bams3 to path (assumes bams3 tools are installed)
sys.path.insert(0, '/opt/bams3')

import boto3


def download_bam(s3_uri, local_path):
    """Download BAM file from S3."""
    print(f"Downloading {s3_uri} to {local_path}...")
    start = time.time()

    # Use aws s3 cp for simple transfer
    subprocess.run([
        'aws', 's3', 'cp',
        s3_uri,
        local_path
    ], check=True)

    elapsed = time.time() - start
    size_gb = Path(local_path).stat().st_size / (1024**3)
    print(f"Downloaded {size_gb:.2f} GB in {elapsed:.1f}s ({size_gb/elapsed:.2f} GB/s)")


def convert_to_bams3(input_bam, output_dir, chunk_size_mb):
    """Convert BAM to BAMS3 format using chunked parallel approach."""
    print(f"Converting {input_bam} → BAMS3 (chunk size: {chunk_size_mb}MB)")
    start = time.time()

    # Use bams3_converter from aws-direct-s3 project
    subprocess.run([
        'python3', '/opt/bams3/bams3_converter.py',
        input_bam,
        output_dir,
        '--chunk-size', str(chunk_size_mb * 1_000_000),
        '--compression', 'lz4',
        '--parallel', '32'  # Use all cores
    ], check=True)

    elapsed = time.time() - start
    print(f"Conversion completed in {elapsed:.1f}s")


def upload_bams3_to_s3(local_dir, s3_prefix):
    """Upload BAMS3 dataset to S3."""
    print(f"Uploading BAMS3 dataset to {s3_prefix}...")
    start = time.time()

    # Use aws s3 sync for efficient upload
    subprocess.run([
        'aws', 's3', 'sync',
        local_dir,
        s3_prefix,
        '--quiet'
    ], check=True)

    elapsed = time.time() - start
    print(f"Upload completed in {elapsed:.1f}s")


def generate_chunk_manifest(bams3_dir, output_file):
    """Generate manifest of all chunks for downstream processing."""
    metadata_path = Path(bams3_dir) / '_metadata.json'
    with open(metadata_path) as f:
        metadata = json.load(f)

    # List all chunks
    chunks = []
    data_dir = Path(bams3_dir) / 'data'
    for chrom_dir in sorted(data_dir.iterdir()):
        if not chrom_dir.is_dir():
            continue

        chrom = chrom_dir.name
        for chunk_file in sorted(chrom_dir.glob('*.chunk')):
            # Parse chunk coordinates from filename
            coords = chunk_file.stem  # e.g., "000000000-001000000"
            start, end = map(int, coords.split('-'))

            chunks.append({
                'chromosome': chrom,
                'start': start,
                'end': end,
                'file': str(chunk_file.relative_to(bams3_dir)),
                'size_bytes': chunk_file.stat().st_size
            })

    manifest = {
        'dataset': metadata.get('dataset_name', 'unknown'),
        'total_reads': metadata.get('total_reads', 0),
        'total_chunks': len(chunks),
        'chunks': chunks
    }

    with open(output_file, 'w') as f:
        json.dump(manifest, f, indent=2)

    print(f"Generated manifest with {len(chunks)} chunks")
    return manifest


def main():
    # Get configuration from environment
    input_bam_uri = os.environ.get('INPUT_BAM')
    output_s3_prefix = os.environ.get('OUTPUT_PREFIX')
    chunk_size_mb = int(os.environ.get('CHUNK_SIZE_MB', '5'))

    if not input_bam_uri or not output_s3_prefix:
        print("Error: INPUT_BAM and OUTPUT_PREFIX environment variables required")
        sys.exit(1)

    # Working directories
    work_dir = Path('/data')
    work_dir.mkdir(exist_ok=True)

    local_bam = work_dir / 'input.bam'
    bams3_dir = work_dir / 'bams3'
    manifest_file = work_dir / 'chunk_manifest.json'

    # Step 1: Download BAM from S3
    download_bam(input_bam_uri, local_bam)

    # Step 2: Convert to BAMS3
    convert_to_bams3(local_bam, bams3_dir, chunk_size_mb)

    # Step 3: Generate chunk manifest for downstream processing
    manifest = generate_chunk_manifest(bams3_dir, manifest_file)

    # Step 4: Upload BAMS3 dataset to S3
    upload_bams3_to_s3(bams3_dir, output_s3_prefix)

    # Step 5: Upload manifest
    s3_manifest_uri = f"{output_s3_prefix}/chunk_manifest.json"
    subprocess.run([
        'aws', 's3', 'cp',
        str(manifest_file),
        s3_manifest_uri
    ], check=True)

    # Step 6: Clean up local BAM (keep BAMS3 for potential downstream use)
    local_bam.unlink()

    print("\n=== BAMS3 Conversion Summary ===")
    print(f"Input:  {input_bam_uri}")
    print(f"Output: {output_s3_prefix}")
    print(f"Chunks: {manifest['total_chunks']}")
    print(f"Reads:  {manifest['total_reads']:,}")
    print(f"Chunk size: {chunk_size_mb} MB")
    print("================================")


if __name__ == '__main__':
    main()
