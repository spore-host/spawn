#!/usr/bin/env python3
"""
Stage 2: Parallel variant calling using RDMA streaming

Receives BAMS3 chunks via ZeroMQ/RDMA, performs variant calling,
streams VCF chunks to downstream merger.

Architecture:
- Instance 0: Chunk distributor (reads S3, pushes to workers via PUB)
- Instances 1-7: Workers (SUB from distributor, call variants, PUSH to merger)
"""

import os
import sys
import json
import subprocess
import tempfile
import time
import zmq
from pathlib import Path
from multiprocessing import Process, Queue

sys.path.insert(0, '/opt/bams3')


def get_instance_role():
    """Determine if this is distributor (index 0) or worker."""
    # Read from peer discovery file
    with open('/etc/spawn/pipeline-peers.json') as f:
        peers = json.load(f)

    instance_index = peers['instance_index']
    total_instances = len(peers['stage_peers'])

    return 'distributor' if instance_index == 0 else 'worker', instance_index, total_instances


def run_distributor(num_workers):
    """Distributor: Read BAMS3 chunks from S3, broadcast to workers."""
    print(f"Starting distributor for {num_workers} workers")

    # Get upstream BAMS3 location
    upstream_prefix = os.environ.get('OUTPUT_PREFIX')  # From stage 1
    manifest_uri = f"{upstream_prefix}/chunk_manifest.json"

    # Download manifest
    manifest_file = '/tmp/chunk_manifest.json'
    subprocess.run(['aws', 's3', 'cp', manifest_uri, manifest_file], check=True)

    with open(manifest_file) as f:
        manifest = json.load(f)

    chunks = manifest['chunks']
    print(f"Loaded manifest with {len(chunks)} chunks")

    # Create ZeroMQ PUB socket (broadcast to all workers)
    context = zmq.Context()
    socket = context.socket(zmq.PUB)
    socket.bind("tcp://*:7500")

    # Give subscribers time to connect (slow joiner syndrome)
    time.sleep(2)

    print(f"Broadcasting {len(chunks)} chunks to workers...")

    for i, chunk in enumerate(chunks):
        # Download chunk from S3
        chunk_s3_uri = f"{upstream_prefix}/{chunk['file']}"
        chunk_local = f"/tmp/chunk_{i}.data"

        subprocess.run([
            'aws', 's3', 'cp',
            chunk_s3_uri,
            chunk_local,
            '--quiet'
        ], check=True)

        # Read chunk data
        with open(chunk_local, 'rb') as f:
            chunk_data = f.read()

        # Send chunk via ZeroMQ (topic: chunk coordinates for routing)
        topic = f"{chunk['chromosome']}:{chunk['start']}-{chunk['end']}"
        message = {
            'topic': topic,
            'chromosome': chunk['chromosome'],
            'start': chunk['start'],
            'end': chunk['end'],
            'data': chunk_data.hex()  # Hex encode for JSON safety
        }

        socket.send_json(message)

        os.unlink(chunk_local)

        if (i + 1) % 10 == 0:
            print(f"Distributed {i + 1}/{len(chunks)} chunks")

    # Send termination signal
    socket.send_json({'topic': 'DONE'})
    print("Distribution complete")

    socket.close()
    context.term()


def run_worker(instance_index, num_workers):
    """Worker: Receive chunks, call variants, send VCF to merger."""
    print(f"Starting worker {instance_index}/{num_workers}")

    # Get distributor IP from peer discovery
    with open('/etc/spawn/pipeline-peers.json') as f:
        peers = json.load(f)

    # Instance 0 is distributor
    distributor_ip = peers['stage_peers'][0]['private_ip']

    # Get downstream merger IPs
    downstream_peers = peers.get('downstream_stages', {})
    if not downstream_peers:
        print("Warning: No downstream stage found")
        merger_ips = []
    else:
        merger_stage = list(downstream_peers.keys())[0]
        merger_ips = [p['private_ip'] for p in downstream_peers[merger_stage]]

    print(f"Distributor: {distributor_ip}")
    print(f"Mergers: {merger_ips}")

    # Create ZeroMQ SUB socket (receive chunks from distributor)
    context = zmq.Context()
    sub_socket = context.socket(zmq.SUB)
    sub_socket.connect(f"tcp://{distributor_ip}:7500")
    sub_socket.setsockopt_string(zmq.SUBSCRIBE, '')  # Subscribe to all

    # Create ZeroMQ PUSH socket (send VCF chunks to merger)
    push_socket = context.socket(zmq.PUSH)
    for merger_ip in merger_ips:
        push_socket.connect(f"tcp://{merger_ip}:7501")

    # Download reference genome (cached across chunks)
    reference_uri = os.environ.get('REFERENCE')
    reference_local = '/data/reference.fasta'
    if not Path(reference_local).exists():
        print(f"Downloading reference: {reference_uri}")
        subprocess.run([
            'aws', 's3', 'cp',
            reference_uri,
            reference_local
        ], check=True)

    threads = int(os.environ.get('THREADS_PER_INSTANCE', '32'))
    chunks_processed = 0

    print(f"Worker ready, processing chunks with {threads} threads...")

    while True:
        # Receive chunk
        message = sub_socket.recv_json()

        if message.get('topic') == 'DONE':
            print(f"Worker {instance_index} received termination signal")
            break

        # Decode chunk data
        chunk_data = bytes.fromhex(message['data'])
        chrom = message['chromosome']
        start = message['start']
        end = message['end']

        # Write chunk to temp BAM file
        chunk_bam = f"/tmp/chunk_{chunks_processed}.bam"
        with open(chunk_bam, 'wb') as f:
            f.write(chunk_data)

        # Call variants using GATK HaplotypeCaller (or freebayes, bcftools, etc.)
        chunk_vcf = f"/tmp/chunk_{chunks_processed}.vcf"

        # Simplified variant calling (use GATK in production)
        subprocess.run([
            'bcftools', 'mpileup',
            '-f', reference_local,
            '-r', f"{chrom}:{start}-{end}",
            chunk_bam,
            '-Ou',
            '|', 'bcftools', 'call',
            '-mv', '-Oz',
            '-o', chunk_vcf
        ], shell=True, check=True)

        # Read VCF result
        with open(chunk_vcf, 'rb') as f:
            vcf_data = f.read()

        # Send VCF chunk to merger
        vcf_message = {
            'chromosome': chrom,
            'start': start,
            'end': end,
            'vcf_data': vcf_data.hex()
        }
        push_socket.send_json(vcf_message)

        # Cleanup
        os.unlink(chunk_bam)
        os.unlink(chunk_vcf)

        chunks_processed += 1
        if chunks_processed % 5 == 0:
            print(f"Worker {instance_index} processed {chunks_processed} chunks")

    print(f"Worker {instance_index} finished: {chunks_processed} chunks processed")

    sub_socket.close()
    push_socket.close()
    context.term()


def main():
    role, instance_index, total_instances = get_instance_role()

    print(f"=== Instance {instance_index}/{total_instances} - Role: {role} ===")

    if role == 'distributor':
        # Instance 0: Distribute chunks to workers
        num_workers = total_instances - 1  # Exclude self
        run_distributor(num_workers)
    else:
        # Instances 1-N: Process chunks
        run_worker(instance_index, total_instances)

    print("Variant calling stage complete")


if __name__ == '__main__':
    main()
