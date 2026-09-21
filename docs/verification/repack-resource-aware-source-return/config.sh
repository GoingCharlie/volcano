#!/usr/bin/env bash

# 只需在执行测试前修改本文件。其他脚本不要通过命令行环境变量传参。

# kubectl 当前 context 必须精确等于此值，避免误操作集群。
EXPECTED_KUBE_CONTEXT="REPLACE_WITH_KUBE_CONTEXT"

# 集群的 8 个 Ascend 节点。N0 是计划释放节点，N1 是计划接收节点。
NODES=(
  "REPLACE_WITH_NODE_0"
  "REPLACE_WITH_NODE_1"
  "REPLACE_WITH_NODE_2"
  "REPLACE_WITH_NODE_3"
  "REPLACE_WITH_NODE_4"
  "REPLACE_WITH_NODE_5"
  "REPLACE_WITH_NODE_6"
  "REPLACE_WITH_NODE_7"
)

TARGET_RESOURCE="huawei.com/ascend-1980"
EXPECTED_CARDS_PER_NODE=8
TEST_POD_CARDS=4

# 必须是包含修复提交 28e82d409 的已推送镜像。预检会与集群中 Engine 的实际镜像逐字比较。
EXPECTED_ENGINE_IMAGE="REPLACE_WITH_PATCHED_ENGINE_IMAGE"

# 改成真实集群可以拉取的基础镜像。容器只需提供 /bin/sh 和 sleep。
TEST_IMAGE="busybox:1.36.1"

SYSTEM_NAMESPACE="volcano-system"
TEST_NAMESPACE="repack-fix-real"
SCHEDULER_NAME="volcano"
QUEUE_NAME="default"
RUN_NAME="repack-resource-aware-source-return"
SOURCE_STS="repack-fix-source"
RECEIVER_STS="repack-fix-receiver"

# 测试专用污点/标签；预检会拒绝复用集群中已存在的同名键。
INITIAL_TAINT_KEY="repack-fix-test/initial-placement"
RECEIVER_HOLD_TAINT_KEY="repack-fix-test/receiver-hold"
SOURCE_NODE_LABEL_KEY="repack-fix-test/source-node"
SOURCE_NODE_LABEL_VALUE="true"

POD_READY_TIMEOUT_SECONDS=600
RUN_TIMEOUT_SECONDS=900

