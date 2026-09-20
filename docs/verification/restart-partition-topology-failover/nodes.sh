#!/usr/bin/env bash

# Edit only this file before running the verification scripts.
# Each value must be the exact Kubernetes Node metadata.name.
N0="replace-with-node-0"
N1="replace-with-node-1"
N2="replace-with-node-2"
N3="replace-with-node-3"
N4="replace-with-node-4"
N5="replace-with-node-5"
N6="replace-with-node-6"
N7="replace-with-node-7"

# Change these values only when the Volcano installation uses non-default names.
VOLCANO_NAMESPACE="volcano-system"
VOLCANO_RELEASE="volcano"
JOB_NAMESPACE="default"
QUEUE_NAME="default"
SCHEDULER_NAME="volcano"

# Ascend resource settings for this verification.
ACCELERATOR_RESOURCE="huawei.com/ascend-1980"
CARDS_PER_NODE="8"
CARDS_PER_POD="8"
TEST_IMAGE="busybox:1.36.1"

# Test output is written here and ignored by the adjacent .gitignore file.
EVIDENCE_ROOT="${SCRIPT_DIR}/evidence"
