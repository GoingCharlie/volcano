# Repack 真实 8×8 卡集群验证

本目录用于在 8 个节点、每节点 8 张加速卡的真实集群上验证 Repack，重点覆盖本次新增的
`PartiallySucceeded` 终态：执行过程没有错误，但计划腾空的节点被新提交的用户 Pod 占用，
因此实际腾空节点数为 0。

脚本默认使用需求中给出的资源名 `huawei.com/asend-1980`。请先以 Node 的
`status.allocatable` 为准核对拼写；如果真实资源名是 `huawei.com/ascend-1980`，必须在
`config.env` 中修改。前置检查不会自动猜测资源名。

## 安全边界

这是一组会真实执行 Eviction 的验证脚本。执行前必须满足：

- 在维护窗口操作，8 个节点均为专用测试节点；
- 8 个节点 Ready、未 cordon，每个节点的目标资源 allocatable 均为 8；
- 节点上不存在其他请求目标加速卡资源的 Pod；
- 测试账号可以创建 Namespace、StatefulSet、Pod、RepackRun 和 RepackPolicy；
- Repack Engine、controller-manager、scheduler 和 admission 已替换为本次代码对应镜像；
- live RepackRun CRD 和 RepackPolicy 内嵌状态 schema 均允许 `PartiallySucceeded`；
- 测试镜像申请真实设备后能够正常启动，并且所有节点都能拉取该镜像。

脚本不会修改 Node capacity、taint 或 cordon 状态。清理脚本只删除：

- `config.env` 中配置的专用测试 Namespace，且该 Namespace 必须带脚本的 managed annotation；
- 带 `repack-test.volcano.sh/id=<TEST_ID>` 标签的 RepackRun/RepackPolicy。

如果前置检查发现其他加速卡 Pod，默认立即退出。不要为了在生产业务节点上强行执行而设置
`ALLOW_EXISTING_ACCELERATOR_PODS=true`；该开关只用于已经人工确认不会相互影响的隔离测试环境。

## 测试布局

脚本在所选 8 个节点内创建如下布局：

| 工作负载 | 初始节点 | 卡数 | 是否允许 Repack 移动 |
|---|---:|---:|---|
| `repack-receiver-0` | `TEST_NODES` 第 1 个节点 | 4 | 否，仅作为接收节点 |
| `repack-moving-0` | `TEST_NODES` 第 2 个节点 | 2 | 是，唯一可移动 PodGroup |
| 其余节点 | 第 3～8 个节点 | 0 | 参与 node scope |

两个工作负载均为 `OnDelete` StatefulSet，Pod 自身只有“可在这 8 个节点之间调度”的 node
affinity，不带单节点约束。为了形成确定的初始布局，脚本临时在其他测试节点上创建资源填充 Pod：

1. 占满第 2～8 个节点，把 4 卡 receiver 引导到第 1 个节点；
2. 删除填充 Pod；
3. 占满第 1、3～8 个节点的剩余卡，把 2 卡 moving Pod 引导到第 2 个节点；
4. 再次删除全部填充 Pod。

因此初始位置确定，同时原 Pod 的调度约束仍允许替代 Pod 迁移到任一测试节点。填充 Pod 都在专用
测试 Namespace 内，不修改 Node capacity、label、taint 或 cordon 状态。

每次 Execute 前都会先运行一次 DryRun，并强校验：

- `Complete=True/Reason=RepackRecommended`；
- 恰好计划移动一个 PodGroup；
- 恰好计划腾空一个节点；
- 计划腾空节点必须是 `TEST_NODES` 中第 2 个节点。

任何条件不满足，脚本都会在发生 Eviction 前退出。

### PartiallySucceeded 场景的确定性控制

该场景还会提前创建一个请求 8 卡、只允许调度到计划腾空节点的普通用户 Pod。它最初因为节点
仍被 `repack-moving-0` 占用而保持 Pending。为避免真实调度器上的竞态：

1. 替代 Pod 模板预先带有测试专用 scheduling gate；
2. Repack 驱逐原 Pod 后，新用户 Pod 获得完整 8 卡并绑定到计划腾空节点；
3. 脚本确认绑定完成后，仅删除测试专用 gate；
4. 替代 Pod 调度到接收节点，Repack 执行正常完成；
5. 终态快照发现计划腾空节点已被新用户 Pod 占用。

预期结果是 `phase=PartiallySucceeded`、计划腾空 1 个节点、实际腾空 0 个节点，而不是
`Failed`。

## 1. 准备配置

在本目录执行：

```bash
cp config.env.example config.env
vi config.env
```

至少必须修改：

- `EXPECTED_CONTEXT`：真实集群的 kubectl context；
- `TEST_NODES`：明确列出 8 个专用测试节点；
- `ACCELERATOR_RESOURCE`：确认实际扩展资源名；
- `TEST_IMAGE`：集群可以拉取且能申请真实设备的长运行镜像；
- `TOLERATIONS_JSON`：节点有 NoSchedule taint 时填写；
- `EXPECTED_IMAGE_TAG`：建议填写本次构建镜像的 tag/commit SHA。

查看候选节点和真实资源名：

```bash
kubectl get nodes -o json | jq -r '
  .items[] |
  [.metadata.name,
   (.status.allocatable["huawei.com/asend-1980"] // "0"),
   (.status.allocatable["huawei.com/ascend-1980"] // "0")] |
  @tsv'
```

## 2. 前置检查

```bash
./scripts/preflight.sh
```

前置检查会验证：

- kubectl context 精确匹配；
- 8 个节点的 Ready/cordon/allocatable/hostname 状态；
- 选中节点没有现存加速卡 Pod；
- scheduling gates 和相关 CRD 可用；
- 两处 CRD phase enum 均包含 `PartiallySucceeded`；
- 集群中只有一个 Ready 的 Repack Engine；
- 没有其他 Pending/Running 的 Execute RepackRun；
- 配置了 `EXPECTED_IMAGE_TAG` 时，相关 Deployment 镜像 tag 一致。

建议同时保留以下基线信息：

```bash
kubectl get nodes -o wide
kubectl get pods -A -o wide
kubectl get repackruns.repack.volcano.sh
kubectl get deployments.apps -A -l app=volcano-repack-engine -o wide
```

## 3. 先执行 DryRun

```bash
./scripts/run-scenario.sh dry-run
```

成功标志：

```text
phase: Succeeded
Complete=True
reason: RepackRecommended
plan.freedNodes: [TEST_NODES 中第 2 个节点]
plan.moves length: 1
```

脚本会保留资源用于检查：

```bash
./scripts/status.sh
```

检查完成后清理：

```bash
./scripts/cleanup.sh
```

## 4. 验证正常成功终态

```bash
./scripts/run-scenario.sh success
```

脚本先重建隔离布局并做 DryRun 安全校验，然后执行 Execute。预期：

- `phase=Succeeded`；
- `Complete=True`；
- reason 为 `ExecutionCompleted` 或 `ExecutionCompletedWithAlternativePlacement`；
- `result.metricsVerified=true`；
- `status.result.freedNodes == status.plan.freedNodes`；
- 没有 `Failed=True` condition。

完成检查后执行：

```bash
./scripts/cleanup.sh
```

注意：Execute 完成后受 `--repack-execute-cooldown` 限制。紧接着执行下一条 Execute 时，Run
可能先进入 `Pending/ExecuteCooldownActive`；脚本默认等待 20 分钟。也可以等待 cooldown 结束后
再运行下一场景，不要通过删除业务对象绕过安全间隔。

## 5. 验证计划节点被新用户 Pod 占用

这是本次改动的核心真实集群验证：

```bash
./scripts/run-scenario.sh partial
```

脚本必须输出类似：

```text
new user blocker is bound to planned freed node <node>
RepackRun <name> terminal: phase=PartiallySucceeded, reason=BenefitNotRealized
PartiallySucceeded contract verified: planned=1 actual=0 ...
PASS: a new user Pod reused the planned node, phase=PartiallySucceeded
```

完整断言为：

```text
status.phase                                      = PartiallySucceeded
status.plan.freedNodes length                     = 1
status.result.metricsVerified                     = true
status.result.freedNodeCount                      = 0
status.result.freedNodes length                   = 0
conditions[Complete].status                       = True
conditions[Complete].reason                       = BenefitNotRealized
conditions[Failed].status                         != True
```

同时检查新用户 Pod 和替代 Pod 的最终节点：

```bash
kubectl get pods -n repack-real-test -o wide
./scripts/status.sh
```

## 6. 验证 RepackPolicy 成功侧记账

可选但推荐执行。它使用相同的“计划 1、实际 0”场景，但 Execute RepackRun 由每分钟 cron 的
RepackPolicy 派生。脚本发现第一个派生 Run 后会立即 suspend Policy，防止再次触发：

```bash
./scripts/cleanup.sh
# 如果刚执行过 Execute，先等待 cooldown，或允许脚本等待 Pending 状态结束。
./scripts/run-scenario.sh partial-policy
```

除 `PartiallySucceeded` 的全部断言外，还会验证：

```text
policy.status.lastRunStatus.name   = 派生 Run 名称
policy.status.lastRunStatus.phase  = PartiallySucceeded
policy.status.lastSuccessfulTime   != null
```

这证明 `PartiallySucceeded` 会更新成功时间并进入 successful history 一侧。脚本将
`successfulRunsHistoryLimit` 配置为 3；“Succeeded + PartiallySucceeded 合计最多保留 3 个”的完整
滚动回收需要跨至少 4 次 Execute/cooldown 周期，建议作为长时间稳定性验证执行，而不是在一次
维护窗口内缩短生产安全 cooldown。

## 7. 日志与排障

随时查看当前测试状态：

```bash
./scripts/status.sh
```

定位 Repack Engine 所在位置：

```bash
kubectl get deployments.apps -A -l app=volcano-repack-engine
```

查看 Engine 和 controller 日志（将名称替换为实际 Deployment）：

```bash
kubectl logs -n <volcano-namespace> deployment/<repack-engine-deployment> --since=30m
kubectl logs -n <volcano-namespace> deployment/<controller-manager-deployment> --since=30m
```

常见问题：

- `allocatable=0`：资源名拼写错误，或设备插件未正常上报；
- fixture Pod Pending：检查镜像、taint/toleration、Volcano scheduler 和设备插件事件；
- DryRun 返回 `NoFragmentation`：节点仍有其他资源 Pod，或 scope/PodGroup 未被正确识别；
- Execute 长时间 Pending：检查 `Progressing=False` reason，通常是 `ExecuteCooldownActive` 或另一个 Execute 正在运行；
- blocker 不绑定：确认 moving 原 Pod 已删除、blocker 请求 8 卡且目标节点设备已释放；
- `Failed`：保留现场，先执行 `status.sh` 和日志采集，再清理。

## 8. 清理

无论成功或失败，完成现场信息收集后执行：

```bash
./scripts/cleanup.sh
```

确认无遗留：

```bash
kubectl get namespace repack-real-test
kubectl get repackruns.repack.volcano.sh -l repack-test.volcano.sh/id=repack-real-01
kubectl get repackpolicies.repack.volcano.sh -l repack-test.volcano.sh/id=repack-real-01
```

上述资源应均不存在。脚本从不删除或修改 `TEST_NODES` 本身。
