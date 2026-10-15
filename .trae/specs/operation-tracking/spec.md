# 操作标识与可恢复进度（Operation Tracking & Resumable Progress）— 产品需求文档

## Overview

- **Summary**：为每个经 API 接纳的存储/共享类变更分配稳定的操作标识（operation ID），Ansible playbook 与本地 ops 两条执行路径在同一操作下发布带单调序号的有序步骤与唯一终态；浏览器按操作标识订阅/查询进度，实现跨标签页隔离、断线/SSE 重连续传不重复、慢订阅者回放恢复、客户端断连后后台执行结果可确定查询、过期操作明确不可恢复。
- **Purpose**：消除当前全局 `ansible.progress` 主题导致的跨窗口步骤串流，消除 `PublishNoCache` 无历史导致的重连丢步骤，消除网络中断后"变更已接纳但结果未知"的状态。
- **Target Users**：在多个标签页并发管理 dumpstore 的管理员；通过不稳定网络（无线、VPN、笔记本休眠）操作 NAS 的管理员；依赖 journald/审计日志排查变更的运维人员。

## Goals

- 变更接纳即生成稳定操作标识，标识可订阅（SSE）、可查询（REST）、可关联日志审计。
- 两条执行路径（`internal/ansible`、`internal/ops`）统一到同一操作抽象：同一步骤序号空间、同一终态机。
- 浏览器严格只展示本请求的步骤；SSE 重连（含 broker 丢弃慢订阅者触发的重连）从最后确认位置补齐且不重复。
- 成功、任务失败、执行超时、客户端断开后后台继续执行四种情形均留下可确定查询的最终结果；过期结果以明确的不可恢复响应返回。
- 保留单窗口正常网络下的全部现有表现、HTTP 成功/错误响应内容、普通缓存主题与 `jobs.update` 行为。
- 进度历史严格有界；日志与审计可用操作标识串联接纳请求、步骤与终态。

## Non-Goals

- 不改变 jobs 长任务通道（`zfs send | recv`、`zfs rewrite`）：`/api/jobs*`、`jobs.update`、作业持久化语义保持不变。
- 不做操作记录的跨重启磁盘持久化：操作历史为进程内有界数据；服务重启时进行中的 playbook/ops 子进程随服务终止，不存在孤儿变更（jobs 通道的既有中断语义另当别论）。
- 不新增操作的列表/检索 UI 或主动通知中心；本期仅支持按标识查询与订阅。
- 不引入构建步骤或前端框架（沿用 Vanilla JS + 事件委托约定）。
- 不改变任何 playbook 内容、校验规则与系统资源所有权模型。

## Background & Context

现状证据（代码库勘察）：

- 两条写路径的步骤都发布到**单一全局主题** `ansible.progress`：
  [handlers.go](file:///Users/vance/project/mineProject/swe/09101010/project-01/internal/api/handlers.go#L253-L266) 中 `runOp`/`runLocal` 均调用 `h.broker.PublishNoCache("ansible.progress", step)`，载荷 `ansible.TaskStep` 不含任何操作归属字段。
- 前端唯一过滤条件是"op-log 对话框打开且关闭按钮禁用"：
  [loader.js](file:///Users/vance/project/mineProject/swe/09101010/project-01/static/js/loader.js#L191-L199) —— 任何标签页触发的步骤都会进入另一个标签页正在运行的对话框。
- `PublishNoCache` 不保留历史：
  [broker.go](file:///Users/vance/project/mineProject/swe/09101010/project-01/internal/broker/broker.go#L114-L126)；SSE 事件也不写 `id:` 字段
  （[handlers.go](file:///Users/vance/project/mineProject/swe/09101010/project-01/internal/api/handlers.go#L494-L508)），浏览器无法用 Last-Event-ID 续传。
- 慢订阅者通道写满即被 broker 关闭（[broker.go](file:///Users/vance/project/mineProject/swe/09101010/project-01/internal/broker/broker.go#L78-L96)），重连后缺口永久存在。
- 执行已对客户端断连免疫（`context.WithoutCancel`：
  [runner.go](file:///Users/vance/project/mineProject/swe/09101010/project-01/internal/ansible/runner.go#L139)、
  [ops.go](file:///Users/vance/project/mineProject/swe/09101010/project-01/internal/ops/ops.go#L113-L116)），但终态只写进当次 HTTP 响应；响应丢失即结果未知，且无任何按标识查询的途径。
- 超时只表现为普通 500 文本（ansible 5 分钟、ops 每步 5 分钟），无独立终态分类。
- 全部 `runOp`/`runLocal` 调用（共 47 个执行点，分布在 12 个 handler 文件）仅存在于 `internal/api`；同一请求可能连续执行两个 playbook（如 `tlsAcmeIssue`、`initSamba`），天然属于"一个接纳请求 = 一个操作"。
- jobs 通道已有成熟的有界保留/持久化模型（[manager.go](file:///Users/vance/project/mineProject/swe/09101010/project-01/internal/jobs/manager.go#L30-L39) retain=50），可作为操作注册表单有界策略的参照，但不共享实现。
- `req_id` 已由日志中间件注入 ctx（[middleware.go](file:///Users/vance/project/mineProject/swe/09101010/project-01/internal/logging/middleware.go#L28-L35)），可与操作标识双向关联。

## Functional Requirements

### 接纳与标识

- **FR-1**：变更请求在通过全部输入校验、真正开始执行前，由服务端建立操作记录；未通过校验的请求（4xx）不产生操作。
- **FR-2**：操作 ID 稳定且唯一。浏览器对变更请求生成客户端 ID（`X-Operation-ID: op_<UUID>` 形态，受严格正则约束）；服务端优先采用客户端 ID，无该头时自行生成 24 位 hex ID。
- **FR-3**：幂等接纳：同一 ID 在历史保留期内重复 POST 不重复执行变更——操作仍在运行返回 409 规范响应；已到终态则按记录的终态重放确定性结果（成功 200 / 失败或超时 500 的规范 JSON）；已过期返回 410。
- **FR-4**：每个被接纳的变更，其**首个** HTTP 响应携带 `X-Operation-ID` 响应头（在 `WriteHeader` 之前写入）；正常成功/失败响应体与状态码形状保持现状。
- **FR-5**：一个 HTTP 请求内连续调用多个 playbook/ops 执行（如 tls 签发后写配置、smb init 后 apply）归属同一操作，步骤共享同一序号空间，终态按"失败优先于成功、超时优先于普通失败"唯一确定。

### 统一步骤与终态

- **FR-6**：`runOp` 与 `runLocal` 的每个步骤写入所属操作：步骤带操作内单调递增、无空洞的整数序号 `seq`（从 1 开始），字段保持 `name/status/msg` 现有形状。
- **FR-7**：操作终态机为 `running → succeeded | failed | timeout`，终态一次性写入且之后不可变；终态记录包含 `status`、`error`（失败/超时时的错误文本）、全部保留步骤、`started_at`、`finished_at`、`action`、`target`、`req_id`。
- **FR-8**：执行超时被分类为 `timeout` 终态（ansible playbook 超时、ops 单步超时），错误文本保留；成功为 `succeeded`；其余执行错误为 `failed`。
- **FR-9**：客户端断连不取消执行（沿用 WithoutCancel）；处理协程走到终态后写入操作记录，此后任意时刻可按 ID 查询到确定结果。

### 订阅、查询与恢复

- **FR-10**：新增 `GET /api/operations/{id}`：返回操作完整当前记录（running 亦可查询）；ID 形态非法或从未存在返回 404；已淘汰过期返回 `410 Gone`，响应体为明确的不可恢复结果（含 `recoverable:false`）。
- **FR-11**：新增 `GET /api/operations/{id}/events`（SSE，同源 cookie 鉴权，与现有 /api 一致）：
  - 连接建立后先发送一个 `op.snapshot` 事件（操作完整记录，全量替换语义），随后实时发送 `op.step`（含 seq）与终态 `op.finished`；
  - 每条 SSE 事件写 `id: <seq>` 行；支持浏览器重连自动携带的 `Last-Event-ID` 头与等价 `?since=<n>` 参数，仅重放序号之后的事件；缺口早于保留窗口时以 `op.snapshot` 全量补齐；
  - 慢订阅者通道满时按现有 broker 同样策略丢弃并断开该 SSE 连接，浏览器自动重连后走同一回放路径，不丢步骤、不产生永久缺口；
  - 对已终态操作的订阅：发送 snapshot（及缺失时的 finished）后由客户端关闭；服务端保持 30s 心跳；过期 ID 返回 410。
- **FR-12**：前端每个变更对话框只订阅自己那次请求的操作事件：
  - snapshot 全量替换步骤列表；`op.step` 按 seq 去重后增量追加；
  - fetch 正常返回时仍以 HTTP 响应中的 `tasks` 做最终整表渲染（保持现有最终表现）；
  - fetch 因网络错误失败（响应不可达）时，对话框进入"正在恢复结果"状态，用已知客户端 ID 轮询 REST 并重连 SSE 直到终态或 410；终态后渲染与现状一致的步骤与错误，410 时显示明确的不可恢复提示。
- **FR-13**：前端移除对全局 `ansible.progress` 主题的监听；全局主题不再有新的发布者（主题名保留在 ValidTopics 中不破坏旧订阅者连接，仅不再产出数据）。

### 有界性与可观测

- **FR-14**：进度历史严格有界：单操作保留步骤设条数与单条消息字节上限（环形淘汰，序号不回收）；注册表按"终态操作最大条数 + 终态后最长存活时间"淘汰；过期 ID 以有界 tombstone 环支持 410 判定。
- **FR-15**：新增结构化日志：接纳（`operation accepted`，含 op_id/req_id/action/target）、每个步骤（debug 级，含 op_id/seq/name/status）、终态（`operation finished`，含 op_id/status/duration_ms/error）；既有 `audit` 审计行自动带上 op_id（与 req_id 同现），可在 journald 中用 op_id 串联接纳、步骤、终态。
- **FR-16**：Prometheus 指标覆盖操作接纳数、按终态分类的完成数、当前进行中操作数（`/metrics` 常开，沿用既有指标风格）。

## Non-Functional Requirements

- **NFR-1（兼容性）**：正常网络下单窗口操作表现与当前像素级一致：运行中实时追加步骤、结束时以最终响应整表渲染、成功 toast、失败红色错误条；成功 JSON 与 `apiError` JSON 的字段/状态码不增不改（操作 ID 仅经响应头传递）。
- **NFR-2（隔离性）**：两个标签页并发变更时，任一标签页的 SSE 事件流、对话框步骤、恢复过程互不串扰。
- **NFR-3（健壮性）**：订阅/重连/恢复路径对网络中断、服务端慢消费者断开、事件重复到达、事件乱序到达均收敛到一致视图；所有并发访问线程安全。
- **NFR-4（资源）**：高频变更压测下（测试中以 10k 操作模拟）注册表内存占用恒定有界；步骤消息截断不影响执行本身。
- **NFR-5（安全）**：操作 ID 与 since 参数严格白名单校验；操作端点位于 `/api/` 鉴权之后；extra-vars 仍只记键不记值；历史中不出现密码等敏感字段（沿用现有步骤载荷约定）。
- **NFR-6（可测）**：所有新单元测试不依赖外部进程（`go test ./...` 可跑）；VM 集成测试覆盖端到端恢复场景；`go build ./...`、`go vet ./...`、`go test ./...` 全绿。

## Constraints

- **Technical**：Go 1.22+ ServeMux 模式路由；Vanilla JS 无构建；沿用 `internal/broker` 现有非阻塞投递/慢消费者断开模型扩展有序回放原语；执行器对客户端取消免疫的既有行为不变。
- **Business / 流程**：全量模板渲染等 AGENTS.md 红线不变；变更覆盖 ~47 个执行点，须机械一致地迁移，不得在调用点引入临时分叉；文档（wiki/API-Reference、wiki/Architecture、docs/index.html 端点清单）随路由同步更新。
- **Dependencies**：无新外部依赖；UUID 由浏览器 `crypto.randomUUID()` 生成，Go 端沿用 `crypto/rand`。

## Assumptions

- 进行中的变更不跨服务重启存活；进程内有界历史足以满足"可确定查询"的时间窗（终态后默认保留 1 小时且总数封顶）。
- 单标签页内 op-log 为模态对话框，同一时刻只有一个变更处于前台运行；后台 jobs（send/rewrite）不经此对话框与操作通道。
- 浏览器支持 EventSource 的 Last-Event-ID 与 `crypto.randomUUID`（当前支持的现代浏览器均可）；不支持时回退为 REST 轮询恢复。
- 默认有界参数（实现为命名常量、便于评审）：单操作 ≤ 500 步骤、单条 msg ≤ 16 KiB；终态操作保留 ≤ 200 条且终态后 ≤ 1 小时；tombstone 环 ≤ 256 条、存活 ≤ 1 小时；SSE 慢消费者通道 32。

## Acceptance Criteria

### AC-1: 被接纳变更获得稳定操作标识并经响应头返回
- **Type**: `rule`
- **Given**：管理员通过校验提交一个会触发 playbook 或 ops 的变更
- **When**：服务端接纳并开始执行
- **Then**：首个响应包含形态合法且全局唯一的 `X-Operation-ID`，且 GET 该 ID 能取到 running/终态记录；校验失败（4xx）响应无该头且无记录
- **Pass Condition**：集成测试断言成功/失败响应均带头且 GET 可查；400 请求无头无记录
- **Evidence**：`tests/integration` 新增用例；`internal/operations` 与 `internal/api` 单元测试

### AC-2: 两条执行路径在同一操作下发布有序步骤与唯一终态
- **Type**: `rule`
- **Given**：任一 runOp 或 runLocal 变更（含一请求两个 playbook 的场景）
- **When**：步骤依次完成、操作结束
- **Then**：记录中步骤 seq 从 1 单调连续；终态恰为 succeeded/failed/timeout 之一且只写一次，再 Finish 为 no-op
- **Pass Condition**：单元测试断言序号连续、终态一次冻结、双 playbook 请求共享序号空间且失败优先
- **Evidence**：`internal/operations` 单元测试；tls/smb 类处理路径走查证据

### AC-3: 跨标签页进度严格隔离
- **Type**: `rule`
- **Given**：标签页 A、B 几乎同时发起不同变更并各自打开操作日志
- **When**：两边步骤交叉到达
- **Then**：A 的订阅（`/api/operations/{A-id}/events`）只收到 A 的步骤，B 同理；两个对话框内容互不包含对方步骤
- **Pass Condition**：集成/浏览器验证两操作事件流零交叉；前端事件处理器仅接受本对话框绑定 ID
- **Evidence**：集成测试并发两个变更并分别消费各自 SSE；前端代码审查证据

### AC-4: SSE 重连按最后确认位置补齐且不重复
- **Type**: `rule`
- **Given**：操作进行中客户端已确认到 seq=k
- **When**：网络中断后 EventSource 自动重连（携带 Last-Event-ID=k）
- **Then**：服务端只重放 seq>k 的 step/finished；若 k 早于保留窗口，先给 op.snapshot 全量；客户端视图无重复步骤、无缺口
- **Pass Condition**：单元测试覆盖 since=k 精确回放、since 越界 snapshot、重复 seq 去重；集成测试中断开 SSE 后重连断言步骤集合完整且唯一
- **Evidence**：`broker.Journal` 与 operations SSE 测试、集成测试输出

### AC-5: 慢订阅者被断开后沿同一恢复路径继续
- **Type**: `rule`
- **Given**：某 SSE 消费者不读取事件直至通道满
- **When**：broker 按现有策略关闭其通道
- **Then**：HTTP SSE 连接结束→EventSource 自动重连→按 Last-Event-ID 回放补齐，期间发布的步骤零丢失
- **Pass Condition**：单元测试制造慢消费者，断言其被断开且重连后拿到完整序号序列
- **Evidence**：`broker.Journal` 慢消费者测试

### AC-6: 断连后后台执行的四类结果均可确定查询
- **Type**: `rule`
- **Given**：变更执行期间客户端 TCP 断开（或 POST 响应丢失），浏览器持有客户端生成的操作 ID
- **When**：操作分别走到成功、任务失败、执行超时
- **Then**：GET 操作记录分别得到 status=succeeded/failed/timeout，含 error（失败/超时）、全部步骤、finished_at；重复 POST 同一 ID 不二次执行（运行中 409、终态重放同结果、过期 410）
- **Pass Condition**：集成测试中断 HTTP 连接后轮询到三类确定终态；重复 POST 断言底层动作只发生一次（副作用计数）
- **Evidence**：集成测试 + 单元测试（幂等接纳状态机）

### AC-7: 过期操作返回明确不可恢复结果
- **Type**: `rule`
- **Given**：操作记录终态后被有界策略淘汰（tombstone 尚在或更久）
- **When**：客户端 GET 或订阅该 ID
- **Then**：返回 410，JSON 明确表达不可恢复（`recoverable:false`、稳定错误文案）；前端显示不可恢复提示而非无限等待
- **Pass Condition**：单元测试驱动淘汰后 GET/SSE 均 410；前端恢复逻辑对 410 有终止渲染
- **Evidence**：operations 管理器 410 测试；前端代码走查

### AC-8: 现有单窗口表现、HTTP 内容、缓存主题与 jobs 行为保持
- **Type**: `rule`
- **Given**：正常网络的单个标签页
- **When**：完成一次成功与一次失败变更，并观察数据刷新与 jobs 页面
- **Then**：成功响应体字段/状态码与 `apiError` 失败体（error/tasks）形状不变；实时步骤与最终整表渲染行为不变；`*.query`/`poolstatus`/`iostat` 等缓存主题语义不变；`jobs.update`、`/api/jobs*` 行为与载荷不变
- **Pass Condition**：对比改动前后响应 JSON（集成测试断言既有字段仍存在且无新增必填字段）；jobs 既有集成用例不改动且通过
- **Evidence**：集成测试；`git diff` 确认 jobs/cache 相关代码无行为改动

### AC-9: 进度历史严格有界
- **Type**: `rule`
- **Given**：持续接纳远超保留上限的操作与超大步骤消息
- **When**：注册表执行淘汰
- **Then**：终态记录数 ≤ 上限、TTL 到期被清；单操作步骤条数/消息字节被截断（序号不回收）；tombstone 不超环容量；内存不随累计操作数增长
- **Pass Condition**：单元测试注入 10,000 个完成操作断言存量恒定；大 msg 被截断为上限
- **Evidence**：operations 有界性测试

### AC-10: 日志与审计可用操作标识完成三段关联
- **Type**: `rule`
- **Given**：一次被接纳的变更
- **When**：检查 journald 日志输出
- **Then**：存在带同一 op_id 的 accepted 行、逐步 debug 行（含 seq）、finished 行；既有 audit 行含 op_id 与 req_id；请求行 req_id 可与之关联
- **Pass Condition**：单元/人工捕获日志断言四类记录 op_id 一致且 audit 含 req_id+op_id
- **Evidence**：日志捕获测试或 dev 模式冒烟输出

### AC-11: 浏览器网络中断恢复体验
- **Type**: `rubric`
- **Dimension**：恢复过程的可理解性与一致性
- **Scale**: 1-5
- **Anchors**：1 = 中断后对话框停在通用错误或永久转圈、结果未知；3 = 能恢复终态但过程无提示或偶发重复/缺失步骤；5 = 中断即刻显示"正在恢复结果"，恢复后步骤完整不重复、终态与正常路径渲染一致，过期则显示明确不可恢复文案
- **Pass Threshold**: >= 4
- **Evidence**：浏览器断网/丢响应演练记录与前端代码走查

### AC-12: 设计与实现的一致性、简洁性与最小侵入
- **Type**: `rubric`
- **Dimension**：抽象统一性与调用点迁移质量
- **Scale**: 1-5
- **Anchors**：1 = 两条路径出现分叉的进度机制或临时补丁式串流；3 = 抽象统一但调用点模板不一致/存在无操作裸跑的写路径；5 = 两条路径仅经同一操作接口发布步骤，~47 个执行点以同一机械模式迁移，无裸跑、无全局串流回退路径残留（nil 防御除外），无新外部依赖
- **Pass Threshold**: >= 4
- **Evidence**：代码走查；`go vet`；全局 grep 证据

### AC-13: 构建与测试门禁
- **Type**: `rule`
- **Given**：完整改动
- **When**：运行 `go build ./...`、`go vet ./...`、`go test ./...`
- **Then**：全部通过；新增单元测试稳定无 flake；集成测试在已部署 VM 上通过（环境受限时记录为 blocked 并说明）
- **Pass Condition**：三条命令退出码 0；评审复核测试输出
- **Evidence**：命令输出归档于 tasks 完成证据与 review.md

## Open Questions

- [x] 跨重启是否持久化操作历史？——本期不做（见 Non-Goals）；若后续需要，可沿 jobs 的 JSON 记录模式扩展，不影响本期接口。
- [x] 操作 ID 由谁生成？——客户端生成以消除"响应丢失即不可恢复"，服务端对无 ID 客户端兜底生成。
- [x] 全局 `ansible.progress` 如何处置？——停止发布并移除前端监听；主题名保留以兼容旧连接握手。
- [ ] 终态保留默认值（200 条 / 1 小时）是否符合运维预期？可在实现评审时调整命名常量，不改接口。
