# Proposal: 将 drive9 FUSE 的 server metadata 提升为系统权威

**Date**: 2026-04-21  
**Purpose**: 基于当前 `drive9` FUSE、server、backend 和 datastore 代码现实，提出一套“server metadata 为系统权威”的架构修复方案，用于消除新建文件在 `Create -> first flush/commit` 窗口内的名字空间可见性空洞，并为后续多挂载、多进程与恢复语义建立统一基础。

## Summary

当前 `drive9` 的 FUSE 写路径已经有本地 `PendingIndex`、`shadowStore`、`writeBack` 和 `commitQueue` 等机制，但系统没有一个从 `Create` 成功开始就持续生效的单一名字空间权威来源。结果是：

- FUSE `Create()` 成功后，本地 handle 与 dirty buffer 已存在；
- 但 `PendingIndex` 通常要到后续 stage/flush 时才补入；
- backend DB 的 `file_nodes/files` 记录也通常要到真正写入或 finalize 上传时才出现；
- 当新的 `Lookup/GetAttr/Readdir/Open` 在这个窗口到来时，客户端会退回到 server `HEAD/LIST`，而 server metadata 尚未包含该路径，最终可能暴露为 `ENOENT` 或 `EIO`。

本 proposal 的核心决策是：

1. 将 `server metadata` 明确定义为整个系统的名字空间权威；
2. 将 `Create/Mkdir/Rename/Unlink` 等名字空间变更改为先落 server metadata，再返回成功；
3. 将本地 `PendingIndex`、`shadowStore`、`writeBack` 从“名字空间正确性核心”降级为“当前 mount 的未提交内容 overlay 与性能优化层”；
4. 将 brand-new create 与 overwrite/update 的语义显式拆开，使“路径是否存在”与“内容是否已确认提交”成为两个独立但可组合的状态面。

当前已经收敛的产品与架构语义是：

- `create` 在 metadata-create 成功后即创建一个正常的 0 字节文件，而不是显式 `PENDING_CREATE` 对象；
- 在 authoritative metadata view 中，该文件从 `create` 成功返回时即已存在；
- 其他 mount 的实际观察仍受各自 dentry/negative/readdir cache 边界约束；一旦越过这些边界重新观察，`stat` 应看到 `size = 0`，`read` 应返回空文件；
- `create` 本身即分配 `revision = 1`，第一次成功写入内容后推进为 `revision = 2`。
- FUSE 客户端强制走 `metadata-only create` 作为 create 起点；
- 旧客户端在过渡期继续兼容，但服务端内部必须逐步收敛到统一的 create state machine，而不是长期保留双语义 create。
- `readdir` 不使用本地 overlay 改写目录项集合；已打开目录句柄采用 per-handle snapshot 语义，因此后续目录变化后同一目录句柄仍可能继续看到旧结果。

该方案不要求移除 `PendingIndex` 或 `shadowStore`，也不要求改变现有 revision-gated overwrite、multipart upload、patch/append 的基本数据面能力；它改变的是名字空间 ownership 与可见性时机。

## Context

### Current State

当前代码已经同时存在两套 metadata 现实，但没有一套从 `Create` 开始就是权威。

#### 1. FUSE 本地已经维护了临时 metadata overlay

`pkg/fuse/dat9fs.go` 中的 `Dat9FS` 当前维护：

- `pendingIndex`：注释直接说明它是 in-memory authoritative metadata index；
- `shadowStore`：按路径维护本地 shadow 文件；
- `writeBack` / `uploader`：本地 write-back cache；
- `dirtyInodes` / open handles：用于未 flush 的 size 与 content 视图。

相关代码位置：

- `pkg/fuse/dat9fs.go` 中 `Dat9FS` 字段定义；
- `Lookup(...)` 先查 `pendingIndex` / `writeBack`，再查远端 `StatCtx/ListCtx`：`pkg/fuse/dat9fs.go`；
- `Create(...)` 仅创建本地 inode、handle、dirty buffer、streamer，没有立即把该文件写入 `pendingIndex`：`pkg/fuse/dat9fs.go`；
- `stageShadowLocked(...)` 与 flush fast-path 才会 `pendingIndex.PutWithBaseRev(...)`：`pkg/fuse/dat9fs.go`。

这意味着本地 overlay 已经存在，但它不是从 `Create` 开始就完整覆盖名字空间的。

#### 2. server 侧已经存在 backend DB metadata 层

当前 server `HEAD/LIST` 不是直接面向对象存储，而是经由 backend 查询 DB metadata：

- `pkg/server/server.go` 中 `handleStat(...)` 调 `b.StatNodeCtx(...)`；
- `pkg/backend/dat9.go` 中 `StatNodeCtx(...)` 最终调用 `store.Stat(...)` / `store.StatPathFallback(...)`；
- `pkg/datastore/store.go` 中 `Stat(...)` 与 `ListDir(...)` 查询 `file_nodes` 与 `files`。

也就是说，server 已经具备“系统 metadata 服务”的雏形。

#### 3. 但新建文件通常不是 metadata-first

当前 brand-new file 的创建路径主要有两类：

- 直接 `PUT` 小文件：`pkg/backend/dat9.go` 中 `createAndWriteCtx(...)` 在一个事务内同时插入 `files` 和 `file_nodes`；
- multipart / patch / append：`pkg/backend/upload.go` 中 create 分支在 finalize 阶段才确认 `files` 并插入 `file_nodes`。

这意味着当前 server metadata 只在“真正写入或 finalize 之后”才知道该 path 存在，而不是在 `Create` 成功时知道。

#### 4. 当前问题与 issue 265 的关系

这个结构直接解释了 `juicefs bench` 在 drive9 FUSE 上复现的新建文件可见性 race：

- FUSE `Create()` 已成功，当前进程持有 open handle；
- `pendingIndex` 尚未登记该 path；
- backend DB metadata 也尚未登记该 path；
- 新的 `Lookup/Stat` 只能退回到 server `HEAD/LIST`；
- server metadata 不认识这个 path，最终暴露错误。

这不是对象存储最终一致性问题，而是系统内部 authority gap：在一段时间内，没有任何一个组件对“该路径是否存在”拥有完整权威。

### Problem Statement

当前架构使 `Create -> first flush/commit` 窗口内的 brand-new file 同时具备以下不良性质：

- 对当前 mount，本地 handle 知道它存在；
- 对新的 lookup/stat/readdir/open 请求，只有在恰好命中本地 overlay 时才可见；
- 对 server metadata 与其他 mount，它仍不可见；
- 对崩溃恢复与跨 mount 协调，这段状态没有统一 owner；
- 对未来的 API 与多客户端语义，文件存在性和内容确认时机也没有统一 contract。

结果是，`drive9` 目前更像“local-create-first, metadata-later”，而不是“metadata-first, data-later”。这对单机短链路也许还能工作，但对 FUSE、bench、多进程访问以及未来多挂载一致性都不稳定。

### Constraints and Decision Drivers

- 当前 `drive9` 已经有 `file_nodes/files/uploads` 表与 `status/revision` 基础，不应推翻现有 backend 存储模型；
- 当前 FUSE 已经依赖 `PendingIndex`、`shadowStore`、`writeBack` 提供性能和未提交内容视图，不应为修正 authority gap 而一次性移除这些层；
- 当前 overwrite/patch/append 已经围绕 `expectedRevision` 与 CAS 语义建立了基本正确性边界，不应在本 proposal 中重写为另一套并发控制模型；
- 当前 server API 已经承担 `HEAD/LIST/PUT/mkdir/rename/delete/uploads` 等角色，proposal 需要优先复用现有 server 边界，而不是引入 FUSE 直连 DB；
- 该修复会直接影响名字空间可见性、跨客户端读写体验与崩溃恢复，因此必须明确产品语义与生命周期，而不能只做局部补丁。

## Terminology Baseline

| 术语 | 本 proposal 中的定义 | 当前代码中的对应实现 |
| --- | --- | --- |
| `server metadata` | 由 server/backend/datastore 管理的权威名字空间与文件元数据 | `file_nodes`、`files`、`StatNodeCtx`、`ListDir` |
| `本地 overlay` | 仅对当前 FUSE mount 生效的未提交视图覆盖 | `PendingIndex`、`shadowStore`、dirty handle state |
| `名字空间权威` | 回答“路径是否存在、类型是什么、父子关系是什么”的最终来源 | proposal 目标：server metadata |
| `内容确认` | 文件内容已经 durable 并切换为对全局可见的确认 revision | 当前 `files.status='CONFIRMED'` |
| `brand-new create` | 从不存在 path 到新文件 path 的首次创建 | 当前 `Create()` + 直接 PUT / finalize create branch |
| `overwrite` | 对已有 path 的 revision-gated 内容替换 | 当前 `WriteCtxIfRevision(...)`、`UpdateFileContent...` |

## Architecture Overview

### 当前结构

```text
FUSE Create()
  -> local inode + local handle + dirty buffer
  -> (later) pendingIndex/shadow/writeBack stage
  -> (later) server write/finalize
  -> (later) DB file_nodes/files visible

Lookup/GetAttr/Readdir
  -> pendingIndex / writeBack ?
  -> server HEAD/LIST
  -> DB metadata
```

当前缺陷在于：`Create()` 返回成功时，名字空间既不完全归本地 overlay 管，也不完全归 server metadata 管。

### 目标结构

```text
FUSE Create()
  -> server metadata-only create (authoritative namespace)
  -> local handle + local dirty/shadow overlay
  -> write / flush / release / finalize content
  -> server confirms file content revision

Lookup/GetAttr/Readdir/Open
  -> server metadata as namespace truth
  -> local overlay only amends uncommitted size/content for current mount
```

目标不是把所有状态都推到 server，而是明确层次：

- namespace truth 在 server；
- current mount 的未提交内容视图在 client；
- revision-confirmed content 在 backend。

## Goals

1. 让 `Create()` 成功后，同一路径的 authoritative existence 不再依赖 first flush/commit 才成立。
2. 消除 `Create -> first flush/commit` 窗口内的 authority gap，避免因 metadata 缺席而暴露 `ENOENT/EIO`。
3. 保留 `PendingIndex`、`shadowStore`、`writeBack` 的性能价值，但将其职责收缩为本地 overlay，而不是系统权威。
4. 为多 mount、多进程、崩溃恢复和后续一致性语义建立统一的 server-side metadata 基础。
5. 保持 overwrite/patch/append 的 revision-gated 正确性边界，不在本 proposal 中重写现有数据面协议。

## Non-Goals

- 不在本 proposal 中移除 `PendingIndex`、`shadowStore` 或 `writeBack`。
- 不在本 proposal 中设计新的对象分块格式或替换当前 multipart / patch / append 上传协议。
- 不在本 proposal 中把 FUSE 客户端改成直连 datastore/DB。
- 不在本 proposal 中承诺 exactly-once 写入或全局线性化读写语义。
- 不在本 proposal 中设计 brand-new 空文件 follow-up processing 的其他变体；当前默认 contract 已固定为“可见但默认不触发相关 follow-up 流程”，任何偏离该默认值的行为都需要额外 proposal。

## FUSE Stable Contracts

本节记录已经在当前 proposal 中拍板的稳定语义。它的目的不是重复设计细节，而是作为后续实现、review、测试和文档更新的硬边界。除非开启新的 proposal 修订，这些 contract 不应在实现阶段被弱化、绕过或重新解释。

以下内容描述的是目标 contract，而不是当前实现现状。当前代码路径里仍然保留 local-overlay-first、`expectedRevision = 0` 表示 brand-new create、以及 `readdir` merge 本地 pending 项等旧行为；这些正是后续实现阶段需要迁移和收敛掉的对象。

### 1. Namespace authority

- `server metadata` 是唯一的名字空间权威来源。
- `Lookup/GetAttr/Readdir/Open` 对路径是否存在与基础 metadata 的判断，最终必须以 server metadata 为准。
- `PendingIndex`、`shadowStore`、本地 dirty state 不得再承担 system-wide path existence truth 的职责。

### 2. Mutation commit boundary

- `Create/Mkdir/Rename/Unlink` 成功返回前，server metadata 必须已经提交成功。
- 成功返回之后，不允许再因为 authority gap 把同一路径看成不存在。
- 当前 mount 的立即可见性来自 metadata commit 与缓存边界控制，而不是本地 namespace patch。

### 3. Brand-new create semantics

- `create` 成功后创建的是一个正常的 0 字节文件，而不是显式 `PENDING_CREATE` 对象。
- 在 authoritative metadata view 中，`create` 成功返回时，该文件已经存在于 server metadata 中。
- metadata-only create 成功提交后，server 必须发布显式 `create` change event；该事件表示 namespace create 已提交、path 已进入 authoritative metadata view、brand-new file entity 已存在且 `revision = 1`。
- 对其他 mount 的实际观察结果，仍然受各自 dentry cache、negative entry cache、directory listing cache 与 opened-directory-handle snapshot 边界约束。
- 除本地 cache boundary 外，其他 mount 何时更快越过这些边界，还取决于跨 mount invalidation propagation；当前系统里 SSE change event 是主要的失效传播机制，但它不是 metadata truth 成立的前提。
- 一旦其他 mount 越过这些缓存边界重新观察该路径，看到的结果应当是：`path exists`、`size = 0`、`read => EOF/empty file`。
- 如果创建者在第一次内容提交前崩溃，系统保留该正常空文件，不进入特殊 orphan create cleanup 流程。

### 4. Revision contract

- brand-new create 本身即分配 `revision = 1`。
- 第一次成功写入内容后，revision 推进为 `2`。
- 后续 patch / append / direct write / finalize 都是在一个已存在的 file entity 上推进 revision，而不是负责“首次让路径出现在 namespace 中”。
- overwrite CAS 必须把 brand-new create 后的空文件视为一个正常存在、`revision = 1` 的文件。
- `revision = 0` 只保留给“path must not already exist”这一类 create precondition / CAS 语义，不作为任何已成功 create 的 file entity 的外部 revision 值。
- 不接受把 `revision = 0` 用作 create 成功后的值；否则它会同时表示“文件尚不存在前的前置条件”和“文件已经存在但尚未第一次内容提交”两种状态，导致 create、overwrite、writeback recovery 与 upload finalize 之间的状态机边界变得含混。

### 5. API evolution boundary

- FUSE 客户端强制使用 `metadata-only create` 作为 create 起点。
- 旧 PUT/create 路径在过渡期继续兼容。
- 但服务端内部必须逐步收敛到统一的 create state machine，不接受长期保留双语义 create。
- 是否未来公开废弃旧入口，不在当前 proposal 中决定；但内部语义统一是当前 proposal 的既定方向。
- `create` change event 是 metadata-only create 的专用事件语义，不复用 `write`、`upload_complete`、`mkdir` 等现有 op。

### 6. Readdir semantics

- `readdir` 的目录项集合完全以 server metadata 为基础。
- 本地 overlay 不改写目录项集合，也不 patch 已打开目录句柄的目录视图。
- 已打开目录句柄采用 per-handle snapshot 语义，因此后续目录变化后，同一目录句柄继续 `readdir` 时仍可能看到旧结果。
- 这种旧结果是缓存边界问题，不是 metadata 提交失败问题。

### 7. Readdir cache boundary

- `readdir` 新鲜度边界必须显式区分：
  - 句柄级快照边界
  - 跨句柄目录缓存边界
- `readdir` 相关 TTL 只控制目录枚举新鲜度，不代表 metadata commit 时机。
- 即使跨句柄 `readdir` 缓存 TTL 为 `0`，也不表示已打开目录句柄自动实时刷新。
- SSE / change event 只能帮助其他 mount 更快失效 dentry、negative entry、跨句柄目录缓存等边界；它不改变已打开目录句柄的 per-handle snapshot contract。

### 8. Default boundary for follow-up processing

- Phase A 默认收敛为：brand-new create 产生的正常 0 字节文件在可见性上是正常 file entity，但默认不触发 semantic task、indexing / search materialization、extract pipeline、storage-bytes 增量、media-file-count 增量等 follow-up 流程。
- 这一默认方向与当前后端实现倾向一致：0-byte create 可记录 file_create / file_meta，但默认不增加 quota bytes，不增加 media count，也不注册 semantic task。
- 如果未来某个下游流程需要把这类 brand-new 空文件视为应立即处理的实体，必须通过单独决策或额外 proposal 显式引入，而不是在实现阶段隐式放宽。

## Architecture Blueprint

### 1. 当前 architecture 与现有 contract

#### 1.1 server 已经是 metadata 查询入口

当前所有 `HEAD/LIST` 都经过 server/backend/datastore，说明 server 已具备系统 metadata owner 的自然位置。

#### 1.2 FUSE 当前的 overlay 已深度嵌入写路径

`PendingIndex`、`shadowStore` 和 dirty handle state 目前不仅用于性能，也承担了部分正确性职能，例如：

- `Lookup` 先查 `pendingIndex`；
- `GetAttr` 会用 dirty handle size 覆盖远端 size；
- `Read` 可能直接命中 shadow data。

proposal 不是删除这些逻辑，而是将其重新定位为“本地未提交视图”，不再承担 namespace ownership。

#### 1.3 当前 DB status 模型已经可以承载“正常空文件 + 后续 revision 推进”

当前 `files.status` 至少已有 `PENDING` 与 `CONFIRMED` 基础，upload finalize 流程已经围绕这套状态转移工作。proposal 不再要求为新建文件引入显式 `PENDING_CREATE` 生命周期，而是要求 metadata-create 直接产出一个 `CONFIRMED` 的 0 字节文件实体，并将后续写入建模为对该文件的普通 revision 推进。

### 2. 提议的 layering 与 ownership

提议将系统分成三层，并明确 owner：

- **Layer A: server metadata authority**
  - owner: `pkg/server` + `pkg/backend` + `pkg/datastore`
  - 负责：路径存在性、目录树、文件实体状态、当前确认 revision、基础时间戳
- **Layer B: client local overlay**
  - owner: `pkg/fuse`
  - 负责：当前 mount 的 dirty size、未提交 shadow data、flush 前本地可读性
- **Layer C: durable content backend**
  - owner: backend + object store / DB blob
  - 负责：确认后的文件内容、revision 递增、overwrite CAS 边界

新的 ownership 原则：

- `Lookup/Readdir/Open` 的“存在性”只信 Layer A；
- `GetAttr/Read` 的“当前 mount 未提交视图”可由 Layer B 覆盖；
- `revision-confirmed bytes` 只由 Layer C 提供。

### 3. 新的生命周期

#### 3.1 Metadata-only create

新增一条显式 metadata-only create 路径。推荐在 server 层提供专用 API，而不是继续复用“PUT body 为空”这类隐式约定。

本 proposal 已收敛的 API 演进方向是：

- FUSE 客户端必须使用 `metadata-only create`；
- 旧 PUT/create 路径在过渡期继续兼容；
- 但服务端内部不接受长期保留“FUSE metadata-first、旧客户端 write-first”的双语义 create；
- 旧路径需要逐步收敛到与 `metadata-only create` 一致的 server-side create state machine。

推荐 contract：

- `POST /v1/fs/<path>?create`
- 作用：仅创建 metadata，不写入文件内容
- server 行为：
  - `EnsureParentDirsTx(...)`
  - 插入 `files` 行，作为正常 0 字节文件创建并立即确认
  - 插入 `file_nodes` 行
  - 返回 file identity、initial revision/status、mtime

实现约束：

- Phase B 的目标不是直接暴露当前 backend `CreateCtx()`；
- 当前 `CreateCtx()` 虽然已接近 metadata-only create，但它既未接入 server/client/FUSE 主链路，也不是单事务 state machine；
- 最终实现必须把 `files`、父目录补齐、`file_nodes` 的写入放进同一个 metadata transaction 中，避免在中间失败时留下部分 metadata；
- 最终实现不得把 metadata-create 语义绑定为“顺带创建一个 S3 侧 0 字节对象”；对象存储写入应继续属于后续内容确认路径，而不是 namespace commit 的前置条件。
- metadata-only create 落地时，必须同步引入显式 `create` change event；不接受复用 `write`、`upload_complete` 或其他现有 op 来承载 namespace-create 语义。
- `create` event 只表示 namespace create committed：path 已进入 authoritative metadata view、brand-new file 已存在、`revision = 1`；它不表示第一次内容确认已经发生，因此不能与后续 `write` / `upload_complete` 的内容层事件混用。
- FUSE SSE 侧对 `create` event 的最低失效要求是：覆盖 parent dentry、target path 的 negative entry / dentry、以及跨句柄目录缓存失效；但不承诺已打开目录句柄自动刷新。

FUSE `Create()` 成功条件变为：

1. server metadata create 成功；
2. 本地 handle / dirty buffer 初始化成功。

只有两者都成功，FUSE 才向 kernel 返回 `OK`。

#### 3.2 Brand-new create 的内容确认

brand-new create 之后的内容写入继续允许走：

- direct PUT
- writeback flush
- multipart finalize
- patch/append 派生流程

但这些路径不再负责“首次让 path 出现在 namespace 中”，而只负责：

- 在同一个已存在的 file entity 上写入或确认内容；
- 将 revision 从 `1` 推进到后续 revision；
- 补齐或更新 size / checksum / storage_ref / confirmed_at 等内容元数据。
- 相关 change event 也必须与 metadata-only create 分层：`create` 负责 namespace create committed，`write` / `upload_complete` 负责内容层提交或 finalize，不得把两者折叠成同一种事件语义。

#### 3.3 Overwrite 与 brand-new create 分离

proposal 明确区分：

- `brand-new create`
  - 先建 metadata，再补内容
- `overwrite`
  - 旧 path 已存在于 metadata
  - 内容通过 expected revision / CAS 协调更新

这能避免当前 create 和 overwrite 混用一条数据面路径导致的语义混乱。

### 4. FUSE 行为调整

#### 4.1 Lookup

`Lookup(...)` 的存在性判定改为：

1. 若本地 overlay 命中当前 mount 的更强未提交信息，可作为 attr 覆盖来源；
2. 但 path 是否存在，最终以 server metadata 为准；
3. brand-new create 的当前 mount 可以依赖 metadata-create 返回值立即命中本地 inode cache，而不是等待一次远端 `HEAD` round trip。

这意味着 `pendingIndex` 不再是 path existence 的唯一兜底来源。

#### 4.2 GetAttr

`GetAttr(...)` 先拿 server metadata 的基础 attr，再叠加本地 overlay：

- 当前 mount 的 dirty size / shadow size / pending mtime 可以覆盖；
- 非当前 mount 不需要看到这些本地覆盖；
- 对 brand-new file，server metadata 从 create 成功起就必须返回一个正常空文件 attr，而不是退为 not found。

#### 4.3 Readdir

目录 listing 的基础来源改为 server metadata，本地 overlay 不改写目录项集合。

本 proposal 已收敛的 `readdir` 语义是：

- `Create/Mkdir/Rename/Unlink` 成功返回前，server metadata 必须已提交；
- 但已经打开的目录句柄后续继续 `readdir` 时，仍可能看到旧目录快照；
- 这种“旧结果”来自目录句柄级缓存，而不是因为 server metadata 尚未提交；
- 当前 mount 不通过本地 overlay patch 已打开目录句柄的 `readdir` 结果。

推荐实现模型：

- `opendir()` 建立 `DirHandle`；
- 第一次 `readdir()` 时，从 server metadata 拉取目录快照并绑定到该 handle；
- 后续同一 handle 的 `readdir()` 默认只回放该快照；
- `releasedir()` 是最明确的句柄级失效边界；
- 可选再引入显式的 handle 级 `readdir` TTL，但它必须独立于 entry/attr cache TTL 单独定义。

因此，`readdir` 的缓存边界需要分成两层：

- **句柄级快照边界**：控制同一个已打开目录句柄何时放弃旧快照；
- **跨句柄目录缓存边界**：控制不同 `opendir` 之间是否可复用同一份目录 listing。

参数语义必须明确：

- `readdir` 相关 TTL 仅控制目录枚举新鲜度，不代表 metadata commit 时机；
- 即使 `readdir` 跨句柄缓存 TTL 为 `0`，也不表示“已经打开的目录句柄会自动实时刷新”；
- 操作成功返回只表示 server metadata 已提交，不表示所有旧目录句柄都立刻反映新名字空间。

#### 4.4 PendingIndex 的职责重定义

`PendingIndex` 保留，但职责收缩为：

- 本地未提交 size / mtime / base revision 索引；
- shadow readiness / staged generation 追踪；
- 当前 mount 的 read-after-write overlay。

不再把它定义为系统 authoritative metadata index。当前注释与职责将需要随设计落地一起修正。

### 5. Server-side metadata state model

本 proposal 不再为新建文件引入显式 `PENDING_CREATE` 状态。brand-new create 的最小模型是：

- create metadata 后：`file_nodes` 已存在，`files` 行也已存在，并表现为一个正常的 `CONFIRMED` 0 字节文件
- create 成功后：在 authoritative metadata view 中，该 path 已存在；对其他 mount 的实际观察仍受各自 cache boundary 约束，但一旦越过这些边界重新观察，结果应为 `path exists`、`size = 0`、`read => EOF/empty file`
- create 本身即分配 `revision = 1`
- first successful content write/flush/finalize 后：文件进入非空或更新后的确认状态，revision 推进到 `2`

该模型要求：

- “正常空文件”与“创建后尚未写入任何内容”的外部语义保持一致；
- system 不尝试从 metadata 上区分“用户本来就想创建空文件”和“用户原本准备写入但在第一次内容提交前崩溃”；
- overwrite CAS 必须把 brand-new create 后的空文件视为 `revision = 1` 的正常现有文件；
- patch/append/direct write/finalize 路径在第一次确认内容时统一从 `revision = 1` 推进到 `revision = 2`。

### 6. Failure handling / recovery / degraded path

#### 6.1 Create 之后 client 崩溃

server metadata 成功、client 尚未提交任何内容就崩溃时，系统会留下一个正常的 0 字节空文件。这一结果是当前 proposal 的有意语义，而不是需要后台回收的“未完成创建”。

这意味着：

- server 不再需要通过 `owner_session_id + lease_expires_at` 之类机制判断一个 brand-new create 是否仍然处于“待确认创建”；
- 崩溃后的空文件保留是正常 contract，而不是异常恢复路径；
- 如果应用不接受最终路径短暂或永久出现 0 字节文件，应通过临时文件 + `rename` 模式规避。

#### 6.2 Flush/Finalize 失败

若内容提交失败：

- path 仍可能在 namespace 中存在；
- 对 brand-new create，若第一次内容提交从未成功，则该 path 继续表现为一个正常的 `revision = 1` 空文件；
- 对 overwrite，仍保持“旧 confirmed revision 可见，直到新 revision 成功确认”的既有原则；
- server 不为 brand-new create 额外引入 pending lifecycle 或 orphan recovery。

#### 6.3 Backward compatibility

server 需要在过渡期同时支持：

- 旧客户端：仍通过现有 PUT / uploads 路径创建文件；
- 新 FUSE 客户端：先 metadata-create，再走后续内容提交流程。

这一兼容策略的目标不是长期保留双语义 create，而是为迁移提供缓冲：

- 对外 API 层面，短期允许多入口；
- 对内 state machine，必须逐步统一为“metadata create first, content later”；
- 本 proposal 不要求立即公开废弃旧入口，但明确不接受长期维持两套 create 语义。

## Compatibility and Invariants

以下不变量在整个 rollout 中必须保持：

1. system namespace truth 只能有一个 owner，proposal 目标是 server metadata。
2. 当前 mount 的本地 overlay 可以覆盖 size/content 视图，但不能否定 server metadata 已确认存在的 path。
3. overwrite / patch / append 的 revision-gated 正确性边界必须保留。
4. 目录与文件的 namespace 可见性模型必须统一，不能继续出现“目录 metadata-first、文件 write-first”的分裂语义。
5. `Create()` 对 kernel 返回成功后，后续 `Lookup/GetAttr/Readdir/Open` 不得再因为 metadata authority gap 把同一路径看成不存在。
6. 对 brand-new create，authoritative metadata view 必须在 create 成功返回时立即成立；其他 mount 越过各自缓存边界后，应观察到一个正常的 `size = 0` 空文件。
7. brand-new create 本身即分配 `revision = 1`；第一次成功写入内容后推进为 `revision = 2`，这一语义必须在 create 与 overwrite 路径之间保持清晰分界。
8. `readdir` 的目录项集合完全以 server metadata 为基础，不由本地 overlay 增量改写。
9. 已打开目录句柄允许继续返回旧目录快照；该陈旧性必须被视为缓存边界问题，而不是 metadata 提交问题。

## Alternatives Considered

### 方案 A：仅修补 PendingIndex，在 Create 时立即插入本地 pending dentry

优点：

- 改动最小；
- 能较快修复单 mount 下的 `Create -> immediate Lookup` race。

缺点：

- 只解决当前 mount 的局部问题；
- 无法成为多 mount / server restart / recovery 的系统权威；
- 继续保留 server metadata 与 client local overlay 的双权威结构；
- 即使补齐当前 mount 的立即可见性，也无法给其他客户端提供统一的“正常空文件即已提交 create”语义。

结论：可作为短期止血补丁，但不适合作为最终架构方向。

### 方案 B：保持当前结构，只把 flush/commit 提前

优点：

- 避免新增 metadata-only create API。

缺点：

- `Create` 延迟直接绑定网络和上传路径；
- 无法稳定支持大文件、writeback、append/patch；
- 仍然没有把 namespace 与 content 分层。

结论：不推荐。

### 方案 C：server metadata 成为系统权威

优点：

- 从根上消除 authority gap；
- 与现有 server/datastore 结构兼容；
- 更适合后续多客户端一致性与恢复能力。

缺点：

- 需要显式调整 create/revision contract，并处理新旧 create 路径的兼容与迁移；
- rollout 和兼容复杂度更高。

结论：推荐作为长期架构方向。

## Incremental Plan

### Phase A: Contract hardening

- 将已收敛的稳定 contract 进一步固化到实现说明中：`create` 即正常空文件、authoritative metadata view 在成功返回时立即成立、其他 mount 的实际观察受 dentry/negative/readdir/opened-dir-handle cache boundary 约束；
- 明确 metadata-only create API contract，以及“metadata truth”与“practical observation”是两层不同语义边界；
- 明确 brand-new create 与 overwrite 的 revision/state model 在实现中的边界，并把 `revision = 1 -> 2` 语义固定到 create-first 生命周期中。
- 将 create/revision 语义的迁移明确为全链路 state machine migration，而不是局部 helper 替换；需要同步收敛 FUSE、client、backend、upload finalize 等路径中对“brand-new create 使用 `expectedRevision = 0`、首次成功后得到 `revision = 1`”的旧假设。
- 将“正常空文件可见，但默认不触发 follow-up processing”固定为默认 contract，并明确这是在固化当前后端已表现出的默认倾向，而不是额外引入一条与现状相反的新语义。

### Phase B: Server metadata state machine

- 让 server 能在无内容写入的情况下直接创建权威 dentry/file entity；
- 将 create 结果持久化为正常空文件而非特殊 pending state；
- 在 finalize / direct write / patch / append 路径中完成从 `revision = 1` 到后续 revision 的推进；
- 保证 `Create/Mkdir/Rename/Unlink` 成功返回前，server metadata commit 已经完成，不再留下 authority gap。
- Phase B 不以“暴露当前 backend `CreateCtx()`”为完成标准；需要新增或收敛出一个专用的 transactional metadata-create state machine，并将其真正接入 server/client/FUSE 链路。
- Phase B 必须按已拍板的 `metadata-only create => create event` taxonomy 实现；server、client、FUSE 不允许各自猜测或临时复用 `write` / `upload_complete` 语义。

### Phase C: FUSE authority migration

- 将 `Create()` 改为先 metadata-create；
- 将 `Lookup/GetAttr/Readdir/Open` 改为以 server metadata 为 namespace truth，其中 `GetAttr` 先取 server metadata 基础 attr，再叠加当前 mount 的本地未提交 overlay；
- 将 `PendingIndex` 角色收缩为当前 mount 的本地 overlay，不再承担 system-wide path existence truth；
- 将 `readdir` 改造成 per-handle snapshot 语义，并显式区分句柄级与跨句柄级缓存边界；
- 明确当前 mount 的立即可见性来自 metadata commit 与本地 overlay 的职责分层，而不是 namespace patch；其他 mount 的实际观察继续受各自 cache boundary 约束。
- 当前 `readdir` 已有 per-handle snapshot 雏形；Phase C 的关键迁移动作不是重新发明 handle lifecycle，而是移除 `mergePendingDirEntries(...)` 一类 local overlay patch，使目录项集合完全回归 server metadata。

### Phase D: Recovery and compatibility

- 保证旧客户端仍可通过旧 API 创建文件；
- 加入 observability、metrics、日志和回滚开关；
- 在 rollout 中显式验证并暴露 cache-boundary 相关行为，避免把 stale observation 误判为 metadata commit failure；
- 固化 brand-new 空文件保留、旧目录句柄可继续返回旧快照等恢复与缓存语义，确保实现、测试和运维口径一致。

## Validation Strategy

- 单 mount FUSE 测试：`Create -> immediate Lookup/GetAttr/Open/Readdir` 不再出现 `ENOENT/EIO`。
- `juicefs bench` 回归：small-file phase 不再因新建文件 visibility race 失败。
- 多 mount 集成测试：一个 mount create 后，另一个 mount 在 fresh observation 或越过相关缓存边界后能看到该 path，且 `stat` 为 `size = 0`，`read` 返回空文件。
- 多 mount invalidation 测试：metadata-only create 成功后，应产生明确的 change event / SSE 失效语义，使其他 mount 可在不等待纯 TTL 自然过期的情况下失效 parent dentry、negative entry 与跨句柄目录缓存；同时验证已打开目录句柄仍允许保留旧快照。
- event taxonomy 测试：metadata-only create 成功后必须发布显式 `create` event，而不是 `write` / `upload_complete`；第一次内容提交后应继续发布相应内容层事件，并与 `create` 的 namespace 语义保持分层。
- 状态机测试：brand-new create 在 metadata-create 成功后直接成为 `revision = 1` 的正常空文件，第一次内容成功提交后推进到 `revision = 2`。
- 崩溃语义测试：metadata-create 成功但未有任何内容提交时，客户端崩溃后该 path 保留为正常空文件，不进入特殊 orphan cleanup 流程。
- 兼容测试：旧 PUT / uploads 客户端与新 FUSE 客户端可同时对同一 server 工作。
- 覆盖写回归：overwrite、patch、append、rename、unlink 继续满足既有 revision-gated 正确性。
- 目录句柄测试：先 `opendir`，再由同 mount 或其他 mount 执行 `create/mkdir/rename/unlink`，旧目录句柄继续 `readdir` 时允许看到旧结果；关闭重开目录后按缓存边界获取新结果。
- 参数边界测试：验证 `readdir` 句柄级与跨句柄级缓存配置只影响目录枚举新鲜度，不影响 metadata commit 成功语义。

## Risks and Mitigations

1. **create 与首次内容提交分阶段后，用户可能长期看到 0 字节空文件**  
   缓解：将该行为定义为正常 contract，并建议需要“内容就绪后再显名”的应用使用临时文件 + `rename` 模式。

2. **Create 延迟增加一次 server RPC**  
   缓解：保持 metadata-only create 轻量，只做 transaction 内 metadata 变更，不绑定实际内容上传。

3. **旧 API 与新 API 并存导致双路径维护**  
   缓解：明确 FUSE 强制走新 API；旧客户端仅在过渡期兼容，服务端内部逐步收敛到统一 state machine，不长期保留双语义 create。

4. **多客户端对 brand-new create 的体验需要与 overwrite 分开理解**  
   缓解：当前 proposal 已收敛为“authoritative metadata 立即成立；其他 mount 越过缓存边界后观察到 size=0 / 空文件；revision=1 -> 2”，后续实现必须以集成测试固化该 contract。

5. **用户可能误把旧目录句柄看到旧结果理解成 metadata 未提交**  
   缓解：明确文档与参数语义，区分 metadata commit boundary 与 `readdir` cache boundary，并通过目录句柄测试固化该 contract。
