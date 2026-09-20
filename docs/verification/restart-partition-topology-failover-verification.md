# Volcano RestartPartition 分组 Pod 故障迁移验证报告

## 1. 文档信息

| 项目 | 内容 |
|---|---|
| 文档名称 | Volcano `RestartPartition` 分组 Pod 故障迁移验证报告 |
| 文档状态 | 待执行 |
| 验证对象 | Volcano Job、`RestartPartition`、`partitionPolicy`、Network Topology Aware Scheduling |
| 集群规模 | 8 个 Kubernetes 节点 |
| 节点设备资源 | 每节点 `huawei.com/ascend-1980: 8` |
| Volcano 版本 | 待填写 |
| Kubernetes 版本 | 待填写 |
| 验证日期 | 待填写 |
| 执行人 | 待填写 |
| 复核人 | 待填写 |

### 1.1 变更记录

| 版本 | 日期 | 作者 | 变更说明 |
|---|---|---|---|
| v1.0 | 待填写 | 待填写 | 初始版本 |
| v1.1 | 待填写 | 待填写 | 操作逻辑收敛到脚本；测试 Pod 改为每 Pod 申请 8 张 Ascend 卡 |

## 2. 验证结论摘要

执行完成后填写。

| 用例编号 | `highestTierAllowed` | 验证边界 | 预期迁移 | 实际结果 | 结论 |
|---|---:|---|---|---|---|
| TC-01 | 1 | 只能位于同一个 tier1 | N0+N1 → N2+N3 | 待填写 | 待执行 |
| TC-02 | 2 | 可跨 tier1、不可跨 tier2 | N0+N1 → N1+N2 | 待填写 | 待执行 |
| TC-03 | 3 | 可跨 tier2、不可跨 tier3 | N0+N1 → N1+N4 | 待填写 | 待执行 |

最终结论：**待执行**。

## 3. 验证目标

本报告验证以下能力：

1. Volcano Job 中一个 Pod 进入 Failed 后，task 级策略能够触发 `RestartPartition`。
2. `RestartPartition` 只删除并重建故障 Pod 所属 partition，不重启另一个 partition。
3. partition 重建后，Volcano Scheduler 根据 `partitionPolicy.networkTopology` 重新选择 HyperNode。
4. `highestTierAllowed=1` 时，分组内两个 Pod 必须位于同一个 tier1 HyperNode。
5. `highestTierAllowed=2` 时，分组可以跨 tier1，但必须位于同一个 tier2 HyperNode。
6. `highestTierAllowed=3` 时，分组可以跨 tier2，但必须位于同一个 tier3 HyperNode。
7. 故障 partition 的两个 Pod UID 全部变化，对照 partition 的两个 Pod UID 全部不变。
8. 每个测试 Pod 申请完整的 `huawei.com/ascend-1980: 8`，保证每个节点最多承载一个测试 Pod。

## 4. 验证范围

### 4.1 包含范围

- Volcano Job task 级生命周期策略。
- `RestartPartition` action。
- `partitionPolicy.totalPartitions`、`partitionSize`、`minPartitions`。
- label-based HyperNode 自动发现。
- `network-topology-aware` 调度插件。
- `hard` 模式的 tier1、tier2、tier3 边界。
- Ascend 扩展资源 `huawei.com/ascend-1980` 的调度。
- Pod 故障后的整组重建、拓扑约束和 UID 隔离验证。

### 4.2 不包含范围

- 业务数据或训练状态恢复。
- Ascend 算子、HCCL 或集合通信性能。
- StatefulSet、MPI Operator、Ray Operator 等其他控制器。
- 跨 tier3 的负向验证；当前拓扑只有一个 tier3 根域。
- 生产节点真实断电后的 Kubernetes eviction 时延。

## 5. 测试拓扑

### 5.1 四层结构

本报告中的“四层”由物理 Node 层和三个 HyperNode 层组成：

```text
tier3:                         fabric-a
                              /        \
tier2:                    spine-a     spine-b
                          /    \       /    \
tier1:                 leaf-a leaf-b leaf-c leaf-d
                       /  \    /  \   /  \   /  \
Node:                  N0 N1  N2 N3  N4 N5  N6 N7
```

每个 tier1 HyperNode 包含两个物理节点。

### 5.2 节点映射

| 逻辑节点 | 实际节点名 | tier1 | tier2 | tier3 | Ascend 卡数 |
|---|---|---|---|---|---:|
| N0 | 待填写 | leaf-a | spine-a | fabric-a | 8 |
| N1 | 待填写 | leaf-a | spine-a | fabric-a | 8 |
| N2 | 待填写 | leaf-b | spine-a | fabric-a | 8 |
| N3 | 待填写 | leaf-b | spine-a | fabric-a | 8 |
| N4 | 待填写 | leaf-c | spine-b | fabric-a | 8 |
| N5 | 待填写 | leaf-c | spine-b | fabric-a | 8 |
| N6 | 待填写 | leaf-d | spine-b | fabric-a | 8 |
| N7 | 待填写 | leaf-d | spine-b | fabric-a | 8 |

### 5.3 tier 边界

| 配置 | 合法节点组合 | 非法节点组合 | 判定依据 |
|---|---|---|---|
| `highestTierAllowed: 1` | N2+N3 | N1+N2 | 两个 Pod 必须位于同一个 tier1 |
| `highestTierAllowed: 2` | N1+N2 | N1+N4 | 可以跨 tier1，但最低公共祖先不能高于 tier2 |
| `highestTierAllowed: 3` | N1+N4 | 本拓扑无 tier4 | 可以跨 tier2，但最低公共祖先不能高于 tier3 |

## 6. Ascend 资源使用方式

测试 Job 包含两个 partition，每个 partition 有两个 Pod，总共四个 Pod。每个 Pod 请求并限制：

```yaml
resources:
  requests:
    huawei.com/ascend-1980: 8
  limits:
    huawei.com/ascend-1980: 8
```

资源占用关系如下：

| 阶段 | Pod 数量 | 每 Pod 卡数 | 合计占用 |
|---|---:|---:|---:|
| 初始运行 | 4 | 8 | 32 卡 |
| 故障组删除后 | 2 | 8 | 16 卡 |
| 迁移完成 | 4 | 8 | 32 卡 |

由于每个节点只有 8 张卡，一个 Pod 会占满一个节点，因此无需额外配置 Pod Anti-Affinity 来实现“一节点一个 Pod”。

验证前必须确保 N0、N1、N6、N7 各有 8 张空闲卡；迁移阶段还必须确保相应用例的目标节点各有 8 张空闲卡。

## 7. 测试资产

所有文件位于：

```text
docs/verification/restart-partition-topology-failover/
├── nodes.sh                    # 唯一需要人工编辑的节点/环境配置
├── controller-topology.yaml    # label discoverer ConfigMap 模板
├── job-template.yaml           # Volcano Job 模板，每 Pod 申请 8 张卡
├── common.sh                   # 公共函数，由其他脚本调用
├── prepare-topology.sh         # 节点、设备资源和 HyperNode 准备/校验
├── run-case.sh                 # 执行 TC-01/TC-02/TC-03
└── cleanup.sh                  # 清理测试 Job 和临时标签
```

用户不需要在交互式 shell 中定义环境变量或函数。节点变量和辅助函数全部位于上述 `.sh` 文件内。

## 8. 前置条件与风险控制

### 8.1 前置条件

1. 本机可执行 `bash`、`kubectl`、`jq` 和 `sed`。
2. 当前 kubeconfig 指向待验证集群。
3. 8 个测试节点均为 Ready。
4. 每个节点的 `status.allocatable["huawei.com/ascend-1980"]` 等于 8。
5. 初始节点和目标节点上的 8 张 Ascend 卡均未被其他 Pod 占用。
6. 已安装以下 CRD：
   - `jobs.batch.volcano.sh`
   - `podgroups.scheduling.volcano.sh`
   - `hypernodes.topology.volcano.sh`
7. Scheduler 已启用 `gang`、`predicates` 和 `network-topology-aware`。
8. `default` Volcano Queue 处于 Open 状态。
9. `busybox:1.36.1` 可拉取；离线环境应在 `nodes.sh` 中替换为内部镜像。

### 8.2 风险控制

- 脚本仅 cordon N0，不执行 drain。
- 主验证使用 `PodFailed -> RestartPartition`，避免节点驱逐时间影响结果。
- `run-case.sh` 只会操作名称为 `partition-migrate-hta1/2/3` 的测试 Job。
- 脚本会拒绝删除已经存在的同名 Job，避免覆盖未知资源。
- 脚本发现 N0 在测试前已经 cordon 时会退出，不会擅自改变原始状态。
- 修改 Controller ConfigMap 前会自动保存备份。
- 用例失败时保留 Job 和证据，便于诊断；只自动恢复由脚本 cordon 的 N0。

## 9. 一次性配置

### 9.1 编辑节点配置

进入测试资产目录：

```bash
cd docs/verification/restart-partition-topology-failover
```

编辑 `nodes.sh`，只需将 N0～N7 替换为实际 Kubernetes Node 名称：

```bash
vi nodes.sh
```

默认环境参数如下，只有实际安装信息不同时才需要修改：

```bash
VOLCANO_NAMESPACE="volcano-system"
VOLCANO_RELEASE="volcano"
JOB_NAMESPACE="default"
QUEUE_NAME="default"
SCHEDULER_NAME="volcano"
ACCELERATOR_RESOURCE="huawei.com/ascend-1980"
CARDS_PER_NODE="8"
CARDS_PER_POD="8"
TEST_IMAGE="busybox:1.36.1"
```

### 9.2 检查 Scheduler 插件

Scheduler ConfigMap 中至少应有：

```yaml
tiers:
  - plugins:
      - name: priority
      - name: gang
      - name: conformance
  - plugins:
      - name: predicates
      - name: proportion
      - name: nodeorder
      - name: binpack
      - name: network-topology-aware
```

`prepare-topology.sh` 会自动检查 `gang`、`predicates` 和 `network-topology-aware`。如果缺失，先合并以上插件配置并重启 Scheduler：

```bash
kubectl rollout restart deployment/volcano-scheduler -n volcano-system
kubectl rollout status deployment/volcano-scheduler -n volcano-system
```

### 9.3 配置 label discoverer

如果 `volcano-controller.conf` 仅用于本次测试，执行：

```bash
./prepare-topology.sh --apply-controller-config
```

该命令会：

1. 检查 8 个节点是否 Ready。
2. 检查每个节点是否上报 `huawei.com/ascend-1980: 8`。
3. 备份 Scheduler 和 Controller ConfigMap。
4. 为 N0～N7 添加 tier1、tier2、tier3 标签。
5. 应用 `controller-topology.yaml`。
6. 重启 Volcano Controller。
7. 等待并验证 4/2/1 个 HyperNode。
8. 将证据写入 `evidence/<时间>-setup/`。

如果当前 Controller ConfigMap 还有 UFM、RoCE 或其他配置，不应使用覆盖参数。应将 `controller-topology.yaml` 中的 `source: label` 段合并到现有 `volcano-controller.conf`，然后执行：

```bash
./prepare-topology.sh
```

### 9.4 准备结果记录

| 检查项 | 预期 | 实际 | 结论 |
|---|---|---|---|
| 8 个节点 Ready | 是 | 待填写 | 待填写 |
| 每节点 Ascend allocatable | 8 | 待填写 | 待填写 |
| tier1 HyperNode 数量 | 4 | 待填写 | 待填写 |
| tier2 HyperNode 数量 | 2 | 待填写 | 待填写 |
| tier3 HyperNode 数量 | 1 | 待填写 | 待填写 |
| `gang`/`predicates`/`network-topology-aware` | 已启用 | 待填写 | 待填写 |

## 10. Job 配置说明

实际 Job YAML 位于 `job-template.yaml`，由 `run-case.sh` 渲染和提交。核心配置如下：

```yaml
spec:
  minAvailable: 4
  maxRetry: 5
  networkTopology:
    mode: hard
    highestTierAllowed: 3
  tasks:
    - name: worker
      replicas: 4
      minAvailable: 4
      policies:
        - event: PodFailed
          action: RestartPartition
      partitionPolicy:
        totalPartitions: 2
        partitionSize: 2
        minPartitions: 2
        networkTopology:
          mode: hard
          highestTierAllowed: 由用例传入
```

Pod 通过临时 nodeSelector 收敛候选节点：

```yaml
nodeSelector:
  test.volcano.sh/migration-case: 对应用例名称
```

运行中的 Pod 不会因该临时标签变化而被删除；只有重建后的 Pod 会根据新的候选节点集合调度。

## 11. TC-01：`highestTierAllowed=1`

### 11.1 用例目标

验证故障 partition：

1. 不能调度到 N1+N2，因为二者分属 leaf-a、leaf-b，LCA 是 tier2。
2. 可以迁移到 N2+N3，因为二者同属 leaf-b/tier1。
3. 两个故障组 Pod 都被重建。
4. N6+N7 上的对照 partition 不被重建。

### 11.2 执行命令

```bash
./run-case.sh 1
```

### 11.3 脚本执行步骤

1. 校验节点、CRD、Ascend allocatable 和 HyperNode。
2. 只允许 N0、N1、N6、N7 接收初始测试 Pod。
3. 创建两个 partition，每个 partition 两个 Pod，每 Pod 请求 8 张卡。
4. 确认故障组在 N0+N1，对照组在 N6+N7。
5. 保存两个 partition 的原始 Pod UID。
6. 将接收节点切换为非法组合 N1+N2。
7. cordon N0，并使 N0 上容器退出，触发 `PodFailed`。
8. 等待 `retryCount=1`，验证故障组两个新 Pod 均为 Pending。
9. 验证对照组 UID 未变化。
10. 将接收节点切换为合法组合 N2+N3。
11. 等待全部 Pod Ready。
12. 验证故障组 UID 全部变化、对照组 UID 全部不变。
13. 保存 Job、Pod、Controller 和 Scheduler 证据。
14. 删除测试 Job、临时标签，并恢复 N0。

### 11.4 预期结果

```text
非法阶段：N1+N2 无法满足 tier1，故障组 Pending
最终阶段：故障 partition 从 N0+N1 迁移到 N2+N3
对照组：始终位于 N6+N7
retryCount：1
```

### 11.5 结果记录

| 检查项 | 预期 | 实际 | 结论 |
|---|---|---|---|
| N1+N2 负向边界 | 两个新 Pod Pending | 待填写 | 待填写 |
| 最终节点 | N2+N3 | 待填写 | 待填写 |
| 故障组 UID | 全部变化 | 待填写 | 待填写 |
| 对照组 UID | 全部不变 | 待填写 | 待填写 |
| `retryCount` | 1 | 待填写 | 待填写 |
| 用例脚本结果 | PASS | 待填写 | 待填写 |

TC-01 结论：**待执行**。

## 12. TC-02：`highestTierAllowed=2`

### 12.1 用例目标

验证故障 partition：

1. 不能调度到 N1+N4，因为二者分属 spine-a、spine-b，LCA 是 tier3。
2. 可以迁移到 N1+N2，因为二者虽跨 leaf-a、leaf-b，但同属 spine-a/tier2。
3. 故障组两个 Pod 都被重建，对照组保持不变。

### 12.2 执行命令

```bash
./run-case.sh 2
```

### 12.3 脚本执行步骤

1. 创建 `highestTierAllowed=2` 测试 Job。
2. 初始故障组运行于 N0+N1，对照组运行于 N6+N7。
3. 将 N1+N4 设置为唯一接收组合。
4. cordon N0，并触发 N0 上测试 Pod Failed。
5. 验证故障组的新 Pod 因跨 tier2 而保持 Pending。
6. 将接收组合切换为 N1+N2。
7. 验证分组跨 tier1 后在同一 tier2 内恢复运行。
8. 验证故障组 UID 全部变化、对照组 UID 全部不变。

### 12.4 预期结果

```text
非法阶段：N1+N4 无法满足 tier2，故障组 Pending
最终阶段：故障 partition 从 N0+N1 迁移到 N1+N2
对照组：始终位于 N6+N7
retryCount：1
```

### 12.5 结果记录

| 检查项 | 预期 | 实际 | 结论 |
|---|---|---|---|
| N1+N4 负向边界 | 两个新 Pod Pending | 待填写 | 待填写 |
| 最终节点 | N1+N2 | 待填写 | 待填写 |
| 跨 tier1 | 是 | 待填写 | 待填写 |
| 保持在同一 tier2 | 是 | 待填写 | 待填写 |
| 故障组 UID | 全部变化 | 待填写 | 待填写 |
| 对照组 UID | 全部不变 | 待填写 | 待填写 |
| `retryCount` | 1 | 待填写 | 待填写 |
| 用例脚本结果 | PASS | 待填写 | 待填写 |

TC-02 结论：**待执行**。

## 13. TC-03：`highestTierAllowed=3`

### 13.1 用例目标

验证故障 partition 可以调度到 N1+N4。二者分属 spine-a、spine-b，LCA 是 fabric-a/tier3。

N1+N4 在 TC-02 中应保持 Pending，在 TC-03 中应恢复 Running，因此两项用例共同构成 tier2/tier3 边界证据。

### 13.2 执行命令

```bash
./run-case.sh 3
```

### 13.3 脚本执行步骤

1. 创建 `highestTierAllowed=3` 测试 Job。
2. 初始故障组运行于 N0+N1，对照组运行于 N6+N7。
3. 将 N1+N4 设置为唯一接收组合。
4. cordon N0，并触发 N0 上测试 Pod Failed。
5. 验证两个故障组 Pod 在 N1+N4 恢复 Ready。
6. 验证故障组 UID 全部变化、对照组 UID 全部不变。

### 13.4 预期结果

```text
最终阶段：故障 partition 从 N0+N1 迁移到 N1+N4
故障组跨越 tier2，但仍位于 fabric-a/tier3
对照组始终位于 N6+N7
retryCount：1
```

### 13.5 结果记录

| 检查项 | 预期 | 实际 | 结论 |
|---|---|---|---|
| 最终节点 | N1+N4 | 待填写 | 待填写 |
| 跨 tier2 | 是 | 待填写 | 待填写 |
| 保持在同一 tier3 | 是 | 待填写 | 待填写 |
| 故障组 UID | 全部变化 | 待填写 | 待填写 |
| 对照组 UID | 全部不变 | 待填写 | 待填写 |
| `retryCount` | 1 | 待填写 | 待填写 |
| 用例脚本结果 | PASS | 待填写 | 待填写 |

TC-03 结论：**待执行**。

## 14. 证据说明

每次运行会生成独立目录：

```text
docs/verification/restart-partition-topology-failover/evidence/
└── YYYYMMDD-HHMMSS-htaN/
    ├── partition-migrate-htaN.yaml
    ├── pods-before.txt
    ├── invalid-receivers-pending.txt   # TC-01/TC-02
    ├── pods-after.txt
    ├── fault-before.uid
    ├── fault-after.uid
    ├── fault-common.uid
    ├── control-before.uid
    ├── control-after.uid
    ├── control-after.diff
    ├── job-after.yaml
    ├── job-describe.txt
    ├── controller.log
    ├── scheduler.log
    └── result.md
```

关键证据判定：

| 文件 | 通过条件 |
|---|---|
| `result.md` | `result: PASS` |
| `fault-common.uid` | 空文件，表示故障组没有保留旧 UID |
| `control-after.diff` | 空文件，表示对照组 UID 未改变 |
| `pods-after.txt` | 故障组和对照组位置符合用例预期 |
| `job-after.yaml` | `status.retryCount: 1`，Job 未进入 Failed |
| `job-describe.txt` | Event 中存在 `RestartPartition`/`ExecuteAction` |

## 15. 总体验收标准

只有同时满足以下条件，才能判定整体验证通过：

1. 每个测试节点上报 `huawei.com/ascend-1980: 8`。
2. label discoverer 创建 4 个 tier1、2 个 tier2 和 1 个 tier3 HyperNode。
3. 三个用例初始均有 4 个 Ready Pod，每个 Pod 独占一个 8 卡节点。
4. 三个用例均只触发一次 `RestartPartition`，`retryCount=1`。
5. 三个用例中故障 partition 的两个 Pod UID 全部变化。
6. 三个用例中对照 partition 的两个 Pod UID 全部不变。
7. TC-01 中 N1+N2 为 Pending，N2+N3 为 Ready。
8. TC-02 中 N1+N4 为 Pending，N1+N2 为 Ready。
9. TC-03 中 N1+N4 为 Ready。
10. 三个用例最终都输出 `PASS`，且 Job 未进入 Failed。

## 16. 缺陷判定建议

| 现象 | 判定方向 |
|---|---|
| 节点未上报 8 张卡 | Ascend Device Plugin、驱动或节点资源注册异常 |
| 测试 Pod 因 Insufficient Ascend Pending | 目标节点卡资源被占用，或设备插件不可用 |
| 只重建故障 Pod | `RestartPartition` 未按 partition 执行 |
| 对照 partition UID 变化 | partition 隔离异常，或错误触发了 task/job 级重启 |
| `retryCount` 持续增加 | 生命周期事件循环或受控删除再次触发策略 |
| TC-01 能在 N1+N2 运行 | tier1 硬约束未生效 |
| TC-02 能在 N1+N4 运行 | tier2 硬约束未生效 |
| TC-03 不能在 N1+N4 运行 | tier3 HyperNode、资源或调度插件异常 |
| 初始分组不是 N0+N1、N6+N7 | 拓扑树、节点临时标签或资源占用与测试假设不符 |

## 17. 清理与回滚

正常情况下，`run-case.sh` 会在成功后自动删除测试 Job、临时 receiver 标签，并恢复 N0。

如果用例异常退出，执行：

```bash
./cleanup.sh
```

该脚本会：

- 删除 `partition-migrate-hta1/2/3`。
- 删除 `test.volcano.sh/migration-case` 临时标签。
- 如果检测到由测试脚本创建的 cordon 标记，则恢复 N0。
- 保留 tier1、tier2、tier3 标签和 HyperNode，供问题分析。

如需删除测试拓扑标签，确认这些标签不再被其他任务使用后执行：

```bash
kubectl label nodes --all \
  topology.volcano.sh/tier1- \
  topology.volcano.sh/tier2- \
  topology.volcano.sh/tier3-
```

Controller ConfigMap 和 Scheduler ConfigMap 的原始版本保存在 `evidence/<时间>-setup/`。只在确认备份正确后进行回滚。

## 18. 真实节点故障扩展测试

主流程采用“cordon N0 + 测试容器退出”的方式，将两个变量分开验证：

1. N0 不再是重建 Pod 的合法接收节点。
2. `PodFailed` 明确触发 `RestartPartition`。

如需测试节点掉电或 kubelet 停止后的 eviction 链路，可以在专用集群将 Job 策略改为：

```yaml
policies:
  - event: PodEvicted
    action: RestartPartition
```

该扩展测试受 Node Monitor、NoExecute taint 和 eviction 时间配置影响。执行前应通过目标版本源码或发布说明，确认受控删除 Pod 不会再次触发 `RestartPartition`，否则可能重复重启直至达到最大重试次数。

## 19. 完整 YAML 清单

### 19.1 Controller label discoverer

对应文件：`restart-partition-topology-failover/controller-topology.yaml`。`prepare-topology.sh --apply-controller-config` 会将两个占位符渲染为 `nodes.sh` 中的实际值。

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: __VOLCANO_RELEASE__-controller-configmap
  namespace: __VOLCANO_NAMESPACE__
data:
  volcano-controller.conf: |
    networkTopologyDiscovery:
      - source: label
        enabled: true
        interval: 1m
        config:
          networkTopologyTypes:
            restartMigration:
              - nodeLabel: "topology.volcano.sh/tier3"
              - nodeLabel: "topology.volcano.sh/tier2"
              - nodeLabel: "topology.volcano.sh/tier1"
              - nodeLabel: "kubernetes.io/hostname"
```

### 19.2 Volcano Job

对应文件：`restart-partition-topology-failover/job-template.yaml`。`run-case.sh` 会按用例替换 `__CASE__`、`__TIER__` 和资源等占位符，并把渲染后的 YAML 存入证据目录。

```yaml
apiVersion: batch.volcano.sh/v1alpha1
kind: Job
metadata:
  name: partition-migrate-__CASE__
  namespace: __JOB_NAMESPACE__
spec:
  schedulerName: __SCHEDULER_NAME__
  queue: __QUEUE_NAME__
  minAvailable: 4
  maxRetry: 5
  networkTopology:
    mode: hard
    highestTierAllowed: 3
  tasks:
    - name: worker
      replicas: 4
      minAvailable: 4
      policies:
        - event: PodFailed
          action: RestartPartition
      partitionPolicy:
        totalPartitions: 2
        partitionSize: 2
        minPartitions: 2
        networkTopology:
          mode: hard
          highestTierAllowed: __TIER__
      template:
        metadata:
          labels:
            test.volcano.sh/case: "__CASE__"
        spec:
          restartPolicy: Never
          nodeSelector:
            test.volcano.sh/migration-case: "__CASE__"
          containers:
            - name: worker
              image: __TEST_IMAGE__
              imagePullPolicy: IfNotPresent
              command:
                - /bin/sh
                - -c
                - |
                  echo "worker started on ${NODE_NAME}"
                  while [ ! -f /work/fail ]; do
                    sleep 1
                  done
                  echo "inject failure"
                  exit 42
              env:
                - name: NODE_NAME
                  valueFrom:
                    fieldRef:
                      fieldPath: spec.nodeName
              resources:
                requests:
                  cpu: 10m
                  memory: 16Mi
                  __ACCELERATOR_RESOURCE__: __CARDS_PER_POD__
                limits:
                  cpu: 100m
                  memory: 64Mi
                  __ACCELERATOR_RESOURCE__: __CARDS_PER_POD__
              volumeMounts:
                - name: work
                  mountPath: /work
          volumes:
            - name: work
              emptyDir: {}
```

## 20. 参考资料

- [Volcano Network Topology Aware Scheduling 用户指南](../user-guide/how_to_use_network_topology_aware_scheduling.md)
- [Volcano Network Topology Aware Scheduling 设计文档](../design/Network%20Topology%20Aware%20Scheduling.md)
- [Volcano Job Policy 用户指南](../user-guide/how_to_use_job_policy.md)

## 21. 最终签署

| 角色 | 姓名 | 结论 | 日期 | 签字/备注 |
|---|---|---|---|---|
| 执行人 | 待填写 | 待填写 | 待填写 | 待填写 |
| 复核人 | 待填写 | 待填写 | 待填写 | 待填写 |
| 负责人 | 待填写 | 待填写 | 待填写 | 待填写 |
