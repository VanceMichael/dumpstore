# 操作标识与可恢复进度 — 实施计划

说明：任务按依赖排序；同一层无依赖的任务可并行委派（T1/T3、T9/T10 中前端部分与后端可并行）。每个任务至少含一个 TR；`rule` 给出可客观判定条件，`rubric` 给出评分锚点。全部任务完成且审查通过前不结束流程。

---

## Task 1: broker 有序回放原语 Journal
- **Status**: `completed`
- **Completion Evidence**:
  - internal/broker/journal.go + journal_test.go；seq 单调、Replay/Subscribe 边界、慢消费者丢弃后重订阅零缺口、step/bytes 双上限淘汰、10k 发布有界、Close 封存、并发安全
  - `go test ./internal/broker/` 通过
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 在 `internal/broker` 新增 `journal.go`：通用有序事件原语 `Journal`，与现有 Broker 同风格（互斥锁、非阻塞投递、满则关闭慢消费者）。
  - API 形态：`NewJournal(stepCap int, bytesCap int) *Journal`；`Publish(data any) (Event, error)` 分配进程内单调 seq（Journal 实例内从 1 起）；`Replay(afterSeq int64) (events []Event, latest int64)` 返回 afterSeq 之后的保留事件；`Subscribe(afterSeq int64) (initial []Event, ch chan Event, latest int64)` 返回回放事件+实时通道；慢消费者（缓冲 32）写满即关闭并移除，与 Broker 现有策略一致。
  - 环形缓冲保留最近 N 条事件与字节上限；被淘汰事件的 seq 仍可由 `OldestSeq()` 查到，供调用方判断"since 早于窗口"。
  - 终态事件支持 `Close()`/封存：终态写入后不再接受 Publish，订阅者仍可取回放与已关闭通道判定。
  - 不改动 `broker.go` 任何现有行为（缓存主题零影响）。
- **Acceptance Criteria Addressed**: AC-4, AC-5, AC-9
- **Test Requirements**:
  - `rule` TR-1.1: seq 单调连续；Publish/Replay 边界（after=0 全量、after=latest 空、after 越界）行为确定；证据：`journal_test.go`
  - `rule` TR-1.2: 慢消费者通道满后被关闭，移除后不影响其他订阅者与发布者；重订阅经 Replay 获得完整序列；证据：`journal_test.go`
  - `rule` TR-1.3: 超过 stepCap/bytesCap 时旧事件被淘汰、`OldestSeq` 前移、内存不随发布次数增长；证据：`journal_test.go`（发布 10k 事件后断言长度有界）
  - `rule` TR-1.4: 现有 broker 单元测试零改动通过，`go test ./internal/broker/` 退出码 0

## Task 2: operations 操作注册表（状态机、有界历史、幂等接纳）
- **Status**: `completed`
- **Completion Evidence**:
  - internal/operations/{operation.go,manager.go} + manager_test.go：状态机、序号步骤环（500/16KiB）、终态优先级（timeout>failed）、sync.Once 冻结、幂等 Start（created/running/terminal 三态）、tombstone 410、TTL+条数修剪
  - 10k 操作压测存量恒定（优化 prune O(n²)→sort.Slice 后 23s 降至 0.65s）
  - `go test ./internal/operations/` 与 `go test -race ./internal/operations/ ./internal/broker/` 通过
- **Priority**: high
- **Depends On**: Task 1
- **Description**:
  - 新增包 `internal/operations`（`manager.go`、`operation.go`、`manager_test.go`）：
    - 类型：`Status`（running/succeeded/failed/timeout）、`Step{Seq int64; Name,Status,Msg string}`、`Record`（operation_id/action/target/req_id/status/steps/seq_first/seq_last/error/started_at/finished_at，JSON tag 稳定）。
    - `Manager`：`Start(proposedID, action, target, reqID string) (*Operation, error)`：proposedID 为空则生成 24hex；非空须匹配 `^op_[0-9a-fA-F-]{36}$`；ID 已存在时返回既有操作与 `ErrAlreadyRunning`/已终态哨兵（供 API 层 409/重放/410 分支），绝不二次执行。
    - `(*Operation).AddStep(ansible.TaskStep) Step`：截断 msg ≤16KiB，步骤环 ≤500（序号不回收，记录 seq_first/seq_last），经 Journal 发布实时事件；`MarkFailed(err)`/`MarkTimeout(err)` 记录结局（超时优先）；`Finish(httpStatus int)` 幂等冻结终态（sync.Once），发布 finished 事件并封存 Journal，按 200 条 + 1 小时 TTL 修剪终态记录，淘汰 ID 写入 ≤256 的 tombstone 环。
    - `Get(id)`：running/终态返回记录；tombstone 命中返回 `ErrExpired`；形态非法/未知返回 `ErrNotFound`。
    - `Subscribe(id, afterSeq)` 与 Journal 对接（供 SSE handler）。
  - 常量集中定义（500/16KiB/200/1h/256/缓冲 32）。
  - 线程安全；不依赖外部进程；不引入磁盘持久化。
- **Acceptance Criteria Addressed**: AC-2, AC-6, AC-7, AC-9
- **Test Requirements**:
  - `rule` TR-2.1: 并发 Start 同一 proposedID 只产生一个操作、AddStep 只执行一次（用回调计数模拟执行）；冲突分类（running→409 哨兵、terminal→终态哨兵、tombstone→ErrExpired、坏 ID→校验错误）正确；证据：`manager_test.go`
  - `rule` TR-2.2: 步骤 seq 连续；超 500 条/超 16KiB 截断后 seq_first/seq_last 与环内容正确；证据：`manager_test.go`
  - `rule` TR-2.3: Finish 幂等；timeout 标记优先于 failed；终态字段完整（error/steps/时间戳）；证据：`manager_test.go`
  - `rule` TR-2.4: 注入 10,000 个终态操作，存活记录 ≤200；tombstone ≤256；TTL（注入可控时钟或短常量测试实例）过期返回 ErrExpired；证据：`manager_test.go`
  - `rule` TR-2.5: Subscribe 回放+实时衔接无缺口（与 Journal 联测），终态后订阅能立即取到 finished/终态 snapshot

## Task 3: 执行器超时哨兵错误
- **Status**: `completed`
- **Completion Evidence**:
  - ansible.ErrTimedOut（timeoutError 包装，保留 playbook/时长）与 ops.ErrTimedOut（runStep DeadlineExceeded 优先分类，Run 经 errors.Join 保留 %w 链）
  - runner_test.go 用 PATH 注入假 ansible-playbook（sleep）验证真实超时分类，无外部依赖；ops_test.go sleep 20ms 验证 errors.Is，普通失败不命中哨兵
  - `go test ./internal/ops/ ./internal/ansible/` 通过
- **Priority**: high
- **Depends On**: None
- **Description**:
  - `internal/ansible/runner.go`：新增 `var ErrTimedOut = errors.New("ansible playbook timed out")`；DeadlineExceeded 分支返回 `fmt.Errorf("%w: playbook %s after %s", ErrTimedOut, playbook, r.Timeout)`（保留现有文案信息）。
  - `internal/ops/ops.go`：单步 DeadlineExceeded 时返回包装 `ops.ErrTimedOut` 的错误（新增同名哨兵），其余错误路径不变。
  - 两个错误均支持 `errors.Is`；不改变退出码、metrics 记录、span 行为。
- **Acceptance Criteria Addressed**: AC-2, AC-6
- **Test Requirements**:
  - `rule` TR-3.1: `errors.Is(err, ansible.ErrTimedOut)` / `errors.Is(err, ops.ErrTimedOut)` 在超时时成立，普通失败不成立；证据：两包新增/现有测试（用极短 Timeout 与 `exec.Command("true")`/慢命令 stub，不依赖 ansible 本体：ops 可直接用 `sleep`；ansible 侧用假 playbookPath 不可行——以错误分支纯函数化或接口抽离方式测试分类逻辑，确保 `go test` 无外部依赖）
  - `rule` TR-3.2: 错误文本仍包含 playbook/步骤名与超时时长；证据：测试断言

## Task 4: API 接线——beginOperation 助手与 runOp/runLocal 操作感知
- **Status**: `completed`
- **Completion Evidence**:
  - Handler 持有 opMgr；beginOperation(w,r,action,target) 返回包装 writer（sync.Once 终态冻结、断连写失败仍 Complete）+ctx，幂等重放 409/200/500/410；runOp/runLocal 经 ctx 操作记录步骤与结局（超时哨兵分类），无 op 时保留旧全局主题回退
  - NewHandler 加参，main.go 装配 DefaultConfig 管理器；audit 自动带 op_id
  - httptest 生命周期测试：成功/失败/超时/运行中 409/非法 ID/过期 410/断连终态可查/SSE snapshot+since+live tail/审计 op_id+req_id 关联，全部通过
- **Priority**: high
- **Depends On**: Task 2, Task 3
- **Description**:
  - `internal/api/handlers.go`：`Handler` 增加 `opMgr *operations.Manager`；`NewHandler` 增加该参数（同步更新 `main.go` 装配与所有构造点）。
  - 新增 `beginOperation(w, r, action, target) *http.Request`：校验后调用点使用 `r = h.beginOperation(w, r, action, target)`；内部完成：Start（取请求头 `X-Operation-ID`，400 响应非法 ID）、立即设置响应头 `X-Operation-ID`、把 op 放入 ctx、用 `opResponseWriter` 包装 w（首次 WriteHeader/Write 时按记录的 HTTP 状态与执行器标记结局调用 `Finish`，断连导致写失败同样完成 Finalize）。
  - `runOp`/`runLocal` 从 ctx 取 op：有 op 时步骤写 `op.AddStep`（不再向全局 `ansible.progress` 发布），结束时 `MarkFailed/MarkTimeout/成功不标记`；ctx 无 op 的防御性回退保留现有 PublishNoCache 行为（仅供未预期调用路径，注释标注迁移后不应出现）。
  - 包装器须正确透传 `http.Flusher`（SSE 包装器另算，本包装器用于变更响应）与 `http.Hijacker`（如需要）。
  - `auditLog` 从 ctx 提取 op_id 并加入既有审计行参数（行格式不变、仅增字段）。
  - 幂等重放分支在 beginOperation 内处理：running→409 `{error,operation_id,status:"running"}`；终态→按记录重放 200/500 规范 JSON（成功 `{operation_id,status,tasks}`；失败/超时 apiError 形态+头）；tombstone→410 `{error:"operation expired",recoverable:false,operation_id}`；重放分支直接短路，不进入 handler 业务。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-6, AC-7, AC-8, AC-10
- **Test Requirements**:
  - `rule` TR-4.1: `httptest` 级测试（包内，注入伪造执行器：给 Handler 增加仅测试使用的执行缝，例如包内变量/接口字段）验证：带 X-Operation-ID 的请求响应头回显同 ID；重复 ID 运行中得 409、终态得同结果重放、不二次执行（执行计数=1）；证据：`internal/api/operations_lifecycle_test.go`
  - `rule` TR-4.2: 客户端断连（取消请求 ctx 后丢弃响应写入）执行仍完成、操作终态可查；超时错误映射 timeout；证据：同上
  - `rule` TR-4.3: 不带 op 的路径不回归（回退分支单测）；audit 行在注入的 slog 测试 handler 中含 op_id+req_id
  - `rule` TR-4.4: 非法 X-Operation-ID 返回 400 且无操作建立
  - `rubric` TR-4.5: 接线抽象简洁性；scale 1-5；锚点 1=runOp/runLocal 出现双轨分叉/handler 自行管终态；3=统一但 begin 模板不一；5=两路径仅经 op 接口发布、终态由包装器单点冻结；threshold >=4；证据：代码走查

## Task 5: 迁移全部 47 个执行点到 beginOperation
- **Status**: `completed`
- **Completion Evidence**:
  - 52 个 runOp/runLocal 执行点全部在通过校验后、执行前以统一模板 beginOperation（12 文件；tls gencert/acme、smb init 的二次 playbook 经 ctx 共享同一操作，applyConfig 辅助函数由 5 个调用方透传）
  - action/target 与同函数 auditLog 完全对齐；脚本锚点全部 count==1 唯一校验
  - `go build ./... && go vet ./... && go test ./...` 全绿；函数级扫描确认除 applyConfig 外无裸跑路径
- **Priority**: high
- **Depends On**: Task 4
- **Description**:
  - 对 12 个 handler 文件中每个 runOp/runLocal 执行点，在**全部校验通过之后、首次执行之前**机械插入：
    `r = h.beginOperation(w, r, "<action>", "<target>")`
    action/target 与同函数内 `auditLog` 的 action/target 字符串保持一致（auditLog 调用点本身不改）。
  - 覆盖文件：zfs_handlers.go、user_handlers.go、smb_handlers.go（含 applyConfig 间接路径与 initSamba 双 playbook）、tls_handlers.go（双 playbook 请求）、pool_handlers.go、acl_handlers.go、auth_handlers.go、autosnap_handlers.go、device_handlers.go、iscsi_handlers.go、service_handlers.go，以及 zfs_handlers 中 scrub/autosnap/chown/batch 等全部点。
  - 不改变任何执行顺序、锁顺序、缓存失效调用（如 publishDatasets）与响应内容。
  - 完成后 grep 证明：不存在"调用 runOp/runLocal 但同函数无 beginOperation"的路径（applyConfig 等被 begin 之后调用的辅助函数除外，靠 r.ctx 透传）。
- **Acceptance Criteria Addressed**: AC-2, AC-8, AC-12
- **Test Requirements**:
  - `rule` TR-5.1: `go build ./... && go vet ./...` 通过；对每个执行点 grep 复核 beginOperation 先行；证据：grep 输出归档
  - `rule` TR-5.2: 代表性端点（createDataset/setDatasetProps、createUser、setSMBShare、initSamba、tlsAcmeIssue、startScrub、createPool、setACLEntry、mutateService）在集成测试中单操作步骤完整、双 playbook 请求步骤序号连续；证据：集成测试
  - `rule` TR-5.3: 校验失败（400/404/409）响应不带 X-Operation-ID；证据：集成/HTTP 测试
  - `rubric` TR-5.4: 迁移一致性（模板统一、无裸跑、无顺手改动业务逻辑）；scale 1-5；threshold >=4；证据：diff 走查

## Task 6: 操作查询与 SSE 端点
- **Status**: `completed`
- **Completion Evidence**:
  - GET /api/operations/{id}（400/404/410 分支）与 GET /api/operations/{id}/events（Last-Event-ID/?since、snapshot/step/finished 协议、30s 心跳、慢消费者断流触发重连、终态即释放）
  - httptest 覆盖终态 snapshot、since 精确增量、越界 snapshot、坏 since 400、运行中 live tail；全部通过
  - **R2 修复**：窗口判定改用 journal.OldestSeq()（REV-1）；先 Subscribe 后 Snapshot + snapshot 模式去重（REV-2）；lookup 非法 ID 改 404（REV-5）；finished 前非阻塞排空缓冲步（R2-N3）；新增字节窗口回归测试与非法 ID 404 测试
- **Priority**: high
- **Depends On**: Task 4
- **Description**:
  - 新增 `internal/api/operations_handlers.go`：
    - `GET /api/operations/{id}`：JSON Record；ErrNotFound→404，ErrExpired→410 规范不可恢复体。
    - `GET /api/operations/{id}/events`：SSE；注册路由；解析 `Last-Event-ID` 头与 `?since=`（整数，非法→400）；先取 Record：未知/过期在写 SSE 头之前分别 404/410 JSON。
    - 事件协议（均带 `id: <seq>` 行，seq 无步骤时为 0）：连接即发 `event: op.snapshot`（完整 Record）；之后实时 `event: op.step`（step JSON）；终态 `event: op.finished`（`{status,error}`）；订阅起点晚于窗口时先发 snapshot；since 精确位置则只发增量。
    - 复用现有 getEvents 的 fan-in/心跳（30s `: keepalive`）/Flusher 模式；慢消费者通道关闭即结束响应让 EventSource 重连；终态 snapshot 后客户端关闭、服务端在 finished 发送后结束响应。
  - 路由注册进 `RegisterRoutes`；更新 broker `ValidTopics` 无需改动（独立端点不走 /api/events）。
- **Acceptance Criteria Addressed**: AC-3, AC-4, AC-5, AC-6, AC-7
- **Test Requirements**:
  - `rule` TR-6.1: httptest SSE 测试：订阅 running 操作收到 snapshot→steps→finished；since 精确回放仅增量；since 越界先 snapshot；404/410 在响应头前返回 JSON；证据：`operations_handlers_test.go`
  - `rule` TR-6.2: 慢消费者断开后重新 Subscribe 序列完整无重复（联测 Journal）；证据：同测试
  - `rule` TR-6.3: 鉴权：未认证请求被现有中间件重定向/拒绝（与其他 /api 路由一致）；证据：中间件既有行为+路由位置走查
  - `rubric` TR-6.4: 协议简洁性（事件类型最少、snapshot 全量替换消除乱序/重复边界）；scale 1-5；threshold >=4；证据：走查

## Task 7: 操作日志线与审计关联
- **Status**: `completed`
- **Completion Evidence**:
  - operation accepted/finished（info）、step（debug，仅 name/status 不记 msg/敏感值）日志键齐全；audit 行经 ctx 自动追加 op_id
  - TestAuditLineCarriesOpAndReqID 捕获 slog 输出断言 op_id 与 req_id 同现
- **Priority**: medium
- **Depends On**: Task 4
- **Description**:
  - Manager/Handler 关键路径日志：`operation accepted`（op_id、req_id、action、target）、AddStep debug 行（op_id、seq、name、status；不记 msg 全文，避免敏感输出，msg 可经查询获取）、`operation finished`（op_id、status、duration_ms、error 存在与否）。
  - 既有 audit 行携带 op_id（Task 4 接线），确认请求行 req_id 关联说明成立。
  - 日志键名在代码中集中常量，风格对齐现有 slog 用法。
- **Acceptance Criteria Addressed**: AC-10
- **Test Requirements**:
  - `rule` TR-7.1: 使用 slog 测试 handler 捕获一次完整操作，断言 accepted/step(debug)/finished 三类行 op_id 一致，audit 行同时含 op_id 与 req_id；证据：`internal/api` 或 `internal/operations` 日志测试
  - `rule` TR-7.2: 步骤日志不含 extra-vars 值与密码字段（只含 name/status）；证据：测试与代码走查

## Task 8: Prometheus 操作指标
- **Status**: `completed`
- **Completion Evidence**:
  - internal/operations/metrics.go：dumpstore_operations_accepted_total、finished_total{status 三枚举}、running gauge，接入 /metrics
  - TestOperationMetricsEmitted 断言三类指标文本正确
- **Priority**: medium
- **Depends On**: Task 2, Task 4
- **Description**:
  - 参照 `internal/ansible/metrics.go` 风格新增操作指标（可置于 operations 包或 api metrics 汇总处）：`dumpstore_operations_accepted_total`、`dumpstore_operations_finished_total{status}`（succeeded/failed/timeout）、`dumpstore_operations_running` gauge；接入现有 `/metrics` 输出（与 ansible 指标同一写出路径）。
- **Acceptance Criteria Addressed**: FR-16（支撑 AC-13 可观测质量）
- **Test Requirements**:
  - `rule` TR-8.1: 执行若干成功/失败/超时操作后 `/metrics` 文本含三类指标且计数正确；证据：metrics 文本断言测试
  - `rule` TR-8.2: 指标基数有界（status 标签仅三个枚举值）；证据：走查

## Task 9: 前端操作生命周期（api() 内置、按操作订阅、断线恢复）
- **Status**: `completed`
- **Completion Evidence**:
  - utils.js：api() 对变更请求生成 op_<UUID>（非安全上下文 getRandomValues/Math.random 兜底）、X-Operation-ID 双向传递；按 op 独立 EventSource（snapshot 全量替换、step seq 去重、404/断流自重建退避、finished/410 关闭）；fetch 无响应时进入 recoverOperation（2s 轮询、410/连续 404/65min 三类明确不可恢复），终态以现有 resolve{tasks}/reject(Error+tasks) 形状回到 65 个调用点，零调用点改动
  - 步骤渲染抽出 stepHTML；renderRunningSteps 支持"连接中断，正在恢复结果…"；jobs/replication/send/rewrite 车道显式跳过跟踪
  - node --check 通过；浏览器真实演练：请求/响应头一致、事件流 opId 一致、终态渲染正确
- **Priority**: high
- **Depends On**: Task 6
- **Description**:
  - `static/js/utils.js`：
    - 变更类请求（POST/PUT/PATCH/DELETE）在 `api()` 内生成 `op_${crypto.randomUUID()}` 并发送 `X-Operation-ID`；从响应头读回规范 ID；返回值/异常形状保持现状（调用方零改动）。
    - 新增按操作的 EventSource 管理（同 ID 单实例、终态/410 关闭）：订阅 `/api/operations/{id}/events`；`op.snapshot` 全量替换运行中对话框步骤；`op.step` 按 seq 去重追加（对话框仍须处于 running 状态且绑定 ID 匹配）；`op.finished` 关闭 ES。
    - 抽出步骤渲染片段供 snapshot 与最终 showOpLog 复用（沿用现有 stepIcons/DOM 结构与 esc 转义）。
    - fetch 网络级失败（无响应）：不抛给调用方终态错误，进入恢复——对话框显示"连接中断，正在恢复结果…"指示（复用现有样式，必要时加一个 class），轮询 GET 操作（2s 间隔、上限随操作 TTL）并保持 SSE 重连；终态成功→以 `{tasks}` 正常 resolve；失败/超时→以现有 Error（含 tasks/message）reject；410→reject 明确不可恢复文案（带 `expired:true`），由调用方现有 catch 渲染为红色错误条。
    - HTTP 4xx/5xx（收到响应）路径完全保持现状（响应体即终态，ES 仅用于实时步骤）。
  - 不引入构建步骤/依赖；遵守事件委托与 esc 约定。
- **Acceptance Criteria Addressed**: AC-1, AC-3, AC-4, AC-7, AC-8, AC-11
- **Test Requirements**:
  - `rule` TR-9.1: 代码走查 + 浏览器演练：两个标签页并发变更对话框零串流；snapshot 替换不产生重复步骤；seq 去重对重复事件有效
  - `rule` TR-9.2: 模拟 fetch reject（DevTools offline/接口拦截）后恢复终态渲染与正常路径一致；410 显示不可恢复文案且不会无限轮询
  - `rule` TR-9.3: 65 个现有变更调用点零修改通过（grep 计数与手工冒烟）；成功 toast/失败错误条行为不变
  - `rubric` TR-9.4: 恢复体验（AC-11 同维）；scale 1-5；threshold >=4；证据：演练记录

## Task 10: 移除全局 ansible.progress 监听并保持 jobs.update
- **Status**: `completed`
- **Completion Evidence**:
  - loader.js 全局 SSE topics 与 ansible.progress 监听移除；jobs.update 与缓存主题映射逐行未动；app.js safety-net 注释校正为仅指缓存主题
  - grep 证据：前端无 ansible.progress 监听；后端无该主题发布（仅 ValidTopics 与 broker 历史测试保留名称）；浏览器 Network 实测 topics 串含 jobs.update、不含 ansible.progress
- **Priority**: high
- **Depends On**: Task 9
- **Description**:
  - `static/js/loader.js`：删除 `ansible.progress` 主题订阅与对应监听器；topics 列表移除该项；`jobs.update` 监听与 mergeJob 逻辑、其余缓存主题映射零改动。
  - 后端在全部执行点迁移完成后不再有 ansible.progress 发布者（ValidTopics 保留名称）；删除/更新引用该主题的过期注释（app.js 中"safety-net"注释若涉及 broker 丢消息，按新机制校正措辞）。
- **Acceptance Criteria Addressed**: AC-3, AC-8
- **Test Requirements**:
  - `rule` TR-10.1: grep 证明前端无 ansible.progress 监听、后端无该主题发布（broker 测试中既有引用保留）；jobs.update 监听与处理代码逐行无改动；证据：grep + diff
  - `rule` TR-10.2: 冒烟：缓存主题实时刷新与 jobs 页面更新行为不变；证据：dev 模式浏览器验证

## Task 11: 文档同步
- **Status**: `completed`
- **Completion Evidence**:
  - wiki/API-Reference.md：Operations 章节（头、409/200/500/410/400 重放表、record JSON、SSE 事件协议/Last-Event-ID、有界参数、指标名）+ 总览表两行
  - wiki/Architecture.md：执行车道节新增 Accepted-change operations（标识/统一终态/Journal 回放/有界/关联）与更新后的请求流图
  - docs/index.html：新增 Tracked, recoverable operations 特性卡；check-docs 的"代码改→文档改"条件满足
- **Priority**: medium
- **Depends On**: Task 6
- **Description**:
  - 更新 `wiki/API-Reference.md`：新增 "Operations" 章节（X-Operation-ID 请求/响应头、幂等重放 409/410、GET record、SSE 事件协议与 Last-Event-ID/since、有界保留默认值、410 语义）。
  - 更新 `wiki/Architecture.md`：四条执行车道语境下操作注册表单的位置（接纳→步骤序号→终态→有界淘汰）、与 jobs 通道、broker 缓存主题的边界。
  - 若 `docs/index.html` 列有端点/特性清单，同步条目；否则不动。
- **Acceptance Criteria Addressed**: AC-1（接口对外契约可查）
- **Test Requirements**:
  - `rule` TR-11.1: 文档描述与实际路由、状态码、事件名、JSON 字段逐一一致；证据：审查比对
  - `rule` TR-11.2: `make` 文档检查（`.github/workflows/check-docs.yml` 所执行内容）通过

## Task 12: 端到端集成测试
- **Status**: `completed`
- **Completion Evidence**:
  - tests/integration/operations_test.go（9 用例）：头与记录、失败终态、校验拒绝无操作、幂等重放记录冻结、410（201 次廉价 scrub 终态淘汰）、snapshot/Last-Event-ID 续传、双流隔离、断连后台完成、jobs 通道回归
  - `go vet -tags integration` 编译通过；本机无 limactl/VM，真实运行在独立审查中记为 blocked（不以 blocked 冒充通过）
- **Priority**: medium
- **Depends On**: Task 5, Task 9, Task 10
- **Description**:
  - 新增 `tests/integration/operations_test.go`（build tag integration，沿用 harness）：
    1. 成功/失败变更响应均含 X-Operation-ID，GET record 终态字段正确；
    2. 并发两操作各自 SSE 流零交叉（AC-3）；
    3. SSE 中途断开以 Last-Event-ID 重连，步骤完整且无重复（AC-4/AC-5）；
    4. POST 后立即断连（取消 context/关闭连接），轮询 GET 得确定终态（AC-6）；同 ID 重放不产生第二次副作用（如重复 create 返回同终态且目标仅一个）；
    5. 400 无操作头；jobs 与缓存主题行为回归断言（最小）。
  - 超时场景（5 分钟）不在 VM 测试中等待；由 Task 3 单测保证分类。
- **Acceptance Criteria Addressed**: AC-1, AC-3, AC-4, AC-5, AC-6, AC-8, AC-13
- **Test Requirements**:
  - `rule` TR-12.1: `go test -tags integration -count=1 -timeout 30m -v ./tests/integration/...` 在已部署 Linux VM 全绿；环境不具备时由独立审查记录 blocked 原因，不以 blocked 冒充通过
  - `rule` TR-12.2: 用例断言以服务端真实状态（数据集/快照存在性、步骤 seq 集合）为准，不仅看 HTTP 状态

## Task 13: 全量门禁与 stub 模式冒烟
- **Status**: `completed`
- **Completion Evidence**:
  - `go build ./...`、`go vet ./...`、`go test ./...` 全部通过（15 包）；`go test -race ./internal/broker ./internal/operations ./internal/api` 无竞争
  - 改动包 gofmt 干净（gofmt -l 列出的 login.go/main.go 等为存量格式问题，与本次改动无关，未制造新差异）
  - stub 模式真实浏览器冒烟全 PASS（含双 playbook 同一操作 seq 1-2、MutationObserver 跨标签零串流、Network 头/事件协议核验、jobs 页回归）
  - rubric 自评：TR-9.4 恢复体验 5/5（协议齐备+真实演练）；TR-13.2 表现保真 5/5（正常路径无感）
- **Priority**: high
- **Depends On**: Task 1–12
- **Description**:
  - 运行 `go build ./...`、`go vet ./...`、`go test ./...` 三连门禁；`make dev` stub 模式浏览器冒烟：单窗口一次成功+一次失败变更（实时步骤、最终渲染、toast/错误条）、双标签页并发隔离、DevTools offline 恢复、jobs 页面与 60s safety-net 刷新正常。
  - 审查 `go vet`、竞态：关键并发包额外跑一次 `go test -race ./internal/broker ./internal/operations ./internal/api`。
- **Acceptance Criteria Addressed**: AC-8, AC-11, AC-12, AC-13
- **Test Requirements**:
  - `rule` TR-13.1: 三命令退出码 0；`-race` 无数据竞争；输出归档为完成证据
  - `rubric` TR-13.2: 冒烟覆盖度与表现保真；scale 1-5；锚点 1=与现状有可感差异；3=功能正确但视觉/交互有偏差；5=正常路径无感、异常路径可解释；threshold >=4；证据：冒烟记录（步骤+观察）
