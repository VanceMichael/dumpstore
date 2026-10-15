# Independent Review — 操作标识与可恢复进度

- **Reviewer**: Independent review agent（全新上下文、只读）
- **Date**: 2026-10-15
- **Result**: R1 fail（1 major/4 minor/7 nit）→ 修复全部可处置项 → **R2 pass**（仅剩 2 条接受的既有架构附注 + 文档 nit 已修）
- **Blocked By**: 9 个端到端集成用例无法在本机运行（无 limactl/Lima VM）；`go vet -tags integration` 编译通过。AC-1/3/4/5/6/8 的端到端断言仅有单测+走查证据；未做真实断网浏览器演练（已有真实 happy-path 浏览器冒烟与恢复路径代码走查）。

## Checkpoints

### Rule（对应 AC）

| CP | 覆盖 | 初审 |
|----|------|------|
| CP-R1 | AC-1 标识与响应头 | PASS |
| CP-R2 | AC-2 两车道统一序号/唯一终态 | PASS |
| CP-R3 | AC-3 跨标签页隔离 | PASS |
| CP-R4 | AC-4 SSE 重连续传 | R1 FAIL（REV-1/2）→ **R2 PASS（修复 VERIFIED + 字节窗口回归测试）** |
| CP-R5 | AC-5 慢消费者同路径恢复（单测） | PASS |
| CP-R6 | AC-6 断连后台完成/幂等 | PASS |
| CP-R7 | AC-7 过期 410 | PASS |
| CP-R8 | AC-8 兼容性（响应体/jobs/缓存/调用点） | PASS |
| CP-R9 | AC-9 有界历史 | PASS |
| CP-R10 | AC-10 日志审计关联 | PASS（REV-4 补 error 文本） |
| CP-R13 | AC-13 build/vet/test/race | PASS；集成 BLOCKED（无 VM） |

### Rubric

| CP | 维度 | 初审 |
|----|------|------|
| CP-U1 | AC-11 网络中断恢复体验 | 4/5（REV-12 文案） |
| CP-U2 | AC-12 抽象统一/迁移质量 | 4/5（REV-3 action 占位符） |
| CP-U3 | 表现保真 | PASS |

## Findings

| ID | 级别 | 位置 | 问题 | 处置 |
|----|------|------|------|------|
| REV-1 | major | internal/api/operations_handlers.go | snapshot/增量分支用操作记录环 SeqFirst 判定，而增量来自独立 Journal（512KiB 字节上限）；字节淘汰早于记录环时 since=k 精确恢复静默丢步、不补 snapshot，违反 FR-11/AC-4 | 修复：Subscribe 前置并以 journal.OldestSeq() 判定；补单测 |
| REV-2 | minor | operations_handlers.go SSE | snapshot 先于 Subscribe，两调用间步骤在 snapshot 模式既不入快照也不回放 | 修复：先 Subscribe 后 Snapshot，snapshot 模式仅发送 seq>snapshotLast 的事件 |
| REV-3 | minor | device_handlers.go:106 | begin action 传了占位符字面量 "action"，与 audit 的 pool.device_offline/online 不一致 | 修复：改用参数 action |
| REV-4 | minor | internal/operations/operation.go | operation finished 日志缺失败 error 文本（FR-15） | 修复：失败时带 error 字段 |
| REV-5 | minor | operations_handlers.go | GET 形态非法 ID 返回 400，规格 FR-10 要求 404（POST 非法头才是 400） | 修复：lookup 非法 ID 映射 404；坏 since 仍 400 |
| REV-6 | nit | README.md、AGENTS.md、ops.go 包注释 | 仍描述向 ansible.progress 主题发布（已无发布者） | 修复：同步措辞 |
| REV-7 | nit | auth_handlers.go changePassword | begin target 传 ""，audit 为 username | 修复：执行前读 cfg.Username 传入 |
| REV-8 | nit | operation.go truncateMsg | maxBytes=2 假设下 cut=-1 可 panic（生产不可达） | 修复：cut<=0 返回 "…" |
| REV-9 | nit | zfs_handlers.go deleteSnapshotBatch | ReleaseHoldsFor 在 begin 之前 shell out（既有代码、best-effort 近似幂等） | 接受：不扩大本次改动，记录在案 |
| REV-10 | nit | 全局无 panic recovery | handler panic 会使在途操作停留 running（既有架构问题） | 接受：后续中间件层独立改进，不在本期范围 |
| REV-11 | nit | wiki/API-Reference.md Bounds | 未列慢消费者缓冲 32 | 修复：补文档 |
| REV-12 | nit | static/js/utils.js | 410 直接显示英文服务端文案 | 修复：前端固定中文不可恢复文案 |

## 安全审查

无发现：ID 严格正则；since 数字白名单；操作端点位于鉴权中间件之后；密码经 stdin 不经 argv/extra-vars，步骤日志不含 msg/变量值；410/404 无额外信息泄漏；指标 status 标签仅 3 枚举。

## 门禁证据（独立运行，-count=1）

- `go build ./...` exit 0
- `go vet ./...` exit 0
- `go test -count=1 ./...` 15 包全 ok
- `go test -race -count=1 ./internal/broker ./internal/operations ./internal/api` 无 DATA RACE
- `node --check` 三个前端文件 OK
- grep 对账：生产 52 个执行点 / 53 处 begin（差异由 applyConfig 透传 +3、双 playbook -2 解释）；65 个前端变更调用点零改动

---

## Review History

### R1 — Independent review agent — 2026-10-15

- **Result**: fail
- **Findings**: 1 major / 4 minor / 7 nit（REV-1…REV-12，见上表）
- **Evidence**: 见上"门禁证据"；锁顺序 m.mu→op.mu→journal.mu 无环；50 并发 Start 仅 1 created；10k 有界；断连 failingWriter 终态可查单测；浏览器真实冒烟全 PASS（实现阶段证据，非审查员执行）
- **Blocked By**: Lima VM 缺失，9 个集成用例未运行；未做真实断网浏览器演练

### R2 — Independent review agent R2 — 2026-10-15

- **Result**: **pass**
- **Scope**: 对 R1 修复的全新上下文只读复核 + 回归扫描。
- **Findings 复核**: REV-1/2/3/4/5/6/7/8/11/12 全部 VERIFIED；REV-9/10 接受理由成立（git diff 证明 ReleaseHoldsFor 为未触及的既有 best-effort 代码；全仓无 recover() 属既有架构，操作历史不跨重启）。
- **R2 新发现及处置**: R2-N1（wiki topics 表仍宣称 ansible.progress 在 playbook 期间推送）已修复（改表+退役说明）；R2-N2（CLAUDE.md 同款陈旧措辞）已修复；R2-N3（select(ch,done) 随机序可能先送 finished 漏尾部缓冲 step）已加固——done 分支先非阻塞排空 ch 再发 finished；R2-N4（changePassword begin/audit 两次加锁读用户名的理论 TOCTOU）接受：仅影响日志关联文本、属既有配置并发语义，不阻断。
- **Evidence**:
  - REV-1/2：Subscribe 先于 Snapshot、窗口判定改用 journal.OldestSeq()（operations_handlers.go）；八种时序组合走查无缺口/重复/泄漏；新增 TestOperationEventsJournalByteWindowTriggersSnapshot（40×16KiB 步真实构造 journal 窗口早于记录环，含 journalOldest>recordFirst 硬断言，双向断言 snapshot/incremental）。
  - REV-5：GET/events 非法 ID→404、坏 since→400、POST 坏头→400 三路单测实测。
  - REV-8：truncateMsg 对 maxBytes 0–8/16KiB × ASCII/CJK/emoji/混合输入独立实证 UTF-8 合法、不 panic。
  - REV-11：500/16KiB/200/1h/256/32 与代码常量逐一相符。
  - 门禁（R2 独立重跑）：`go build ./...`、`go vet ./...`、`go vet -tags integration ./tests/integration/...` exit 0；`go test -count=1 ./...` 15 包全 ok；`go test -race -count=1 ./internal/broker ./internal/operations ./internal/api` 无 DATA RACE（operations/broker 另 -count=3 重复干净）；node --check 三文件 OK；改动包 gofmt 干净。
- **Blocked By**: Lima VM 缺失，集成用例未运行（同 R1）。
