# local-backup — 本地增量备份服务（仅 API）

“备份任务显示成功，恢复时却缺了一段文件”——本服务用**快照前逐块校验**把这种
失败挡在提交之前：任何被引用但内容寻址块缺失/损坏的快照都不会标记为
`complete`，恢复也会被预检拒绝，并且能诊断到**具体缺失的块 id 和受影响文件**。

## 设计要点

- **内容定义分块**：`github.com/restic/chunker`，多项式在首次运行时随机生成
  并存入清单（`meta.chunker_poly`），保证后续快照边界稳定，小改动只产生少量新块。
- **内容寻址块存储**：独立目录 `blobs/<ab>/<64hex sha256>`，所有快照全局去重；
  写入走临时文件 + fsync + 原子 rename，崩溃不会留下半成品块文件。
- **SQLite 清单**：`manifest.db`（纯 Go 驱动 modernc.org/sqlite，无 cgo）：
  `snapshots / entries / chunks / snapshot_chunks / file_chunks`。
- **验证门**：快照行在扫描开始即以 `interrupted` 落库；所有文件处理完后，对
  本快照引用的**每个块**重新读盘哈希、比 id 与长度。全部通过才置 `complete`，
  否则置 `incomplete` 并在响应与 `reason` 中列出缺块。
- **写入中文件**：每个文件读前/读后比对 dev+inode、size、mode、mtime；不一致
  就**从头重读一次**；仍在变化则该条目标记为 `changed`（不写入任何块），快照
  因而不完整——绝不保存半截文件。
- **空文件**：正常文件、零块、sha256 为空输入摘要；恢复时重建 0 字节并核对长度。
- **权限**：目录与文件 mode（含 setuid/setgid/sticky）、uid/gid（best-effort，
  非 root 下 chown 失败不致命）、mtime 保留；目录元数据按深→浅顺序最后设置，
  避免先收权导致后续写入失败。
- **符号链接**：只备份/恢复链接本身（readlink / symlink，Linux 下链接时间用
  `utimensat(AT_SYMLINK_NOFOLLOW)`、属主用 `lchown`）。扫描用 WalkDir +
  `O_NOFOLLOW` 打开文件，**不沿链接读取，也不会越出允许根目录**。
- **恢复安全**：
  - 目标必须不存在或为空目录，否则整体拒绝；
  - 普通文件以 `O_CREATE|O_EXCL` 创建，绝不覆盖任何已有文件；
  - 恢复前预检全部块（损坏/缺失→返回 409/400，不创建目标）；
  - 清单中被篡改的 `..` 越界条目名会被跳过并报错。
- **每个文件单独事务**：块数据随条目即时落盘，即使提交前崩溃，已写入块也
  完整可用，留下的是一条可查询的 `interrupted` 快照，而不是“空的上传队列”。

## HTTP/JSON API（默认监听 127.0.0.1:8765）

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/backups` | `{"path":"/data"}` 执行一次快照；**即使有缺块也返回 200**，必须检查 `complete` |
| GET  | `/v1/snapshots` | 列出全部快照（含 interrupted/incomplete） |
| GET  | `/v1/snapshots/{id}` | 快照与条目详情 |
| GET  | `/v1/snapshots/{id}/diag` | **运维诊断**：缺块 id + 受影响条目列表 |
| POST | `/v1/restore` | `{"snapshot_id":1,"target":"/new/dir"}` 恢复到新目录 |
| GET  | `/v1/healthz` | 存活检查 |

构建与运行：

```bash
go build -o bin/local-backup ./cmd/local-backup
./bin/local-backup -repo ./repo -addr 127.0.0.1:8765
```

## 演示

```bash
./demo/run-demo.sh
```

六幕：基线快照（空文件/750 目录/符号链接，含指向根外的链接）→ 恢复到新目录
并核对摘要、长度、mode、链接 → 16MiB 文件中部改 4KiB，展示
`new_chunks=1, chunks_reused≈17` → 拒绝覆盖非空目标 → 注入“块丢失”
（`FAULT_INJECT=skip-chunk=2`），快照不完整、diag 给出具体缺块与文件、恢复被
预检拒绝 → 注入“提交中断”（`FAULT_INJECT=crash-commit`，进程 exit 99），
重启后看到 `interrupted` 行并可诊断。

`FAULT_INJECT` 仅用于演示与测试，生产不要设置。

## 测试

```bash
go test ./...
```

覆盖：往返恢复与元数据保留、增量块复用、缺块故障被验证门拦截并能定位、
崩溃后 interrupted 行存活、扫描中持续写入被标记 changed、非空目标拒绝、
符号链接不跟随、HTTP API 全链路。

## 仓库布局

```
cmd/local-backup/        main，HTTP 服务入口
internal/backup/
  types.go               领域类型与状态常量
  manifest.go            SQLite 清单（schema、事务、查询、缺块关联分析）
  blob.go                内容寻址块存储（原子写、重哈希校验）
  service.go             备份扫描/重读/分块/验证门/恢复预检与执行
  meta_linux.go          uid/gid、O_NOFOLLOW、lchown、链接级时间
  api.go                 HTTP/JSON 路由
demo/                    端到端演示脚本
```
