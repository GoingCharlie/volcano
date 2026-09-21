# Repack 已释放节点被新作业复用—真实集群验证

## 1. 验证目标

本用例验证提交 `3b11004eb` 的核心行为：

> Repack 已将原 victim 从计划腾空节点 N0 迁出，replacement 也已落到计划接收节点 N1。在 Engine 采集终态快照前，用户新提交的作业 Pod 已调度到 N0 并消费目标资源。这说明 N0 释放的容量已被正常复用，RepackRun 应当成功，而不应因终态时 N0 不空闲而失败。

本验证包不修改 CRD，也不手工修改 RepackRun status。规划、Eviction API、replacement placement、Volcano Job 调度和终态判定都经过真实集群链路。

## 2. 通过标准

| 检查项 | 必须结果 |
|---|---|
| RepackRun phase | `Succeeded` |
| 终态 condition reason | `ExecutionCompleted` |
| 计划路径 | N0 -> N1，迁移 4 卡 |
| replacement | `selectedNodeName=N1` 且 `actualNodeName=N1` |
| 新用户作业 | 1 个 1 卡 VCJob Pod，由 Volcano Scheduler 绑定到 N0 |
| `status.plan.freedNodes` | `[N0]` |
| `status.result.metricsVerified` | `true` |
| `status.result.freedNodes` | `[]`，因为它保留“终态快照中当前为空”的语义 |
| `status.message` | 包含 `already reused by unrelated workloads` 和 N0 |

旧实现的典型结果是 `Failed / BenefitNotRealized`，因此该用例可以直接区分修复前后的行为。

## 3. 集群前置条件

- 集群恰好 8 个 Ready、未 cordon 的节点；
- 每节点 `huawei.com/ascend-1980` Capacity 和 Allocatable 都是 8；
- 执行前 64 张卡都没有被 Pod 请求；
- Volcano Scheduler、Controller、Admission 和 Repack Engine 已部署，Repack/PodGroup/VCJob CRD 已安装；
- `/pods/mutate` webhook 已启用，用于将 Repack replacement 稳定阻塞在 placement gate；
- Repack Engine 镜像包含 `3b11004eb` 或后续等价修复；
- 执行机已安装 Bash、`kubectl`、`jq`、`sed` 和 `sort`；
- 执行账号可管理 Node 标签/污点、测试 Namespace/StatefulSet/VCJob、RepackRun，并可缩放 Repack Engine Deployment；
- 测试期间不运行其他 Execute RepackRun，所有 RepackPolicy 都已 `suspend`。

脚本会强制检查上述条件。请只在无业务流量的专用验证集群执行。

## 4. 测试布局

| 阶段 | N0（计划腾空/复用节点） | N1（接收节点） | N2-N7 |
|---|---|---|---|
| 初始 | source StatefulSet Pod，4 卡 | receiver StatefulSet Pod，4 卡 | 0 卡 |
| 驱逐后、Engine 暂停 | source replacement 已创建但被 gate 拦住，节点 0 卡 | receiver Pod，4 卡 | 临时 `NoSchedule` |
| 新作业提交后 | concurrent-user VCJob Pod，1 卡 | receiver Pod，4 卡 | 临时 `NoSchedule` |
| Engine 恢复后 | concurrent-user VCJob Pod，1 卡 | receiver 4 卡 + replacement 4 卡 | 临时 `NoSchedule` |

source 和 receiver Pod 初次定位通过短时 `NoSchedule` 污点完成，Pod 模板本身没有持久节点约束，因而 source 可以被 Repack 真实迁移。新 VCJob 使用 N0 的测试专用 `nodeSelector`，但不写 `spec.nodeName`；Pod 仍由 Volcano Scheduler 完成绑定。

## 5. 为什么要暂停 Engine

“新作业占用 N0”必须发生在 victim 已离开 N0 之后、Engine 进行终态判定之前。如果单纯依赖轮询抢这个窗口，结果会受控制器速度影响而不稳定。

`run-test.sh` 使用如下可恢复检查点：

1. source Pod 进入 Terminating，且 Run 中的 `Eviction=Accepted` 已持久化。
2. 脚本记录 Engine Deployment 原副本数，再临时缩容到 0。
3. Volcano Controller 和 Admission 继续运行；StatefulSet replacement 创建后被 `repack.volcano.sh/placement` gate 拦住。
4. 提交新 VCJob，等待其 Pod Ready 且占用 N0。
5. 恢复 Engine 原副本数。Engine 重启时的缓存初始快照已包含新作业，随后完成 replacement placement 和终态判定。

暂停期间 Scheduler、Controller 和 Admission 不会停止，但所有 RepackRun 都不会向前推进。因此预检会拒绝已有活动 Run 或未暂停的 Policy。

## 6. 准备修复镜像

如果尚未构建，可从当前仓库生成 Repack Engine 镜像。用真实镜像仓库、标签和集群架构替换下列示例：

```bash
git merge-base --is-ancestor 3b11004eb HEAD
make vc-repack-engine-image \
  IMAGE_PREFIX=registry.example.com/volcano \
  TAG=repack-fix-3b11004eb \
  DOCKER_PLATFORMS=linux/amd64
docker push registry.example.com/volcano/vc-repack-engine:repack-fix-3b11004eb
```

更新集群中 Repack Engine 镜像并等待 rollout 完成。建议记录 image digest，不要仅使用 `latest` 作为验证依据。

## 7. 执行步骤

### 7.1 配置

```bash
cd docs/verification/repack-concurrent-node-reuse
vi config.sh
```

必填或必须确认的配置：

- `EXPECTED_KUBE_CONTEXT`：目标集群当前 context；
- `NODES`：完整的 8 个节点名，希望作为 N0 的放第 1 个，N1 放第 2 个；
- `EXPECTED_ENGINE_IMAGE`：集群中已部署的、包含修复的完整镜像名；
- `TEST_IMAGE`：必要时替换为集群可拉取的内部镜像；
- `QUEUE_NAME`：默认为 `default`，如集群使用其他 Open Queue，修改为对应名称。

### 7.2 预检

```bash
./preflight.sh
```

预检包含 context、RBAC、CRD、组件、`/pods/mutate` webhook、Engine 镜像、Queue、8 x 8 卡节点、64 卡空闲、活动 RepackRun/Policy 和测试残留检查。任一条件不符合都会立即停止。

### 7.3 构造 4+4 卡碎片布局

```bash
./prepare-fixture.sh
```

脚本将创建专用 namespace，把 4 卡 source Pod 放到 N0、4 卡 receiver Pod 放到 N1，并等待 source 自动 PodGroup 就绪。

### 7.4 执行定向回归

```bash
./run-test.sh
```

脚本会自动：

- 为 N2-N7 添加临时 receiver-hold 污点，使计划唯一；
- 创建 Execute RepackRun，校验计划精确为 N0 -> N1；
- 观察到 victim 进入 Terminating 且 `Eviction=Accepted` 已持久化后暂停 Engine；
- 等待旧 UID 消失、新 UID replacement 进入 `WaitingForNodeSelection`；
- 创建 1 卡 Volcano Job，校验 Pod 经 Scheduler 绑定到 N0 并 Ready；
- 恢复 Engine，等待 replacement 落到 N1；
- 校验 Run 终态、plan/result 语义、victim/replacement UID、新作业 UID 和节点占用；
- 在 `evidence/<timestamp>/` 保存 YAML、Events 和组件日志。

通过时核心输出类似：

```text
PASS: 已释放节点被新用户作业复用时，RepackRun 仍成功
phase=Succeeded
reason=ExecutionCompleted
plannedFreedNodes=<N0>
terminalCurrentlyFreeNodes=[]
reusedNode=<N0>
reusePod=repack-concurrent-reuse-real/<pod-name>
replacementNode=<N1>
```

### 7.5 清理

检查完资料后执行：

```bash
./cleanup.sh
```

脚本会先根据 `.state/` 中的记录恢复 Engine 原副本数，再删除固定名称的 RepackRun、整个测试 namespace，并移除本用例的污点和节点标签。`evidence/` 保留在本地。

## 8. 中断恢复与安全边界

`run-test.sh` 正常报错或收到 `Ctrl-C` 时，EXIT trap 会先收集现场，然后恢复 Engine 原副本数并移除 receiver-hold 污点。如果执行机断电、Shell 被 `kill -9` 或网络中断，重连后立即在同一目录执行：

```bash
./cleanup.sh
```

如果 `.state/` 状态文件也丢失，先手工检查：

```bash
kubectl -n volcano-system get deployment -l app=volcano-repack-engine
kubectl -n volcano-system get pods -l app=volcano-repack-engine
```

确认 Engine 副本数已恢复后，再删除测试资源。

脚本的集群变更范围仅包括：

- 一个专用 namespace `repack-concurrent-reuse-real`；
- 一个集群级 RepackRun `repack-concurrent-node-reuse`；
- N0 的测试标签；
- 节点上的两类测试专用 `NoSchedule` 污点；
- Repack Engine Deployment 的一次临时缩容和原值恢复。

脚本不 cordon、不 drain 节点，不修改业务 namespace，不修改 CRD 或 RepackRun status。

## 9. 文件说明

| 文件 | 用途 |
|---|---|
| `config.sh` | context、8 个节点、镜像、Queue 和超时配置 |
| `preflight.sh` | 只读预检和 YAML 客户端 dry-run |
| `prepare-fixture.sh` | 创建 N0=4 卡、N1=4 卡的初始布局 |
| `run-test.sh` | 执行 Repack、创建并发 VCJob、恢复 Engine、断言和取证 |
| `cleanup.sh` | 恢复 Engine 并清理集群资源 |
| `manifests/10-source.yaml` | 可迁移的 4 卡 source StatefulSet |
| `manifests/20-receiver.yaml` | N1 上的 4 卡 receiver StatefulSet |
| `manifests/30-reuse-job.yaml` | 新提交的 1 卡 Volcano Job |
| `manifests/40-repack-run.yaml` | 仅允许释放 N0 的 Execute RepackRun |

源 YAML 使用 `__PLACEHOLDER__`，脚本会根据 `config.sh` 生成 `.rendered/`，不需要手工修改 manifest。

## 10. 结果记录

| 字段 | 实测值 |
|---|---|
| 日期 / 执行人 | |
| Kubernetes 版本 | |
| Volcano 版本/提交 | |
| Repack Engine image / digest | |
| N0 / N1 | |
| RepackRun phase / reason | |
| victim UID / replacement UID / reuse Pod UID | |
| plan.freedNodes / result.freedNodes | |
| status.message | |
| evidence 目录 | |
| 结论 | 通过 / 不通过 |

## 11. 失败排查

- `NoFragmentation` / `InsufficientImprovement`：检查 N0/N1 是否分别只有 4 卡 source/receiver Pod，以及 source PodGroup 是否继承 `repack-reuse-test=source`。
- replacement 未出现 gate：检查 `/pods/mutate` webhook、PodGroup 上的 `repack.volcano.sh/placement-lease` 和 Admission 日志。
- 新 VCJob 长时间 Pending：检查 N0 测试标签、Queue 状态、Volcano Scheduler Events 和 Ascend device plugin。
- replacement 没有落到 N1：检查 N1 是否仅占用 4 卡，以及 N2-N7 的 receiver-hold 污点是否仍在。
- Run 为 `BenefitNotRealized`：先确认 victim/replacement/reuse Pod 的 UID 和节点与证据一致；若一致，优先核对 Engine 实际 digest 是否包含 `3b11004eb`。
- Run 为 `ResultVerificationFailed`：检查 Scheduler cache/Pod informer 日志以及 Run 的 `executionDeadline`，确认 Engine 暂停时间未超过执行截止时间。
- 脚本中断：先执行 `./cleanup.sh`，确认 Engine 和节点元数据已恢复，再查看最新 `evidence/` 目录。
