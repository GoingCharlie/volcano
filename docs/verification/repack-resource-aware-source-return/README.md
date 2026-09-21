# Repack replacement 回源但不再申请目标资源—真实集群验证报告

## 1. 验证目标

验证提交 `28e82d409` 对以下缺陷的修复：

> Repack 计划将原 Pod 从源节点 N0 迁移到 N1。驱逐后 workload 模板已变更，replacement 不再申请 `huawei.com/ascend-1980`，但实际落回 N0。N0 仍然已释放目标资源，因此不应仅根据 `actualNodeName == N0` 判定收益失败。

本用例是对修复点的定向回归，不是 `highestTierAllowed` 分层拓扑用例。

## 2. 验证结论标准

| 项目 | 修复后的必须结果 |
|---|---|
| RepackRun phase | `Succeeded` |
| 终态 condition reason | `ExecutionCompletedWithAlternativePlacement` |
| 计划路径 | `N0 -> N1` |
| `placement.selectedNodeName` | N1 |
| `placement.actualNodeName` | N0 |
| replacement 卡数 | `0` |
| `result.metricsVerified` | `true` |
| `result.freedNodes` | 只包含 N0 |

如果仍是旧实现，典型结果为 `Failed / BenefitNotRealized`。因此本用例能直接区分修复前后的行为。

## 3. 集群和风险边界

前置条件：

- 集群恰好 8 个 Ready、未 cordon 的节点；
- 每节点 `huawei.com/ascend-1980` 的 Capacity 和 Allocatable 都是 8；
- 执行前 64 张卡全部空闲；
- Volcano Scheduler、Controller 和 Repack Engine 已部署，Repack CRD 已安装；
- Repack Engine 镜像包含修复提交 `28e82d409` 或后续等价修复；
- 执行机已安装 Bash、`kubectl`、`jq`、`sed` 和 `sort`；
- 执行账号有 Node 标签/污点、Namespace/StatefulSet/Pod 和集群级 RepackRun 的管理权限；
- 测试期间暂停其他可能创建 Execute RepackRun 的 RepackPolicy 或自动化。

测试的集群变更范围：

- 创建专用 namespace `repack-fix-real`；
- N0 和 N1 各运行 1 个 4 卡 StatefulSet Pod；
- 初始布局期间，对非目标节点短暂添加测试专用 `NoSchedule` 污点；
- Execute 期间，对 N2-N7 短暂添加另一个测试污点，使 N1 成为唯一计划接收节点；
- Repack 只驱逐 source StatefulSet 的 1 个 Pod；
- 脚本不 cordon、不 drain 节点，不修改业务 namespace。

建议在无业务流量的专用验证集群执行。

## 4. 用例布局和时序

1. source Pod 不带持久节点约束，通过短暂污点首次调度到 N0，申请 4 卡。
2. receiver Pod 同样首次调度到 N1，申请 4 卡。
3. N2-N7 在 Repack 规划期间临时不可调度，`scope.nodes` 又只允许释放 N0，因此计划必须为 N0 -> N1。
4. Repack 通过 Eviction API 驱逐 source Pod。该 Pod 有 90 秒优雅终止时间，脚本在观察到 `deletionTimestamp` 后立即更新 StatefulSet 模板。
5. StatefulSet 使用 `OnDelete` 策略，更新模板不会另外删除旧 Pod；当 Repack 驱逐完成后，同名 replacement 才从新模板创建。
6. Engine 先把计划接收节点 N1 持久化到 `placement.selectedNodeName`，placement controller 再移除 replacement 的 scheduling gate。
7. 新模板删除 Ascend 资源请求，并用测试节点标签使 Scheduler 在绑定时拒绝 N1、将 replacement 落回 N0；已持久的 `selectedNodeName` 仍为 N1。
8. 最终 N0 上虽然存在 own replacement，但其不消费目标资源，因此 N0 应当仍被判定为 freed。

## 5. 准备修复镜像

如果尚未构建，可从当前仓库生成 Repack Engine 镜像。以实际镜像仓库和集群架构替换示例值：

```bash
git merge-base --is-ancestor 28e82d409 HEAD
make vc-repack-engine-image \
  IMAGE_PREFIX=registry.example.com/volcano \
  TAG=repack-fix-28e82d409 \
  DOCKER_PLATFORMS=linux/amd64
docker push registry.example.com/volcano/vc-repack-engine:repack-fix-28e82d409
```

在集群的 Volcano Helm values 中指定该镜像，或对现有 Repack Engine Deployment 更新镜像，然后等待 rollout 完成。不建议仅使用 `latest` 作为验证依据。

## 6. 执行步骤

### 6.1 配置

进入本目录，只修改 [`config.sh`](config.sh)：

```bash
cd docs/verification/repack-resource-aware-source-return
vi config.sh
```

必填项：

- `EXPECTED_KUBE_CONTEXT`：当前目标集群 context；
- `NODES`：完整的 8 个节点名，希望作为源节点的放第 1 个，接收节点放第 2 个；
- `EXPECTED_ENGINE_IMAGE`：集群中已部署的、包含修复的完整镜像名；
- `TEST_IMAGE`：必要时替换为集群可拉取的内部镜像；
- `QUEUE_NAME`：默认为 `default`；如集群使用其他 Open Queue，修改为对应名称。

### 6.2 预检

```bash
./preflight.sh
```

预检会直接失败的情况包括：context 不匹配、不是恰好 8 节点、单节点不是 8 卡、任一目标卡已被 Pod 请求、Engine 镜像不匹配、Queue 不是 Open、存在活动 RepackRun、存在未 suspend 的 RepackPolicy，或存在上次遗留的测试资源。

### 6.3 构造 4+4 卡碎片布局

```bash
./prepare-fixture.sh
```

执行后核对输出：

```text
N0: repack-fix-source-0   4 卡
N1: repack-fix-receiver-0 4 卡
N2-N7:                    0 卡
```

### 6.4 执行回归

```bash
./run-test.sh
```

脚本会自动完成以下关键操作：

- 保存 source Pod 的旧 UID；
- 创建 Execute RepackRun；
- 在旧 Pod 进入 Terminating 后更新 StatefulSet 模板；
- 等待新 UID replacement Ready；
- 校验新 Pod 落在 N0 且卡请求为 0；
- 校验 RepackRun 终态、plan、relocation 和 result 的每个关键字段；
- 在 `evidence/<timestamp>/` 收集 YAML、Events 和组件日志。

通过时的核心输出示例：

```text
PASS: 修复已通过真实集群验证
phase=Succeeded
reason=ExecutionCompletedWithAlternativePlacement
selectedNode=<N1>
actualNode=<N0>
replacementCards=0
freedNodes=<N0>
```

### 6.5 清理

验证资料后执行：

```bash
./cleanup.sh
```

清理脚本会删除固定名称的 RepackRun、整个专用测试 namespace，并移除本用例的污点和节点标签。`evidence/` 保留在本地，不会被删除。

## 7. YAML 文件说明

| 文件 | 用途 |
|---|---|
| `manifests/00-namespace.yaml` | 专用测试 namespace |
| `manifests/10-source-before.yaml` | 初始申请 4 卡、`OnDelete` 的 source StatefulSet |
| `manifests/20-receiver.yaml` | 在 N1 占用 4 卡的 receiver StatefulSet |
| `manifests/30-source-after.yaml` | 不再申请目标资源且只能落回 N0 的新 source 模板 |
| `manifests/40-repack-run.yaml` | 只允许释放 N0、最多迁移 1 个 PodGroup/4 卡的 Execute Run |

源 YAML 使用 `__PLACEHOLDER__`，脚本会根据 `config.sh` 生成 `.rendered/`，不需要手工修改 YAML。

## 8. 结果记录

| 字段 | 实测值 |
|---|---|
| 日期 | |
| 执行人 | |
| Kubernetes 版本 | |
| Volcano 版本/提交 | |
| Repack Engine 镜像 | |
| N0 / N1 | |
| RepackRun phase / reason | |
| selectedNode / actualNode | |
| replacement 卡数 | |
| result.freedNodes | |
| evidence 目录 | |
| 结论 | 通过 / 不通过 |

## 9. 失败排查

- `NoFragmentation` / `InsufficientImprovement`：检查 source/receiver 是否分别为 N0/N1 上的 4 卡 Pod，以及 source PodGroup 是否继承 `repack-fix-test=source`。
- `BenefitNotRealized`：先确认 replacement 的目标卡请求确实为 0；若为 0，优先确认 Engine 实际运行的 digest 是否包含修复。
- replacement 一直 Pending：查看 Pod Events，检查 N0 的测试标签、Volcano Scheduler 和 placement gate/提名日志。
- Run 长时间 Pending：检查 Engine 的 Execute cooldown，以及集群中是否有其他 Execute Run。
- 脚本中断：`run-test.sh` 会尽力清理 N2-N7 的 receiver-hold 污点并保留现场；排查后仍应执行 `./cleanup.sh`。
