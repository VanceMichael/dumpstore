# Dataset Destroy 引用准入守卫（refguard）实施计划

## 背景与目标

存储管理员删除文件系统 / zvol / 递归子树时，必须在调用销毁 playbook **之前**获得 dumpstore 所管理引用的一致视图；发现引用则返回稳定的 409 冲突响应且绝不执行销毁；任何后端 / 配置读取失败一律失败关闭；检查与各消费者的创建、修改、移除共用同一准入锁，消除「检查通过后新增引用」的 TOCTOU；解除引用后同一删除请求可直接重试。

## Repository Research（现状结论）

- 删除入口：[deleteDataset](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/api/zfs_handlers.go#L366-L411)（`DELETE /api/datasets/{name...}`）。现有顺序：名称校验 400 → 池根保护 400 → `datasetExists` 404 → 直接跑 `zfs_dataset_destroy.yml`（`zfs destroy -f [-r]`）。**当前没有任何引用检查。**
- snapshot 专用入口 `DELETE /api/snapshots/{snapshot...}`（zfs_handlers.go:806）与批量删除（:849）独立，本次不动。
- 消费者及配置来源：
  1. **usershare**：playbook `smb_usershare_set.yml` 经 `net usershare add` 写入 dumpstore 独占目录 [UsershareDir](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/smb/config.go#L61-L66)（smb.conf 模板里配置 `usershare path` 指向它，故该目录内容即为权威集合）。UI 列表走 `net usershare info`（[system.ListSMBUsershares](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/system/system.go#L99-L127)），处理器为 [setSMBShare/deleteSMBShare](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/api/smb_handlers.go#L144-L213)，目前**无互斥锁**。
  2. **homes 基路径**：smb.conf `[homes] path`（常含 `/%U` 后缀），[SMBHomesConfig.Path](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/smb/config.go#L36-L43)；set/delete 处理器持 `h.smbMu` 做整文件 render-apply。
  3. **Time Machine 路径**：smb.conf 中带 `fruit:time machine` 的 section，[TimeMachineShare](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/smb/config.go#L45-L51)；create/delete 同样持 `h.smbMu`。
  4. **iSCSI zvol**：[iscsi.ListTargets](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/iscsi/iscsi.go#L40-L49) 直接读 Linux `/etc/rtslib-fb-target/saveconfig.json` 或 FreeBSD `/etc/ctl.conf`；无后端返回空切片（非错误），文件读 / 解析失败返回 error。Target 含 `IQN`（定位身份）与 `ZvolName`（如 `tank/vm1`）。处理器 [createISCSITarget/deleteISCSITarget](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/api/iscsi_handlers.go#L36-L162)，**无锁**。
  5. **replication source / 本机 target**：[replication.Task](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/replication/task.go#L37-L52)（`Source`、`Target`、`Remote`，`Remote==""` 即本机）。Store 为内存 map + tmp/rename 原子落盘（[store.go](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/replication/store.go)）。create/update/delete 处理器目前仅靠 Store 自身锁，**无跨处理器准入边界**；create 有注册失败回滚（删 task）。
- 路径归属所需数据：[zfs.Dataset](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/zfs/zfs.go#L28-L44) 一次 `zfs list` 同时给出 `Name / Mountpoint / Type`（volume 的 mountpoint 为 `-`；legacy/none 无真实挂载点）。
- 现有锁：Handler 上 `userMu`、`smbMu`（[handlers.go:199-200](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/api/handlers.go#L199-L206)）；错误响应统一为 `{error, tasks?}`（[apiError](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/api/handlers.go#L182-L188)）。
- 前端删除流程：[dataset-dialogs.js:55-72](file:///Users/vance/project/mineProject/swe/09101010/project-02/static/js/dataset-dialogs.js#L55-L72) 先关对话框再发请求，失败只走 showOpLog；`api()` 抛错仅带 `error/tasks`（[utils.js:11-24](file:///Users/vance/project/mineProject/swe/09101010/project-02/static/js/utils.js#L11-L24)）。对话框 HTML 见 [index.html:362-384](file:///Users/vance/project/mineProject/swe/09101010/project-02/static/index.html#L362-L384)。
- 集成环境（`dev/lima-linux.yaml`）只有 ZFS+Ansible，无 samba/targetcli；本机 replication 任务无需额外服务，是集成测试可依赖的消费者。

## 设计决策

1. **新包 `internal/refs`（纯逻辑 + 准入锁）**：不 import 任何业务包，数据结构自定义，api 层做适配。匹配逻辑可脱离 CLI/Ansible 做完整单测。
2. **子树集合**：`recursive=false` 时仅 `{target}`；`true` 时为 target + 从同一份 `zfs list` 快照筛出的全部后代（组件边界前缀：`d==target || strings.HasPrefix(d, target+"/")`，含 volume 类型）。
3. **mountpoint 归属（非字符串巧合）**：仅取真实绝对挂载点的 filesystem（排除 `""`、`-`、`none`、`legacy`），对配置路径 P 选「最长匹配挂载点」的数据集（`P==mp || strings.HasPrefix(P, mp+"/")`）。因此 `/tank/data2` 的 share 不会误算到 `tank/data`；路径归属到更近的子数据集时，非递归删父数据集不误报、递归删才报。
4. **homes 基路径**：剥离 `/%U`、`/%u` 后缀及尾斜杠后做归属；路径中段出现替换符时最长前缀仍倾向阻塞（保守正确）。
5. **iSCSI**：`ZvolName` 命中子树（等于或为其后代）即冲突，身份用 IQN，detail 带 zvol。
6. **replication**：source 命中子树 → `replication_source`；仅 `Remote==""` 且 target 命中 → `replication_local_target`；远端任务 target 在别处、不阻塞。Enabled=false 的任务同样阻塞（配置仍悬空）。任务 source 是被删数据集**祖先**时不阻塞（递归 send 少一个子数据集，任务仍有效）。
7. **失败关闭**：采集阶段任一读取失败 → 500，不跑销毁 playbook。SMB 未初始化（smb.conf 不存在）→ 不存在 dumpstore 管理的 SMB 引用，跳过整组 SMB 读取；已初始化则 smb.conf 解析与 usershare 目录读取都必须成功。iSCSI 无后端 = 空集合（沿用现状）；saveconfig/ctl.conf 存在但读 / 解析失败 = 错误。
8. **准入边界**：Handler 新增 `guard *refs.Guard`（内嵌 `sync.Mutex`）。`deleteDataset` 在锁内完成「采集 → 匹配 → 销毁 playbook」全过程；下列消费者变更在**提交段**（playbook / store 写入期间）持同一把锁：
   - usershare set/unset；homes set/delete；timemachine create/delete；smb init（顺带纳入，保证读快照一致）
   - iscsi target create/delete
   - replication create/update/delete（含注册失败回滚段）
   - 另将 dataset **rename** 与 **setDatasetProps**（可改 mountpoint，影响归属）纳入同一互斥段，仅加锁、不加新校验，堵住检查快照与实际归属不一致的窗口。
   - 锁序固定：guard 为最外层，smbMu 在内层（不存在反向获取）。
9. **usershare 权威读取改为直接读独占目录**：在 `internal/smb` 新增 `ListUsershares(goos)`（文件名即 share 名，解析文件内 `path=` 行）；目录不存在 → 空集合；读取 / 解析失败 → error。比 shell `net usershare info`（无 share 时非零退出、无法区分错误）更确定，且该目录由 dumpstore 独占。`system.ListSMBUsershares` 保留给现有 GET 列表接口，不动。
10. **冲突响应（稳定契约）**：HTTP 409：
    ```json
    {
      "error": "dataset \"tank/data\" is in use by 2 dumpstore-managed consumer(s)",
      "code": "dataset_in_use",
      "conflicts": [
        {"type": "smb_usershare", "id": "docs", "name": "docs", "detail": "path /tank/data/docs"},
        {"type": "replication_source", "id": "a1b2c3d4", "name": "nightly", "detail": "source tank/data"}
      ]
    }
    ```
    固定 type 词汇表：`smb_usershare` / `smb_homes` / `smb_timemachine` / `iscsi_target` / `replication_source` / `replication_local_target`；`id` 为可直接定位 / 删除的身份（share 名、`homes` 固定串、TM share 名、IQN、replication task id）；输出按 `(type,id)` 排序保证稳定。
11. **兼容性保持**：无引用时响应体与 tasks 形状不变；池根 400、非法名 400、不存在 404、`?recursive=true`、playbook 失败的 500+tasks 全部维持；snapshot 两个入口完全不引入检查。
12. **重试**：检查无状态，DELETE 重试即重跑全流程；前端在 409 时保持对话框开启并内联展示消费者清单，解除后可直接再次点击同一删除按钮。
13. **非目标（明确不做）**：不给 snapshot 删除加检查；不处理自动快照计划（autosnap）引用——需求枚举的六类消费者之外；不拦截正在运行的 replication job（准入边界只管配置的增改删；运行中 send/recv 与 destroy 的竞争由 ZFS busy 错误与既有 `-f` 语义兜底）；NFS sharenfs / ACL 随数据集本体销毁，无悬空配置。

## Files and Modules

- `internal/refs/guard.go`（新建）：`Dataset / Usershare / TMShare / SMBState / ISCSITarget / ReplTask / Snapshot / Consumer` 类型；`Guard{ mu; }`（`Lock/Unlock`）；`Snapshot.DestroyConflicts(target string, recursive bool) []Consumer`；子树筛选、最长挂载点归属、固定顺序枚举与排序；homes 基路径规范化辅助函数。
- `internal/refs/guard_test.go`（新建）：表驱动单测（见 Validation）。
- `internal/smb/config.go`：新增 `ListUsershares(goos) ([]UsershareFile, error)`（目录直读 + `path=` 解析）及小测试 `config` 侧的解析函数（可单测纯解析部分）。
- `internal/api/handlers.go`：`Handler` 增加 `guard *refs.Guard`；`NewHandler` 内构造（无需改签名，`h.repl` 已在参数中）；`apiError` 增加 `Code string` 与 `Conflicts` 字段（omitempty，旧响应不受影响）；新增 `writeConflictError`。
- `internal/api/zfs_handlers.go`：重写 `deleteDataset` 尾部（校验顺序不变，存在性确认后进入 guard：采集 → 409 或销毁）；`renameDataset`、`setDatasetProps` 的写入段加 guard。
- 新增 `internal/api/refs_collect.go`（新建）：`gatherRefSnapshot() (refs.Snapshot, error)` 适配 zfs/smb/iscsi/replication 读取，集中失败关闭语义。
- `internal/api/smb_handlers.go`：六个写处理器 + init 的变更段包 guard（guard→smbMu 顺序）。
- `internal/api/iscsi_handlers.go`：create/delete runOp 段包 guard。
- `internal/api/replication_handlers.go`：create/update/delete 的 store + scheduler 段包 guard（含回滚路径）。
- `static/index.html`：删除对话框新增 `#deleteDatasetConflicts` 冲突清单容器（默认隐藏）。
- `static/js/utils.js`：`api()` 抛错附带 `code`、`conflicts`。
- `static/js/dataset-dialogs.js`：删除按钮流程改为成功后才关对话框；409 时在对话框内渲染类型标签 + 身份 + detail 并允许直接重试；其余错误维持 showOpLog。
- `wiki/API-Reference.md`：补 DELETE /api/datasets 的 409 `dataset_in_use` 响应契约。
- `tests/integration/api_test.go`：新增 `TestDatasetDestroyConflicts`（仅依赖 ZFS：本机 replication source/local-target 阻塞 → 删任务后重试成功；递归子树场景；无引用数据集与池根行为回归）。

## Implementation Steps（依赖顺序）

1. 新建 `internal/refs` 纯逻辑包（快照结构、子树 / 归属算法、冲突枚举、Guard 互斥），同步写表驱动单测并跑通。
2. `internal/smb` 增加 usershare 目录直读函数与纯解析测试。
3. api 层：Handler 挂 guard；新增 `gatherRefSnapshot` 采集器（失败关闭）。
4. 改造 `deleteDataset`：guard 内采集→409 稳定响应→无冲突才调销毁 playbook；补 `writeConflictError` 与 apiError 字段。
5. 将 guard 接入全部消费者写处理器及 rename / setDatasetProps；检查锁序（guard 外、smbMu 内）。
6. 前端：api() 传 code/conflicts；删除对话框冲突面板与不关窗重试。
7. 更新 wiki API-Reference。
8. 新增集成测试（integration tag，VM 环境）。
9. 全量 `go build ./...`、`go vet ./...`、`go test ./...`；`make dev` 手工冒烟（无引用路径与 replication 冲突路径）。

## Validation

- `go test ./...`（无外部依赖）必须通过；refs 单测至少覆盖：
  - 无任何引用 → 无冲突；非递归仅查目标自身；递归覆盖全部后代（含 zvol 后代）。
  - usershare：精确命中、子目录命中、`/tank/data2` vs `tank/data` 字符串巧合不误报、嵌套数据集归属到更近 mountpoint。
  - homes：`/%U` 基路径剥离后命中；未启用 homes 不报。
  - TM：路径命中与多 share 排序。
  - iSCSI：zvol 等于 / 属于子树 → 报 IQN；非递归删父、zvol 为独立子数据集时不报。
  - replication：source 命中；本机 target 命中；Remote 非空 target 不阻塞；disabled 任务仍阻塞；source 为祖先不阻塞。
  - 输出按 `(type,id)` 稳定排序；消费 identity/detail 内容断言。
- 集成测试（`go test -tags integration ...`，Lima VM）：本机 replication source 与 local target 分别触发 409，删除任务后同一 DELETE 重试 200；池根与无引用数据集行为不变。
- 手工：`make dev` 起桩环境，造本机 replication 任务后删除数据集得到 409 清单，删任务后重试成功；前端对话框展示 / 重试流程正常。
- 兼容性核对：snapshot 删除入口、`?recursive` 参数、playbook 失败 500+tasks、旧错误响应字段均不变。

## Risks

- **锁持有覆盖 playbook 执行（秒级）**：消费者写操作与销毁互相串行，功能正确但并发吞吐下降；这些操作本身低频且均为管理员显式变更，可接受。
- **运行中 replication job 与 destroy 竞争**：不纳入配置锁（属执行面而非配置面）；本机 recv 重建目标目录属极小窗口，依赖 ZFS busy/已存在错误暴露，保持既有 ZFS 错误兼容；在代码注释与本计划中显式标注。
- **usershare 文件格式依赖 Samba 实现**：该目录由 dumpstore 独占、由 `net usershare add` 写入，文件名与 `path=` 行格式长期稳定；解析对未知行容错，任何异常按失败关闭处理。
- **homes 非标准替换符路径**：采用最长挂载点前缀的保守匹配，宁可阻塞也不漏报；管理员移除引用后即可重试。
- **smb.conf / saveconfig 在已初始化状态下被外部损坏**：解析失败 → 500 失败关闭（不销毁），符合需求，管理员需先修复配置。
