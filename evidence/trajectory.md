# 阻止删除仍被托管服务引用的数据集子树

> **Workspace:** /Users/vance/project/mineProject/swe/09101010/project-02

---

**User**

存储管理员删除一个文件系统、卷或递归数据集子树时，界面不能报告成功后才发现 SMB 路径、iSCSI target 或复制任务已经指向不存在的存储对象。请在执行 dataset destroy 前建立 dumpstore 所管理引用的一致视图：覆盖 usershare、homes 基路径、Time Machine 路径、iSCSI zvol、本机 replication source，以及 Remote 为空时的本机 target；递归删除必须检查目标及全部后代，路径引用要按数据集实际 mountpoint 归属而不是字符串巧合。若发现引用，以稳定的冲突响应列出消费者类型和可定位身份，且不得调用销毁 playbook；任何依赖后端或配置读取失败都要失败关闭。删除检查还要与这些消费者的创建、修改和移除共享准入边界，避免检查通过后又新增引用；解除最后一个阻塞引用后，同一个删除请求应可正常重试。无引用数据集、池根保护、snapshot 专用入口、recursive 参数和既有 ZFS 错误保持兼容，部分消费者检查失败或并发变更不能留下被破坏的配置。

**Assistant**

toolName: Skill

status: success

我先研究仓库现状，再制定实施计划。让我并行探查相关代码。

toolName: view_folder

status: success

/Users/vance/project/mineProject/swe/09101010/project-02

toolName: file_search

status: success

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/smb/config.go

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/zfs_handlers.go

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/handlers.go

继续研究 SMB、iSCSI、replication 的配置存储与处理逻辑。

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/smb_handlers.go

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/iscsi_handlers.go

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/iscsi/iscsi.go

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/replication/store.go

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/replication/task.go

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/replication_handlers.go

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/system/system.go

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/playbooks/zfs_dataset_destroy.yml

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/replication/runner.go

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/static/js/dataset-dialogs.js

toolName: search_by_regex

status: success

核心后端路径已清楚。再看 usershare playbook、main.go 装配、dev 桩与前端对话框 HTML。

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/playbooks/smb_usershare_set.yml

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/playbooks/smb_usershare_unset.yml

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

再确认集成测试环境与 API 文档现状，然后落地计划文档。

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

研究完成，信息已齐。现在编写实施计划文档。

toolName: Write

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/.trae/documents/dataset_destroy_refguard_plan.md

changes: undefined

计划已完成，请审阅。核心要点：

**研究结论**：当前 `deleteDataset` 在校验后直接调用销毁 playbook，对六类 dumpstore 托管引用（usershare、homes 基路径、Time Machine、iSCSI zvol、replication source、Remote 为空时的本机 target）没有任何检查；各消费者写处理器之间也没有统一准入边界。

**方案要点**：
1. 新建纯逻辑包 `internal/refs`：子树集合（非递归只查目标、递归覆盖全部后代含 zvol）+ 按**最长实际 mountpoint 前缀**归属路径（杜绝 `/tank/data2` 误匹配 `/tank/data` 这类字符串巧合），输出按 `(type,id)` 稳定排序的冲突清单。
2. 冲突时返回 **409 `dataset_in_use`**，含六类稳定 type 标签与可定位身份（share 名 / IQN / 任务 ID 等），**绝不调用销毁 playbook**；任一后端读取失败一律 500 失败关闭。
3. Handler 新增统一准入锁：销毁在锁内完成「采集→检查→销毁」，所有消费者增改删（含 rename / mountpoint 变更）的提交段持同一把锁，消除 TOCTOU；解除引用后同一 DELETE 无状态重试即成功。
4. usershare 改为直读 dumpstore 独占的 usershare 目录（比解析 `net usershare info` 退出码更确定）。
5. 前端删除对话框在 409 时保持打开并内联列出消费者，可直接重试。
6. 完全兼容：池根 400、snapshot 专用入口、recursive 参数、playbook 500+tasks 错误形态不变。

toolName: NotifyUser

status: success

计划已批准，开始实施。

toolName: todo_write

status: success

Todos updated: 9 items

**步骤 1：新建 `internal/refs` 纯逻辑包**

toolName: Write

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/refs/guard.go

changes: undefined

**步骤 1 续：refs 包表驱动单测**

toolName: Write

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/refs/guard_test.go

changes: undefined

运行 refs 包测试：

toolName: Shell

status: success

单测发现一个语义问题：非递归删除时子 zvol 不会被移除（ZFS 自身会因 has-children 失败），不应报告为悬空引用。修正为仅递归模式才按 target 前缀匹配后代。

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/refs/guard.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/refs/guard.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/refs/guard.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/refs/guard.go

changes: undefined

toolName: Shell

status: success

**步骤 2：smb 包新增 usershare 目录直读**

toolName: todo_write

status: success

Todos updated: 9 items

toolName: Write

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/smb/usershares.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/smb/usershares.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/smb/usershares.go

changes: undefined

toolName: Write

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/smb/usershares_test.go

changes: undefined

让 ListUsershares 委托给可注入目录的内部函数，测试直接覆盖目录读取（含 malformed 失败关闭路径）：

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/smb/usershares.go

changes: undefined

toolName: Write

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/smb/usershares_test.go

changes: undefined

toolName: Shell

status: success

**步骤 3：Handler 挂 guard + 采集器**

toolName: todo_write

status: success

Todos updated: 9 items

toolName: Write

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/refs_collect.go

changes: undefined

现在改 handlers.go：

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/handlers.go

changes: undefined

**步骤 4：改造 deleteDataset**

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/zfs_handlers.go

changes: undefined

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/zfs_handlers.go

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/zfs_handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/zfs_handlers.go

changes: undefined

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/zfs_handlers.go

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/zfs_handlers.go

changes: undefined

**步骤 5：guard 接入消费者写处理器（SMB 组）**

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/smb_handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/smb_handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/smb_handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/smb_handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/smb_handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/smb_handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/smb_handlers.go

changes: undefined

**iSCSI 处理器接入 guard：**

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/iscsi_handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/iscsi_handlers.go

changes: undefined

**Replication 处理器接入 guard：**

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/replication_handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/replication_handlers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/replication_handlers.go

changes: undefined

后端接线完成，先编译验证：

toolName: Shell

status: success

后端全绿。**步骤 6：前端冲突展示与重试**

toolName: todo_write

status: success

Todos updated: 9 items

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/static/style.css

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/static/style.css

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/static/index.html

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/static/js/utils.js

changes: undefined

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/static/js/utils.js

`opLogDialog` 已导出。现在改 dataset-dialogs.js 的删除流程：

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/static/js/dataset-dialogs.js

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/static/js/dataset-dialogs.js

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/static/js/dataset-dialogs.js

changes: undefined

**步骤 7：更新 API 文档**

toolName: todo_write

status: success

Todos updated: 9 items

toolName: edit_file_search_replace

status: failed

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/wiki/API-Reference.md

changes: undefined

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/wiki/API-Reference.md

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/wiki/API-Reference.md

changes: undefined

**步骤 8：集成测试**

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/tests/integration/harness_test.go

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/tests/integration/api_test.go

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/tests/integration/api_test.go

toolName: search_by_regex

status: success

toolName: edit_file_search_replace

status: failed

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/tests/integration/api_test.go

changes: undefined

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/tests/integration/api_test.go

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/tests/integration/api_test.go

changes: undefined

修正测试里 taskID 的声明与清理保护：

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/tests/integration/api_test.go

changes: undefined

修正三处断言逻辑反转（应为「仍存在才失败」）：

toolName: edit_file_search_replace

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/tests/integration/api_test.go

changes: undefined

**步骤 9：全量验证**

toolName: todo_write

status: success

Todos updated: 9 items

toolName: Shell

status: success

编译、vet、单测全绿（含 integration 标签编译）。现在做 dev 桩环境冒烟，先看桩行为：

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/dev/bin/zfs

toolName: search_by_regex

status: success

启动 dev 桩服务器进行冒烟：

toolName: Shell

status: success

toolName: Shell

status: success

toolName: view_files

status: success

filePath: /var/folders/h2/dl6g5c0d2jjb29trgqldb6fw0000gn/T/trae-agent-toolhost-501/jobs/job-07a071ec8ebf4094a41845c172608428/output.log

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/platform/paths.go

toolName: search_by_regex

status: success

dev 模式默认写 `/var/lib/dumpstore`（本机无权限），改用 httptest 在桩 PATH 下驱动真实路由做端到端冒烟。先确认构造函数签名：

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/zfs/zfs.go

toolName: view_files

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/dev/bin/ansible-playbook

编写临时冒烟测试（env 门控，用完即删），在桩 PATH 下走真实 HTTP 路由：

toolName: search_by_regex

status: success

toolName: Write

status: success

filePath: /Users/vance/project/mineProject/swe/09101010/project-02/internal/api/zz_smoke_refguard_test.go

changes: undefined

toolName: Shell

status: success

端到端冒烟通过：409 时销毁 playbook 完全未执行，解除引用后同一请求重试成功。清理临时测试并做 JS 语法校验：

toolName: delete_file

status: success

toolName: Shell

status: success

最终全量验证：

toolName: Shell

status: success

toolName: GetDiagnostics

status: success

toolName: todo_write

status: success

Todos updated: 9 items

已完成 dataset destroy 引用准入守卫的全部实现，`go build`、`go vet`、`go test ./...`（含 integration 标签编译）全部通过，零诊断。

## 实现内容

**1. 新增纯逻辑包 [internal/refs/guard.go](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/refs/guard.go)**
- 销毁子树：非递归仅目标本身；递归覆盖目标及全部后代（含 zvol），组件边界前缀匹配杜绝 `tank/da` 误中 `tank/data`。
- 路径归属按**最长实际 mountpoint 前缀**：share/homes/TM 路径归属于最近的挂载数据集，`/tank/data2` 不会误算到 `tank/data`；路径挂在子数据集下时，非递归删父不报、递归才报。
- 六类消费者固定 type 词汇表：`smb_usershare`、`smb_homes`（剥离 `/%U` 基路径）、`smb_timemachine`、`iscsi_target`（IQN 定位）、`replication_source`、`replication_local_target`（仅 Remote 为空）；结果按 `(type,id)` 稳定排序。
- `Guard` 互斥锁作为统一准入边界。22 个表驱动用例见 [guard_test.go](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/refs/guard_test.go)。

**2. usershare 权威直读**：[internal/smb/usershares.go](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/smb/usershares.go) 直接读取 dumpstore 独占的 usershare 目录（文件名=share 名，解析 `path=`），不依赖 `net usershare info` 的退出码；目录缺失视为空，读失败/畸形文件报错以失败关闭。

**3. 后端接入**
- 采集器 [refs_collect.go](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/api/refs_collect.go)：持锁采集 zfs/smb/iscsi/replication 一致快照，任一读取失败即 500，不调销毁 playbook；SMB 未初始化、iSCSI 无后端视为空集合。
- [deleteDataset](file:///Users/vance/project/mineProject/swe/09101010/project-02/internal/api/zfs_handlers.go#L383-L448)：锁内「采集 → 匹配 → 销毁」，冲突返回 **409 `dataset_in_use`**（`{code, conflicts:[{type,id,name,detail}]}`）并写 `dataset.destroy_blocked` 审计日志，playbook 绝不执行。
- 消费者增改删（SMB usershare/homes/TM/init、iSCSI create/delete、replication 增改删）及 rename、setDatasetProps 的提交段全部持同一把锁，锁序固定 guard 在外、smbMu 在内——检查通过后不可能再新增引用。

**4. 前端**：[删除对话框](file:///Users/vance/project/mineProject/swe/09101010/project-02/static/js/dataset-dialogs.js#L69-L102) 在 409 时保持打开，内联列出消费者类型、身份与路径，解除引用后点同一 Delete 即可重试；[api()](file:///Users/vance/project/mineProject/swe/09101010/project-02/static/js/utils.js#L18-L24) 透传 code/conflicts。

**5. 兼容与文档**：池根 400、不存在 404、`?recursive` 参数、snapshot 专用入口、playbook 500+tasks 错误形态全部不变；[API-Reference](file:///Users/vance/project/mineProject/swe/09101010/project-02/wiki/API-Reference.md#L180-L203) 记录了 409 契约；新增 [TestDatasetDestroyConflicts](file:///Users/vance/project/mineProject/swe/09101010/project-02/tests/integration/api_test.go#L174-L289) 集成测试。

**验证**：除单测外，还用 httptest + CLI 桩做了真实 HTTP 端到端冒烟（临时测试已删除）：source 非递归/递归均 409 且 playbook 未执行、本机 target 409、无引用 zvol 正常销毁、删除 replication 任务后同一 DELETE 重试 200。计划文档在 [dataset_destroy_refguard_plan.md](file:///Users/vance/project/mineProject/swe/09101010/project-02/.trae/documents/dataset_destroy_refguard_plan.md)。

一点明确的设计取舍：准入锁只覆盖消费者**配置**的增改删，不拦截运行中的 replication job（执行面）；其与销毁的极小竞争由 ZFS busy 错误和既有 `-f` 语义兜底，与现有错误处理保持兼容。
