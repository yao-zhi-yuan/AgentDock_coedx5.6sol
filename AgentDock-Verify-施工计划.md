# AgentDock Verify 施工计划

> 项目定位：面向 Eino Agent 的安全、可恢复、可验证、可回放的 Managed Agent Runtime。
>
> 交付目标：5 周内完成一个可以本地运行、自动验收、录制演示并开源的面试级 MVP。项目体现生产级设计思想和工程证据，但不宣称已经达到真实多租户或大规模生产水平。

## 1. 最终交付物

项目完成时必须同时交付以下内容：

1. 一个 Go 编写的 Agent Runtime：
   - `Run / Stage / Attempt / Event / Artifact` 显式建模；
   - PostgreSQL 持久化事件；
   - Worker Lease、Fencing Token、崩溃接管；
   - Pause、Resume、Cancel；
   - 正常执行和故障恢复使用同一条 Reconcile 路径。
2. 一个安全执行层：
   - 每次任务使用独立 Git Worktree；
   - Docker 隔离执行；
   - 默认关闭网络；
   - 非 root 用户、只读根文件系统；
   - CPU、内存、PID、超时和输出大小限制；
   - Tool Contract 和静态 Policy。
3. 一个 Harness 验证层：
   - Go 单元测试 Verifier；
   - 架构规则 Verifier；
   - 允许修改路径 Verifier；
   - 结构化失败证据；
   - 最多三轮有界修复。
4. 一个 Eino Coding Agent 真实负载：
   - 读取和检索仓库；
   - 在隔离工作区产生 Patch；
   - 根据 Verifier 证据修复；
   - 不允许任意访问宿主文件系统。
5. 一套可靠性能力：
   - Tool 超时、Tool 错误、非法结果、Worker Kill 故障注入；
   - Event Replay；
   - Model/Tool Cassette Replay；
   - OpenTelemetry Trace 和基础指标。
6. 一套工程证据：
   - 单元测试、集成测试、Chaos 测试；
   - Golden Trace；
   - Harness 开关 A/B 实验；
   - 架构文档、ADR、威胁模型；
   - 一键启动命令；
   - 5 分钟演示脚本和录屏。

## 2. 五周范围冻结

### 2.1 必须实现

- Go 单体仓库；
- PostgreSQL；
- 单 Controller、多 Worker 模型；
- Eino Adapter；
- Fake Reasoner 和 Replay Reasoner；
- Docker Sandbox；
- 一个 Go 示例仓库；
- 一个默认 Coding Workflow；
- 三个确定性 Verifier；
- 四类故障注入；
- OTel Trace；
- CLI；
- Recorded Mode 可以在没有模型凭证时完整演示。

### 2.2 明确不做

以下功能不得在五周 MVP 中实现：

- 多 Agent 协作；
- 通用 DAG 编辑器；
- Web 管理后台；
- Kubernetes Operator；
- Service Mesh；
- MCP / Skill 市场；
- Memory 产品化；
- Credential Proxy；
- 多租户和完整身份认证；
- 跨集群调度；
- wazero 和 Docker 双 Sandbox；
- LLM-as-judge 平台；
- 任意语言仓库；
- 任意 Agent 框架适配；
- GitHub App、自动提交 PR；
- 声称 exactly-once；
- 声称容器等同于强多租户安全边界。

### 2.3 可选项

只有阶段 0～8 全部通过后，才允许选择一个可选项：

- 简单的只读运行详情页面；
- wazero 纯函数 Tool 示例；
- Kubernetes Deployment 示例；
- MCP Tool Adapter；
- 第二种模型提供方。

## 3. 核心设计原则

### 3.1 编排层薄，运行和验证层厚

默认流程只有：

```text
Prepare → Reason → Act → Verify → Repair → Verify → Complete
```

流程不是项目的创新点。项目的深度必须体现在：

- 持久化状态；
- 崩溃恢复；
- 并发接管；
- 隔离执行；
- 确定性验证；
- 故障注入；
- Trace / Replay；
- 审计证据。

### 3.2 Eino 只负责模型与 Agent 组件

Eino 负责：

- ChatModel；
- Message；
- Tool Calling；
- Streaming；
- Agent 内部事件适配。

AgentDock Verify 自己负责：

- Managed Run 生命周期；
- Session / Run / Event 持久化；
- Worker Lease；
- Checkpoint / Resume；
- Tool Contract；
- Sandbox；
- Policy；
- Verifier；
- Fault Injection；
- Trace / Replay / Audit。

### 3.3 Event Log 是权威状态

每个 Reconcile 周期必须执行：

```text
Load events → Reduce state → Decide one action → Persist intent
→ Execute action → Persist result → Release/renew lease
```

进程内对象只能是缓存，不得作为权威状态。重启后必须仅依靠数据库事件和持久化 Artifact 恢复。

### 3.4 不宣称 exactly-once

MVP 的执行语义是：

```text
at-least-once execution
+ idempotent action
+ fencing token
= effectively-once for scoped idempotent actions
```

每个外部动作必须先写 `ActionPlanned` 事件，并携带稳定的 `action_id`。完成后写 `ActionCompleted` 或 `ActionFailed`。

MVP 只允许以下副作用：

- 临时 Worktree 内文件修改；
- 临时 Docker 容器；
- Event Store 追加；
- Artifact 目录写入。

不得修改用户原仓库，不得发布代码，不得调用具有真实业务副作用的外部 Tool。

### 3.5 验证优先使用确定性 Oracle

Verifier 优先级：

1. 测试命令；
2. JSON Schema；
3. 架构规则；
4. Diff 范围和安全规则；
5. LLM 判断，仅作为非阻塞补充。

任何“验证通过”都必须关联：

- `run_id`；
- `attempt_id`；
- `workspace_digest`；
- `spec_hash`；
- `verifier_version`；
- 原始证据 Artifact。

代码或 Spec 变化后，旧验证结果必须自动失效。

## 4. 建议技术栈

版本号在阶段 0 根据本机和官方当前稳定版本确认并固定，不在计划中写死。

- Go；
- Eino；
- PostgreSQL；
- `pgx`；
- Docker / Docker Compose；
- OpenTelemetry Go SDK；
- OTel Collector；
- Jaeger；
- Cobra 或同等级轻量 CLI；
- YAML 配置；
- SQL Migration；
- 标准库 `testing`；
- `go test -race`；
- `golangci-lint` 或项目选定的单一 Linter。

禁止为了“架构感”额外引入：

- Kafka；
- Redis；
- Temporal；
- Kubernetes；
- OPA；
- 服务注册中心；
- 独立消息队列。

## 5. 目标仓库结构

```text
agentdock-verify/
├── cmd/
│   ├── agentdock/                 # CLI
│   ├── controller/                # Controller 进程
│   └── worker/                    # Worker 进程
├── internal/
│   ├── domain/                    # Run/Event/State/Command
│   ├── controller/                # Reconcile 与状态迁移
│   ├── store/                     # PostgreSQL Event Store
│   ├── lease/                     # Lease、Heartbeat、Fencing
│   ├── reasoner/                  # Fake/Eino/Replay
│   ├── tools/                     # Tool Contract 与工具适配
│   ├── sandbox/                   # Docker Workspace
│   ├── policy/                    # ACL、Quota、路径和网络规则
│   ├── verifier/                  # 测试、架构、Diff Verifier
│   ├── repair/                    # 有界修复策略
│   ├── fault/                     # 故障注入
│   ├── replay/                    # Event/Cassette Replay
│   ├── telemetry/                 # OTel
│   └── artifact/                  # Artifact 管理
├── migrations/
├── configs/
├── examples/
│   ├── buggy-go-service/          # 示例被测仓库
│   └── scenarios/                 # HarnessSpec
├── testdata/
│   ├── cassettes/
│   ├── golden/
│   └── policies/
├── deployments/
│   └── docker-compose.yml
├── scripts/
├── docs/
│   ├── architecture.md
│   ├── state-machine.md
│   ├── event-model.md
│   ├── threat-model.md
│   ├── demo.md
│   ├── adr/
│   ├── acceptance/
│   └── status/
├── Makefile
├── go.mod
├── README.md
└── LICENSE
```

## 6. 核心领域模型

### 6.1 Run 状态

建议状态：

```text
Queued
Provisioning
Reasoning
Acting
Verifying
Repairing
WaitingApproval
Paused
Succeeded
Failed
Cancelled
```

终态只有：

```text
Succeeded / Failed / Cancelled
```

非法状态迁移必须由纯函数拒绝，模型不得直接写状态。

### 6.2 核心实体

`Run`

- `run_id`
- `scenario_id`
- `spec_hash`
- `desired_state`
- `observed_state`
- `current_attempt`
- `version`
- `created_at`
- `updated_at`

`Event`

- `run_id`
- `seq`
- `event_type`
- `payload`
- `idempotency_key`
- `causation_id`
- `correlation_id`
- `worker_id`
- `fencing_token`
- `created_at`

`Attempt`

- `attempt_id`
- `run_id`
- `number`
- `workspace_digest`
- `reason`
- `started_at`
- `finished_at`

`Lease`

- `run_id`
- `worker_id`
- `fencing_token`
- `expires_at`
- `heartbeat_at`

`Artifact`

- `artifact_id`
- `run_id`
- `attempt_id`
- `type`
- `digest`
- `path`
- `size`
- `created_at`

### 6.3 关键事件

最小事件集合：

```text
RunCreated
RunDesiredStateChanged
LeaseAcquired
LeaseRenewed
LeaseExpired
AttemptStarted
WorkspaceProvisionPlanned
WorkspaceProvisioned
ReasoningPlanned
ReasoningCompleted
ToolCallPlanned
ToolCallCompleted
ToolCallFailed
PatchProduced
VerificationPlanned
VerificationPassed
VerificationFailed
RepairScheduled
ApprovalRequested
ApprovalResolved
CheckpointSaved
RunSucceeded
RunFailed
RunCancelled
```

## 7. HarnessSpec 最小协议

示例：

```yaml
apiVersion: agentdock.dev/v1alpha1
kind: HarnessRun
metadata:
  name: fix-handler-layer-violation

target:
  adapter: eino
  repository: examples/buggy-go-service
  revision: main

task:
  prompt: |
    修复用户查询接口的错误，并保持项目架构约束。
  allowedPaths:
    - internal/
    - tests/

runtime:
  maxRepairRounds: 3
  timeout: 10m
  tokenBudget: 30000

sandbox:
  network: none
  cpu: 1
  memory: 512Mi
  pids: 128
  commandTimeout: 60s

verification:
  - type: go-test
    command: ["go", "test", "./..."]
  - type: architecture
    ruleSet: configs/architecture-rules.yaml
  - type: diff-scope

faults:
  - type: tool-error
    tool: repo.search
    occurrence: 2
    error: temporary_unavailable
```

阶段 0 只确定该协议，不实现通用 CRD 或版本迁移系统。

## 8. Codex 施工纪律

每个阶段都必须遵守以下流程：

1. 读取本计划和上一阶段状态报告；
2. 确认只实现当前阶段；
3. 先写或更新验收测试；
4. 实现最小代码；
5. 运行当前阶段自动验收；
6. 运行已有全量回归；
7. 执行 `git diff --check`；
8. 生成 `docs/status/phase-N.md`；
9. 报告：
   - 完成项；
   - 未完成项；
   - 验收命令和真实结果；
   - 已知限制；
   - 是否满足 Gate；
10. Gate 未通过时禁止进入下一阶段。

状态报告不得使用模糊表述，如：

- “应该可以”；
- “基本完成”；
- “理论上支持”；
- “测试看起来正常”。

必须记录真实命令、退出码和关键断言。

### 8.1 每阶段统一验收层次

每个阶段至少包含：

1. 静态检查；
2. 单元测试；
3. 集成测试；
4. 负向测试；
5. 人工演示；
6. 回归测试；
7. 验收证据归档。

如果某阶段不适用某一层，必须在状态报告中解释原因。

## 9. 阶段 0：建仓与架构冻结

**预计时间：0.5～1 天**

### 9.1 目标

把项目目标、边界、技术选择和验收口径固化，避免后续由 Codex 自主扩张范围。

### 9.2 实现内容

- 初始化 Git 仓库；
- 创建 Go Module；
- 创建目标目录结构；
- 创建 `README.md`；
- 创建：
  - `docs/architecture.md`
  - `docs/state-machine.md`
  - `docs/event-model.md`
  - `docs/threat-model.md`
  - `docs/adr/0001-event-log.md`
  - `docs/adr/0002-postgresql.md`
  - `docs/adr/0003-docker-sandbox.md`
  - `docs/adr/0004-eino-boundary.md`
- 固定依赖版本；
- 创建 Makefile 占位目标；
- 创建 Docker Compose 基础服务；
- 创建 `.env.example`，不得包含真实密钥。

### 9.3 必须回答的架构问题

- Eino 与自研 Runtime 的边界；
- 为什么使用 PostgreSQL；
- 为什么不使用 Temporal；
- 为什么不使用 Kafka；
- 为什么使用 Docker 而不是只使用 wazero；
- Event Replay 和 Execution Replay 的区别；
- Lease 过期后如何避免旧 Worker 继续写；
- Crash 在 Tool 执行前、中、后分别如何处理；
- 为什么不宣称 exactly-once；
- Docker Sandbox 的安全边界；
- 哪些内容属于 MVP，哪些属于扩展。

### 9.4 自动验收

```bash
go version
docker version
docker compose version
go mod verify
make doctor
git diff --check
```

`make doctor` 至少检查：

- Go 可用；
- Docker Daemon 可用；
- Compose 可用；
- 必要端口未冲突；
- 配置文件可以解析；
- 不存在必填但缺失的运行参数。

### 9.5 人工验收

- 阅读架构图，能够从用户请求一直讲到 Artifact；
- 能够明确指出 Eino 只占哪一层；
- 能够指出三个最高风险：
  - 恢复正确性；
  - Docker 隔离边界；
  - Replay 一致性。

### 9.6 Gate 0

- 所有文档存在；
- 范围冻结清单存在；
- `make doctor` 通过；
- 没有业务代码；
- 没有提前实现多 Agent、UI 或 Kubernetes。

## 10. 阶段 1：领域模型与纯状态机

**预计时间：1.5～2 天**

### 10.1 目标

在不接数据库、不接 Eino、不接 Docker 的情况下，用纯函数证明状态模型正确。

### 10.2 实现内容

- 定义 Run、Event、State、Command；
- 实现 `Reduce(events) -> State`；
- 实现 `Decide(state) -> Command`；
- 实现状态迁移校验；
- 实现 Fake Reasoner；
- 实现内存 Event Store；
- 实现单进程 Reconcile Loop；
- 实现最小 CLI：
  - `run create`
  - `run get`
  - `run step`
  - `run pause`
  - `run resume`
  - `run cancel`

### 10.3 自动验收

单元测试必须覆盖：

- 空事件得到初始状态；
- 同一事件序列重复 Reduce 结果一致；
- 非法事件顺序被拒绝；
- 终态不可再次推进；
- Pause 后不会产生新动作；
- Resume 后继续原路径；
- Cancel 后停止；
- 同一个 Command 使用相同 action ID；
- 事件乱序、缺失、重复时行为明确；
- Reducer 无时间、随机数、网络等非确定输入。

命令：

```bash
go test ./internal/domain/... ./internal/controller/...
go test -race ./internal/domain/... ./internal/controller/...
make demo-fake
git diff --check
```

### 10.4 负向验收

- 手动构造 `Succeeded → Acting`，必须失败；
- 重复追加相同 idempotency key，必须只保留一次；
- Pause 状态连续执行 10 次 Reconcile，不得产生副作用 Command；
- Fake Reasoner 返回非法 Tool Call，必须转换成受控失败。

### 10.5 人工演示

运行 `make demo-fake`，输出必须展示：

```text
RunCreated
AttemptStarted
ReasoningCompleted
VerificationPassed
RunSucceeded
```

然后执行 Pause / Resume 场景并展示状态恢复。

### 10.6 Gate 1

- Reducer 100% 确定；
- 状态迁移全部由程序控制；
- Fake Reasoner 能完成一次 Run；
- Race Test 通过；
- 尚未出现 Eino 和 Docker 依赖。

## 11. 阶段 2：PostgreSQL Event Store 与 Reconciliation

**预计时间：3～4 天**

### 11.1 目标

把进程内状态替换为可持久化、可事务化、可重建的权威事件日志。

### 11.2 实现内容

- SQL Migration；
- PostgreSQL Event Store；
- `runs`、`events`、`attempts`、`artifacts`、`leases` 表；
- `(run_id, seq)` 唯一约束；
- `(run_id, idempotency_key)` 唯一约束；
- Run version CAS；
- 事务内 Append Event；
- 从 Event Log 重建 State；
- Checkpoint Snapshot；
- Controller Reconcile；
- 进程重启后继续执行；
- Store 接口兼容内存实现。

### 11.3 数据一致性要求

- Event 和 Run version 在同一事务中提交；
- Append 时校验期望版本；
- stale version 返回明确冲突，不静默覆盖；
- Artifact 只在内容写完并计算 digest 后登记；
- Event Payload 不存模型密钥和环境凭证；
- 所有时间使用统一时区和数据库时间；
- Migration 可从空库执行；
- Migration 失败不能留下半完成 Schema。

### 11.4 自动验收

```bash
docker compose up -d postgres
make migrate
go test -tags=integration ./internal/store/... ./internal/controller/...
go test -race ./internal/store/... ./internal/controller/...
make test-rebuild-state
git diff --check
```

必须覆盖：

- 1000 条 Event 重建状态一致；
- 数据库断开后重连；
- 事务失败不会出现半条事件；
- 两个写入者竞争相同 Run，只有一个成功；
- 重复 idempotency key 不产生重复事件；
- Checkpoint 前后重建结果逐字段一致；
- 删除进程内缓存后仍可完成 Run。

### 11.5 Golden Trace 验收

保存一条固定 Event Log 到 `testdata/golden/`。

测试要求：

```text
Reduce(golden events) == golden state
```

比较必须逐字段进行，不允许只比较最终 Status。

### 11.6 人工演示

1. 创建 Run；
2. 执行到中间状态；
3. 停止 Controller；
4. 清空进程内缓存；
5. 重新启动；
6. 查看 Run 继续推进；
7. 查询 Event Log，确认 seq 连续。

### 11.7 Gate 2

- PostgreSQL 是唯一权威状态；
- 进程重启恢复通过；
- Golden Trace 通过；
- CAS 冲突测试通过；
- 没有依赖真实模型。

## 12. 阶段 3：Worker Lease、Fencing 与崩溃恢复

**预计时间：3～4 天**

### 12.1 目标

证明多 Worker 条件下任务不会被旧 Worker 和新 Worker同时推进，恢复与正常执行使用同一条路径。

### 12.2 实现内容

- Worker 注册；
- Run Lease；
- Heartbeat；
- Lease TTL；
- 单调递增 Fencing Token；
- Lease 过期接管；
- stale Worker 写入拒绝；
- `ActionPlanned / Completed / Failed`；
- Action Receipt；
- Worker Kill Chaos Harness；
- Pause、Resume、Cancel 跨进程生效。

### 12.3 Crash Window

必须分别测试：

1. 写 `ActionPlanned` 前崩溃；
2. 写 `ActionPlanned` 后、执行前崩溃；
3. 动作执行中崩溃；
4. 动作完成后、写 `Completed` 前崩溃；
5. 写 `Completed` 后崩溃；
6. Lease 刚过期时旧 Worker 恢复；
7. 两个 Worker 同时尝试接管。

每种窗口都要在 `docs/acceptance/recovery-matrix.md` 写明：

- 恢复行为；
- 是否允许重试；
- 幂等依据；
- 是否需要检查 Receipt；
- 最终期望状态。

### 12.4 自动验收

```bash
go test -tags=integration ./internal/lease/... ./internal/controller/...
go test -tags=chaos ./internal/controller/...
go test -race ./internal/...
make chaos-worker-kill
git diff --check
```

断言：

- 同一个 Run 同时最多一个有效 Lease；
- Fencing Token 单调增加；
- 旧 Worker 的 Event Append 被拒绝；
- 随机 Kill 100 次后 Run 均收敛到允许的终态；
- 已确认完成的幂等动作不重复计费、不重复产生 Artifact；
- 无法安全判断的动作进入 `WaitingApproval`，不得假装成功。

### 12.5 人工演示

1. 启动两个 Worker；
2. Worker A 获得 Lease；
3. Kill Worker A；
4. 等待 TTL；
5. Worker B 接管；
6. 重启 Worker A；
7. 展示 A 使用旧 Fencing Token 写入被拒绝；
8. Run 最终收敛。

### 12.6 Gate 3

- Chaos Kill 测试稳定通过；
- stale Worker 无法污染状态；
- 无 exactly-once 宣称；
- 恢复矩阵完整；
- 本阶段是项目最高优先级，不通过不得接 Eino。

## 13. 阶段 4：Docker Sandbox、Worktree 与 Policy

**预计时间：3～4 天**

### 13.1 目标

让所有代码读取、修改和测试都发生在一次性隔离工作区，不影响用户原仓库。

### 13.2 实现内容

- `Sandbox` 接口；
- Git Worktree Provider；
- Docker Sandbox；
- 非 root 用户；
- 只读 RootFS；
- 可写 Workspace；
- 默认 `network=none`；
- CPU、内存、PID、超时；
- stdout/stderr 大小限制；
- 路径规范化和目录穿越防护；
- 环境变量白名单；
- Sandbox Destroy；
- Tool Contract；
- 静态 YAML Policy；
- 审计事件。

### 13.3 Tool Contract

每个 Tool 至少声明：

- 名称和版本；
- 输入 JSON Schema；
- 输出 JSON Schema；
- 所需 Capability；
- 是否只读；
- 超时；
- 输出上限；
- 允许访问路径；
- 是否允许网络；
- 幂等性说明。

第一版只提供：

- `repo.list`
- `repo.read`
- `repo.search`
- `repo.apply_patch`
- `repo.test`

不得提供任意宿主 Shell。

### 13.4 自动验收

```bash
go test ./internal/policy/... ./internal/tools/...
go test -tags=integration ./internal/sandbox/...
make sandbox-security-test
git diff --check
```

必须覆盖：

- `../` 目录穿越被拒绝；
- 绝对路径访问宿主被拒绝；
- Symbolic Link 逃逸被拒绝；
- 网络请求失败；
- 超时进程被终止；
- Fork Bomb 受到 PID 限制；
- 超大输出被截断并产生审计记录；
- 容器内用户不是 root；
- RootFS 不可写；
- Workspace 可以写；
- Destroy 后容器和临时 Worktree 被清理；
- 用户原仓库内容和 Git Status 不变。

### 13.5 人工演示

1. 对原仓库记录 digest；
2. 创建 Sandbox；
3. 在 Worktree 修改文件；
4. 尝试访问宿主敏感路径；
5. 尝试访问网络；
6. 运行测试；
7. Destroy；
8. 再次比较原仓库 digest 和 Git Status。

### 13.6 Gate 4

- 原仓库零修改；
- 安全负向用例通过；
- 每个 Tool 有 Contract；
- 每次拒绝都有 Audit Event；
- README 明确 Docker 不等同于强多租户安全隔离。

## 14. 阶段 5：Eino Adapter 与真实 Coding Agent

**预计时间：2.5～3 天**

### 14.1 目标

接入 Eino，但保持 Runtime 不依赖 Eino 内部状态，证明 Eino 可替换。

### 14.2 实现内容

- `Reasoner` 接口；
- `FakeReasoner`；
- `EinoReasoner`；
- `ReplayReasoner`；
- Eino Message 和内部事件转换；
- Streaming Chunk 规范化；
- Tool Call 规范化；
- Token / Usage 提取；
- Provider 错误分类；
- Coding Agent System Contract；
- 一个示例 Go 仓库；
- 两个固定 Bug Scenario。

### 14.3 边界要求

`Reasoner` 输入：

- Messages；
- 可用 Tool Contracts；
- 当前任务摘要；
- 当前失败证据；
- Budget。

`Reasoner` 输出流：

- Text Delta；
- Tool Call；
- Usage；
- Finish；
- Error。

Controller 不得 import Eino 的具体 Agent 状态类型。Eino Adapter 必须位于独立包。

### 14.4 自动验收

```bash
go test ./internal/reasoner/...
go test -tags=integration ./internal/reasoner/...
make demo-eino-recorded
git diff --check
```

必须覆盖：

- Fake 和 Replay 模式不需要模型凭证；
- 同一 Cassette Replay 产生相同规范化事件；
- Streaming 中断转成可恢复错误；
- 非法 Tool Name 被拒绝；
- Tool 参数不符合 Schema 被拒绝；
- Token Budget 超限停止；
- Reasoner 不可直接写数据库或宿主文件。

### 14.5 Live Model 验收

Live Model 只作为人工验收，不进入 CI 硬门禁：

1. 使用外部安全配置的模型凭证；
2. 运行固定 Scenario；
3. 保存脱敏 Cassette；
4. 删除或确认 Cassette 中不存在凭证和隐私数据；
5. 后续 CI 使用 Cassette。

### 14.6 人工演示

- 使用 Live Eino Reasoner 完成一次仓库分析；
- 使用 Recorded Reasoner 重放同一执行；
- 展示 Runtime 无需修改即可切换 Fake/Eino/Replay。

### 14.7 Gate 5

- CI 不依赖模型凭证；
- Eino 被限制在 Adapter 层；
- Recorded Demo 可重复运行；
- 不得直接 Fork Eino Example 作为项目主体。

## 15. 阶段 6：Verification Hub 与有界修复

**预计时间：3～4 天**

### 15.1 目标

把“Agent 声称完成”替换为“确定性证据通过”，构成真正闭环。

### 15.2 实现内容

- `Verifier` 接口；
- Go Test Verifier；
- Architecture Verifier；
- Diff Scope Verifier；
- Verification Plan；
- 并行执行互不依赖的 Verifier；
- Evidence Schema；
- Verification Artifact；
- 版本绑定和失效机制；
- Repair Policy；
- 最多三轮修复；
- `WaitingApproval`；
- Harness 开关。

### 15.3 Evidence Schema

每条失败证据至少包含：

- `code`
- `severity`
- `verifier`
- `expected`
- `actual`
- `location`
- `artifact_ref`
- `remediation_hint`
- `workspace_digest`
- `verifier_version`

Agent 不允许自行提交“Verifier 已通过”事件。只有 Verifier Worker 可以提交验证结果。

### 15.4 示例任务

示例仓库至少包含：

1. 功能 Bug：测试失败；
2. 架构 Bug：Handler 越层访问 Repository；
3. Scope Bug：Agent 尝试修改允许目录之外的文件。

期望闭环：

```text
Patch v1
→ Unit Test Pass
→ Architecture Fail
→ Structured Evidence
→ Repair
→ Patch v2
→ All Verifiers Pass
→ Succeeded
```

### 15.5 自动验收

```bash
go test ./internal/verifier/... ./internal/repair/...
go test -tags=integration ./internal/verifier/... ./internal/repair/...
make e2e-recorded
git diff --check
```

必须覆盖：

- Test Pass 但 Architecture Fail 时 Run 不得成功；
- 任一阻塞 Verifier 失败都不能进入 Succeeded；
- 非阻塞 Verifier 只产生 Warning；
- Patch 变化后旧 Verification 自动失效；
- 失败证据可以被 Replay；
- Repair 超过三轮进入 Failed 或 WaitingApproval；
- 相同 Patch 不允许无限重复修复；
- Verifier 自身超时有明确失败类型；
- Verifier 输出不能伪造另一个 Verifier 的身份。

### 15.6 人工演示

完整展示：

1. Agent 生成第一版 Patch；
2. 单测通过；
3. 架构规则失败；
4. 查看结构化 Evidence；
5. Agent 第二轮修复；
6. 全部 Verifier 通过；
7. 展示最终 Patch 和 Artifact。

### 15.7 Gate 6

- “完成”只由 Verifier 决定；
- 修复循环有明确上限；
- 证据绑定代码 digest；
- 至少一个“单测通过但架构失败”的演示场景；
- 这是项目第二核心能力，不通过不得进入包装阶段。

## 16. 阶段 7：Fault Injection、Trace 与 Replay

**预计时间：3～4 天**

### 16.1 目标

证明 Harness 防线在真实故障下有效，并能复现一次运行。

### 16.2 实现内容

- Fault Injector 接口；
- Tool Timeout；
- Tool Error；
- Tool Invalid Result；
- Worker Kill；
- 可配置触发条件；
- OTel Span；
- Metrics；
- Model/Tool Cassette；
- Event Replay；
- Execution Replay；
- Audit Timeline；
- 脱敏规则。

### 16.3 Trace 结构

至少包含：

```text
Run
├── Reconcile
├── Reasoning
│   └── Model Stream
├── Tool Call
│   └── Sandbox Exec
├── Verification
│   ├── Go Test
│   ├── Architecture
│   └── Diff Scope
└── Repair
```

不得写入 Trace：

- 模型密钥；
- 数据库密码；
- 完整环境变量；
- 用户主目录；
- 未脱敏 Prompt 中的敏感内容。

### 16.4 指标

至少暴露：

- Run 数量及状态；
- Run Duration；
- Reconcile 次数；
- Repair Round；
- Tool Call 数量和错误率；
- Token Usage；
- Lease Takeover 数量；
- Policy Denial 数量；
- Verification Pass/Fail；
- Replay Divergence。

### 16.5 自动验收

```bash
docker compose up -d postgres otel-collector jaeger
go test ./internal/fault/... ./internal/replay/... ./internal/telemetry/...
go test -tags=chaos ./internal/...
make e2e-chaos
make e2e-replay
git diff --check
```

必须覆盖：

- Tool 503 不超过重试预算；
- Tool Timeout 触发取消；
- 非法 Tool 输出被 Schema 拒绝；
- Worker Kill 后接管；
- Replay 不调用真实模型和真实 Tool；
- 同一 Cassette 重放事件一致；
- Replay 不一致产生明确 Divergence；
- Trace Span 父子关系正确；
- 故障注入事件可审计；
- Cassette 扫描不存在凭证。

### 16.6 人工演示

1. 运行基线 Scenario；
2. 注入 Tool 503；
3. 展示有界重试和降级；
4. 中途 Kill Worker；
5. 展示 Lease Takeover；
6. Run 完成后打开 Jaeger；
7. 使用同一 Run ID Replay；
8. 展示 Replay 不再调用模型。

### 16.7 Gate 7

- 四类故障均可复现；
- Replay 完整可运行；
- OTel Trace 可查看；
- 没有敏感信息泄漏；
- 故障下行为可预测，不出现无限循环。

## 17. 阶段 8：系统验收、量化实验与开源交付

**预计时间：3～5 天**

### 17.1 目标

停止增加功能，把已有能力变成可信的工程证据和面试演示。

### 17.2 场景集

至少准备 6 个固定 Scenario：

1. 正常一次通过；
2. 单测失败后修复；
3. 单测通过但架构失败；
4. Tool 503；
5. Tool Timeout；
6. Worker Kill 恢复。

可选：

7. 非法路径访问；
8. Token Budget 耗尽；
9. Replay Divergence。

### 17.3 A/B 实验

对相同 Scenario、相同模型版本、相同仓库 Revision 比较：

`Harness Off`

- Agent 生成 Patch；
- 只看 Agent 自报完成。

`Harness On`

- Sandbox；
- Verifier；
- 有界修复；
- Trace；
- Policy。

至少记录：

- 最终任务成功率；
- 首轮通过率；
- 自愈率；
- 平均修复轮数；
- 平均 Tool Call 数；
- Token 使用量；
- 总耗时；
- Policy 拒绝次数；
- Crash 恢复成功率。

不得只展示对 Harness 有利的数据。必须记录：

- Harness 增加的延迟；
- Harness 增加的 Token；
- Sandbox 启动成本；
- 误报或不稳定 Verifier；
- 失败场景。

### 17.4 性能验收

MVP 不做大规模压测，但至少执行：

- 10 个并发 Run；
- 2 个 Worker；
- 每个 Run 100～1000 条 Event；
- 事件重建耗时；
- Lease 竞争；
- 数据库连接耗尽保护；
- Artifact 输出上限。

性能目标由首次基线确定，不虚构生产 SLO。README 中明确测试机器和环境。

### 17.5 安全验收

完成 `docs/threat-model.md`：

- 资产；
- 信任边界；
- 攻击面；
- Tool 注入；
- Prompt 注入；
- 路径逃逸；
- 容器逃逸风险；
- 凭证泄漏；
- Replay 数据泄漏；
- 拒绝服务；
- 已实现控制；
- 未解决风险。

执行：

```bash
make security-test
```

### 17.6 全量自动验收

最终必须存在统一入口：

```bash
make doctor
make lint
make test
make test-race
make test-integration
make test-chaos
make security-test
make e2e-recorded
make e2e-replay
git diff --check
```

如果 Live Model 测试需要凭证，必须作为独立可选命令：

```bash
make e2e-live
```

它不得影响默认 CI。

### 17.7 开源交付

README 必须包含：

- 一句话定位；
- 项目解决的问题；
- 不解决的问题；
- 架构图；
- 五分钟 Quickstart；
- Recorded Mode；
- Live Eino Mode；
- 故障演示；
- Trace 截图；
- 安全边界；
- 一致性语义；
- 与 Eino 的边界；
- 与传统 CI、AgentOps、Agent Infra 的关系；
- Roadmap。

其他交付：

- LICENSE；
- CONTRIBUTING；
- SECURITY；
- 示例配置；
- 清理后的 Commit 历史；
- 无内部域名、内部代码、内部凭证、内部业务名；
- 无复制的内部实现；
- 所有示例数据自行构造。

### 17.8 演示脚本

5 分钟演示顺序：

1. 30 秒：问题和架构；
2. 45 秒：创建一个 Run；
3. 60 秒：第一版 Patch、单测通过、架构失败；
4. 45 秒：结构化证据驱动第二轮修复；
5. 45 秒：Kill Worker，展示恢复；
6. 30 秒：Tool 503 有界重试；
7. 30 秒：Jaeger Trace；
8. 30 秒：Replay；
9. 15 秒：总结取舍与局限。

### 17.9 Gate 8

- 所有默认验收命令通过；
- Recorded Demo 可在干净环境运行；
- README 中没有生产规模夸大；
- 所有内部信息已清理；
- 失败和局限被诚实记录；
- 演示在 5 分钟内完成；
- 项目达到可开源状态。

## 18. 五周日历

| 周次 | 阶段 | 主要结果 |
|---|---|---|
| 第 1 周 | 阶段 0、1、2 前半 | 架构冻结、纯状态机、Event Store |
| 第 2 周 | 阶段 2 后半、3 | PostgreSQL、Lease、Fencing、崩溃恢复 |
| 第 3 周 | 阶段 4、5 | Docker Sandbox、Policy、Eino Agent |
| 第 4 周 | 阶段 6、7 | Verifier、修复闭环、Fault、Trace、Replay |
| 第 5 周 | 阶段 8＋缓冲 | 系统验收、A/B、文档、演示、开源清理 |

每周建议保留约 20% 时间处理未预期问题。阶段 3 或阶段 6 延误时，优先砍掉可选功能，不得削弱恢复测试或 Verifier 证据。

## 19. 阶段失败时的降级顺序

若五周时间不足，按以下顺序砍功能：

1. 取消 Web UI；
2. 取消 Kubernetes 示例；
3. 取消第二模型 Provider；
4. Fault Injection 从四类减为三类；
5. 性能测试从 10 Run 降到 5 Run；
6. Execution Replay 只支持 Recorded Reasoner；
7. Pause/Approval UI 只保留 CLI。

不得砍：

- Event Store；
- Reducer / Reconcile；
- Lease / Fencing；
- Worker Kill 恢复；
- Docker Sandbox；
- Verifier；
- 修复上限；
- Recorded Demo；
- README 中的限制说明。

## 20. Codex 每阶段任务模板

每次只把以下模板中的一个阶段交给 Codex：

```text
你正在实现 AgentDock Verify 的阶段 N。

请完整阅读：
1. AgentDock-Verify-施工计划.md
2. docs/status/phase-(N-1).md
3. 当前阶段引用的 ADR 和协议

约束：
- 只实现阶段 N，不开始阶段 N+1；
- 不增加范围冻结清单之外的能力；
- 先补验收测试，再实现；
- 保持 Eino 在 Adapter 边界内；
- 不修改用户原仓库；
- 不声称 exactly-once 或生产级强隔离；
- 不使用真实模型作为 CI 必需条件。

完成后：
1. 运行阶段 N 的全部验收命令；
2. 运行已有全量回归；
3. 运行 git diff --check；
4. 生成 docs/status/phase-N.md；
5. 报告真实命令、退出码、关键断言、已知限制；
6. 如果 Gate 未通过，继续修复，不进入下一阶段。
```

## 21. 最终面试验收问题

项目结束前，必须能够基于实际代码回答：

1. Eino 已经有 Runner/Interrupt，为什么还需要这个 Runtime？
2. Event Log 为什么是权威状态？
3. Reconcile 和普通 while-loop 有什么差别？
4. Worker 在 Tool 执行完成但 Completion Event 未落库时崩溃怎么办？
5. 为什么不说 exactly-once？
6. Lease 为什么还需要 Fencing Token？
7. 为什么 Coding Agent 使用 Docker 而不是 wazero？
8. Docker Sandbox 的安全边界在哪里？
9. 如何阻止 Agent 伪造 Verifier 证据？
10. 为什么单测通过仍不能说明任务完成？
11. Event Replay 和模型/工具 Cassette Replay 的区别？
12. Replay 出现 Divergence 怎么定位？
13. Harness 带来了哪些额外成本？
14. 如果扩展到 K8s 和多租户，哪些组件需要替换？
15. 如何接入 MCP、Credential Proxy 和 Egress Policy？
16. 哪些指标能够证明 Harness 真正有效？
17. 当前项目哪些能力只是原型，哪些已经通过故障测试？

只有能够从代码、测试、Trace 和报告中给出证据，才算项目真正完成。
